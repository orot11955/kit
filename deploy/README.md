# kit 배포 구성

공개 요청은 다음 경로로 전달된다.

```text
Cloudflare DNS-only
  -> edge (공개 80/443, TLS 종료)
  -> apps-prod:18080 (Docker Nginx 정적 origin)
  -> /srv/data/apps/kit/data
```

Gitea가 없어도 같은 forced-command SSH 경로로 수동 배포할 수 있다. 자동 workflow와
수동 배포의 차이는 archive를 누가 만드는지뿐이며, 서버의 검증·원자적 symlink 전환·보존
정책은 같다. 실제 수동 빌드와 업로드는 [MANUAL_DEPLOY.md](./MANUAL_DEPLOY.md)를 따른다.

검증 책임은 두 단계로 분리한다.

1. apps-prod의 `kit-activate`: root 관리 설정에 지정된 origin HTTP 응답을 배포 파일과 byte
   단위로 비교한다. 이 단계가 실패할 때만 활성 symlink를 자동 복원한다.
2. 수동 배포자 또는 Gitea workflow: activation 성공 후 public HTTPS 전체 경로를
   검증한다. public 실패는 배포 작업을 실패로 끝낼 뿐 자동 rollback을 호출하지 않는다.

현재 edge→apps-prod origin 구간은 신뢰된 사설망의 평문 HTTP를 전제로 한다. 이 구간에서
on-path 공격이 가능한 VLAN이거나 executable 배포 경계에 내부망 신뢰를 둘 수 없다면 운영
공개 전에 origin TLS+mTLS 또는 WireGuard/IPsec 같은 인증된 암호화 구간을 먼저 구성한다.
공개 HTTPS 인증서만으로는 edge 뒤 내부 HTTP 응답의 무결성을 보장하지 않는다.

## 1. apps-prod 준비

apps-prod에는 Docker Engine과 Compose plugin, 배포 script 실행을 위한 `bash`, `curl` 8.4.0
이상,
`tar`, `gzip`, GNU `coreutils`, `findutils`, `util-linux`의 `flock`, `iproute2`, OpenSSH
server가 필요하다.

### 전용 계정과 directory

`kit-deploy`는 password와 sudo 권한이 없는 배포 전용 계정이다. SSH forced command가
실행되어야 하므로 login shell은 `/bin/bash`로 두되, key 제한으로 임의 command와 PTY를
차단한다.

```sh
sudo useradd --system --create-home --home-dir /home/kit-deploy --shell /bin/bash kit-deploy
sudo passwd -l kit-deploy

sudo install -d -o root -g root -m 0755 /srv/data/apps/kit
sudo install -d -o kit-deploy -g kit-deploy -m 0755 \
  /srv/data/apps/kit/data \
  /srv/data/apps/kit/data/incoming \
  /srv/data/apps/kit/data/sites \
  /srv/data/apps/kit/data/releases
sudo install -o kit-deploy -g kit-deploy -m 0600 /dev/null \
  /srv/data/apps/kit/data/.deploy.lock
sudo install -o kit-deploy -g kit-deploy -m 0600 /dev/null \
  /srv/data/apps/kit/data/.upload.lock
```

이미 계정이 있으면 `useradd`를 다시 실행하지 말고 UID, home, shell과 소유권을 확인한다.
앱 설정은 root가 관리하고 `data`만 `kit-deploy`가 쓴다.

```sh
sudo install -d -o kit-deploy -g kit-deploy -m 0700 /home/kit-deploy/.ssh
sudo -u kit-deploy touch /home/kit-deploy/.ssh/authorized_keys
sudo chmod 0600 /home/kit-deploy/.ssh/authorized_keys
```

key를 추가할 때 기존 `authorized_keys`를 덮어쓰지 않는다.

### origin 앱 설치

저장소의 `deploy/apps-prod` 내용을 `/srv/data/apps/kit`에 복사한다. `data`는
배포 산출물 경로이므로 복사 과정에서 삭제하거나 덮어쓰지 않는다.

```sh
sudo install -o root -g root -m 0644 deploy/apps-prod/compose.yml \
  /srv/data/apps/kit/compose.yml
sudo install -o root -g root -m 0644 deploy/apps-prod/nginx.conf \
  /srv/data/apps/kit/nginx.conf
sudo install -o root -g root -m 0600 deploy/apps-prod/.env.example \
  /srv/data/apps/kit/.env
sudo install -o root -g root -m 0755 deploy/apps-prod/deploy.sh \
  /srv/data/apps/kit/deploy.sh

cd /srv/data/apps/kit
sudo editor .env
sudo ./deploy.sh config
sudo ./deploy.sh up
sudo ./deploy.sh status
curl -fsS -H 'Host: kit.2juho.com' http://APPS_PROD_INTERNAL_IP:18080/healthz
```

`.env`의 `KIT_ORIGIN_BIND`에는 `0.0.0.0`이나 공인 IP가 아니라 edge에서 접근 가능한
apps-prod의 사설 IP를 반드시 지정한다. 값이 없으면 Compose 설정 검증부터 실패한다.
`KIT_ORIGIN_PORT` 기본값은 `18080`이다. 별도 `compose.production.yml` overlay는 사용하지
않는다. 운영 설정이 하나뿐이므로 단일 compose 파일이 실제 설정의 기준이다.
port를 바꾸면 edge의 `upstream kit_origin` port도 같은 값으로 바꾼다.
Nginx image는 검증한 `1.30.4-alpine` tag와 manifest digest로 고정되어 있다. image를
갱신할 때는 새 digest를 확인하고 `nginx -t`와 healthcheck를 통과시킨 뒤 명시적으로
Compose 파일을 변경한다.

### 서버 고정 배포 script

wrapper와 activator entrypoint/core는 repository workflow가 갱신할 수 없도록 root 소유로
설치한다. 내용을 관리자가 검토한 후에만 교체한다. `/usr/local/libexec/kit-activate`는
운영 entrypoint이며 실제 배포 core는 `/usr/local/libexec/kit-activate-core`에 고정한다.
entrypoint는 core의 실패를 그대로 전달하고, historical site 성공 status `1`만 exact
postcondition이 확인될 때 `0`으로 정규화한다.

```sh
sudo install -d -o root -g root -m 0755 /usr/local/libexec
sudo install -d -o root -g root -m 0755 /usr/local/sbin /etc/kit
sudo install -o root -g root -m 0644 deploy/config/deploy.env.example /etc/kit/deploy.env
sudo editor /etc/kit/deploy.env
sudo install -o root -g root -m 0755 deploy/activate.sh /usr/local/libexec/kit-activate-core
sudo install -o root -g root -m 0755 deploy/activate-entrypoint.sh /usr/local/libexec/kit-activate
sudo install -o root -g root -m 0755 deploy/ssh-wrapper.sh /usr/local/libexec/kit-ssh-wrapper
sudo install -o root -g root -m 0755 deploy/rollback.sh /usr/local/sbin/kit-rollback
sudo chown root:root /etc/kit/deploy.env
sudo chmod 0644 /etc/kit/deploy.env
```

`KIT_ORIGIN_BASE_URL`은 Compose가 bind한 apps-prod의 숫자형 RFC1918 IPv4 주소와 port를
명시해야 하며 기본값, hostname 또는 public URL로 대체되지 않는다.
`KIT_ORIGIN_HOST=kit.2juho.com`은 고정값이며 모든 origin 검증 요청의 `Host` header로
전달된다. 설정은 root 소유이고 group/other writable이면 activator가 거부한다. 값에는
shell quote나 공백을 넣지 않는다. origin 요청은 proxy 환경과 curl 사용자 설정을 사용하지
않고, 기대 파일 크기를 초과하는 응답을 중단한다.

### manual key와 Gitea Runner key 분리

수동 배포와 Gitea Runner는 같은 private key를 공유하지 않는다. 각 위치에서 별도
Ed25519 keypair를 만들고 public fingerprint를 기록한다.

```sh
# 수동 build host
umask 077
MANUAL_KEY_DIR=$(mktemp -d)
ssh-keygen -t ed25519 -f "$MANUAL_KEY_DIR/kit-manual-deploy-key" -C kit-manual-deploy
ssh-keygen -lf "$MANUAL_KEY_DIR/kit-manual-deploy-key.pub" -E sha256

# 보안 관리 host에서 Runner용 key를 별도로 생성한 뒤 Gitea secret으로 등록
GITEA_KEY_DIR=$(mktemp -d)
ssh-keygen -t ed25519 -f "$GITEA_KEY_DIR/kit-gitea-deploy-key" -C kit-gitea-deploy
ssh-keygen -lf "$GITEA_KEY_DIR/kit-gitea-deploy-key.pub" -E sha256
```

두 임시 directory는 checkout 밖에 생성된다. Runner private key는
`KIT_DEPLOY_SSH_KEY` secret 등록과 public key fingerprint 확인이 끝난 뒤 관리 host에서
삭제한다. manual private key는 수동 배포 기간에만 보관하고 Gitea secret에 등록하지
않는다.

두 public key를 각각 다른 `authorized_keys` line에 추가한다. `MANUAL_BUILD_IP`와
`GITEA_RUNNER_IP`도 서로의 실제 source IP로 제한한다.

```text
from="MANUAL_BUILD_IP",restrict,command="/usr/local/libexec/kit-ssh-wrapper manual" ssh-ed25519 AAAA... kit-manual-deploy
from="GITEA_RUNNER_IP",restrict,command="/usr/local/libexec/kit-ssh-wrapper gitea" ssh-ed25519 AAAA... kit-gitea-deploy
```

wrapper는 `upload-site <40 hex>`와 `upload-release <SemVer>`만 허용하고 표준 입력 archive를
100 MiB 및 300초로 제한하며 `.upload.lock`으로 동시 upload를 하나만 허용한다. identity는
system log의 `kit-deploy` tag에 남는다. 인자가 없는 기존 forced-command line은 `legacy`
identity로 계속 동작하지만, fingerprint와 사용 주체를 구분할 수 있도록 위 두 line으로
전환한다. 등록 후 실제 `.pub` 파일의 SHA256 fingerprint와 `authorized_keys` 대상 line의
fingerprint를 다시 비교한다. 계정에 sudo, Docker socket, 다른 `/srv/data/apps` 쓰기 권한을
주지 않는다.

```sh
sudo ssh-keygen -lf /home/kit-deploy/.ssh/authorized_keys -E sha256
```

출력된 각 comment와 SHA256 fingerprint가 manual/Gitea의 `.pub` 출력과 각각 일치해야
한다. `.deploy.lock`과 `.upload.lock`은 위 설치 단계처럼 `kit-deploy:kit-deploy`, mode
`0600`을 유지한다. rollback 요청은 root 전용 `kit-rollback`에서 검증하지만 activator
자체는 즉시 `kit-deploy` 권한으로 낮춰 실행한다. activator도 이 UID가 아니면 실행을
거부한다. 따라서 rollback과 SSH 배포가 같은 사용 권한과 lock inode를 사용하며, rollback은
`.upload.lock` 다음 `.deploy.lock` 순서로 획득해 진행 중 upload가 나중에 결과를 덮는 것을
막는다.

### Gitea Actions와 Runner 연결

kit repository에서 Actions를 활성화하고 홈 Gitea instance에 역할이 다른 Runner를
등록한다. PR을 검사하는 `kit-ci` Runner와 배포 secret을 사용하는 `kit-deploy` Runner는
서로 다른 OS account, work directory, cache, 등록 token과 network 권한을 사용해야 한다.
둘 다 Ubuntu 24.04 amd64 환경으로 운영하되 `runs-on` label이 workflow와 정확히 일치해야
한다. 회사 Gitea와 개인 Gitea 사이에도 token, Runner 등록 token, cache와 배포 SSH key를
공유하지 않는다.

- `kit-ci`: `pull_request`와 일반 branch 검사 전용. 배포 secret과 apps-prod 접근 권한이
  없으며 가능한 경우 job마다 폐기되는 ephemeral Runner로 운영한다.
- `kit-deploy`: 보호된 `main` push와 보호된 `v*` tag 배포 전용. PR workflow를 받지 않고
  apps-prod의 forced-command SSH endpoint에만 접근한다.

Runner label은 작업 routing 수단일 뿐 권한 경계가 아니다. 이 구성은 홈의 kit source
repository에 배포 책임자만 same-repository branch write 권한을 갖는 것을 전제로 한다.
외부 기여자는 fork PR만 사용하고, fork workflow 실행을 승인하기 전에
`.gitea/workflows/` 변경 여부를 먼저 검토한다. 회사 Gitea repository에는 아래 kit 배포
secret과 `kit-deploy` Runner를 등록하지 않는다.

다른 개발자에게 kit source repository의 branch write 권한을 줄 계획이라면 repository
secret을 등록하기 전에 배포 workflow와 SSH key를 PR을 받지 않는 별도 deployment
repository/Gitea scope로 분리해야 한다. 현재 Gitea의 Runner label만으로 feature branch가
`kit-deploy`를 요청하지 못하게 강제된다고 가정하지 않는다.

Gitea repository의 `main`은 direct push를 차단하고 PR 승인과 `ci` 상태 검사를 요구한다.
`v*` tag 생성 권한은 release 담당자 또는 전용 자동화 주체로 제한한다. 이 보호 규칙을
적용하기 전에는 docs/release workflow를 활성화하거나 배포 secret을 등록하지 않는다.

repository Actions 설정에는 다음 값을 등록한다.

```text
Variables
  KIT_DEPLOY_HOST       apps-prod의 Runner 접근용 사설 주소
  KIT_DEPLOY_PORT       기본 22 또는 실제 SSH port
  KIT_DEPLOY_USER       kit-deploy

Secrets
  KIT_DEPLOY_SSH_KEY    Gitea Runner 전용 private key
  KIT_DEPLOY_KNOWN_HOSTS  별도 경로로 fingerprint를 확인한 apps-prod host key line
```

workflow는 `.gitea/workflows/`에서 읽으며 `main` push는 docs, `vX.Y.Z` tag push는 release를
배포한다. `GITHUB_SHA`와 `GITHUB_REF_NAME`은 Gitea Actions가 제공하는 호환 환경 변수를
사용한다. checkout action은 Gitea 공식 mirror의 검증한 commit SHA로 고정되어 있으므로
갱신할 때 tag 이름만 바꾸지 말고 새 tag가 가리키는 commit과 release 내용을 함께 확인한다.

기존 Forgejo forced-command key를 전환하는 동안 wrapper는 `forgejo` identity를 레거시로
허용한다. 먼저 `gitea` identity의 새 public key를 추가하고 Gitea workflow의 SSH 연결을
검증한 뒤, 기존 Forgejo key line과 secret을 제거한다. manual key는 이 전환과 무관하게
별도로 유지한다. 제거 후 `authorized_keys` fingerprint를 다시 기록하고 system log에 더
이상 `identity=forgejo` 또는 의도하지 않은 `identity=legacy` 요청이 없는지 확인한다.

### 방화벽

apps-prod의 TCP 18080은 edge 내부 IP에서만 허용한다. 인터넷과 다른 VLAN에서의 접근은
차단한다. Docker publish port는 host firewall 도구에 따라 일반 UFW rule을 우회할 수
있으므로 사설 IP bind와 `DOCKER-USER`/nftables 또는 상위 방화벽 제한을 함께 확인한다.

```text
ALLOW tcp EDGE_INTERNAL_IP -> APPS_PROD_INTERNAL_IP:18080
DENY  tcp any              -> APPS_PROD_INTERNAL_IP:18080
```

SSH도 수동 빌드 서버 또는 Runner IP만 `kit-deploy`에 도달하도록 제한한다.

## 2. edge Nginx 준비

Cloudflare의 `kit.2juho.com` record는 proxy를 끈 DNS-only로 두고 edge의 공인 주소를
가리킨다. [edge 설정 예시](./edge/kit.2juho.com.conf.example)를 복사한 뒤 다음 두 값을
반드시 실제 값으로 바꾼다.

- `APPS_PROD_INTERNAL_IP`: edge에서 접근 가능한 apps-prod 내부 IP
- `/PATH/TO/...`: edge에서 이미 관리 중인 `kit.2juho.com` 인증서와 private key 경로

```sh
sudo nginx -T 2>/dev/null | grep -n 'server_name.*kit\.2juho\.com' || true
sudo install -o root -g root -m 0644 deploy/edge/kit.2juho.com.conf.example \
  /etc/nginx/sites-available/kit.2juho.com.conf
sudo editor /etc/nginx/sites-available/kit.2juho.com.conf
sudo ln -s /etc/nginx/sites-available/kit.2juho.com.conf \
  /etc/nginx/sites-enabled/kit.2juho.com.conf
sudo nginx -t
sudo systemctl reload nginx
```

첫 검사에서 `kit.2juho.com` server block이 이미 나오면 새 파일을 활성화하지 말고 기존
설정의 중복 hostname을 먼저 제거하거나 해당 block을 수정한다. `2juho.conf`가 wildcard
인증서를 관리한다면 인증서 경로와 공통 TLS 설정만 재사용하고 `server_name`은 정확히 한
block에서만 선언한다.
edge의 공통 TLS 설정에서 이미 HSTS를 추가한다면 예시의 HSTS header와 중복되지 않게 한
곳에서만 관리한다. `includeSubDomains`나 preload는 다른 `2juho.com` subdomain의 HTTPS
준비 상태를 확인하기 전에는 추가하지 않는다.

같은 symlink가 이미 있으면 `ln -s`를 반복하지 않는다. edge에서 origin 연결과 public TLS를
각각 확인한다.

```sh
curl -fsS -H 'Host: kit.2juho.com' http://APPS_PROD_INTERNAL_IP:18080/healthz
curl -fsS https://kit.2juho.com/healthz
```

기존 direct-static 예시는 제거했다. edge는
`/srv/data`를 직접 보지 않고 apps-prod origin에만 reverse proxy한다.

## 3. 배포 동작

- docs archive: HTML의 `kit-site-id`가 요청 ID와 정확히 일치하는지 확인하고
  `sites/<40-hex-id>`에 저장한 뒤 `current-site`를 원자적으로 교체한다.
- release archive: `releases/<version>`을 검증한 뒤 `current-release`를 교체한다.
- release는 같은 version을 덮어쓰지 않고 현재 version보다 낮거나 같은 version을 거부한다.
- origin은 site 전체 정적 파일, release metadata/checksum/binary를 배포 파일과 정확히
  비교한다. retry 후에도 origin 검증이 실패하면 직전 symlink를 복원한다.
- public HTTPS 검증은 호출자가 activation 이후 수행한다. public 실패만으로 서버가 자동
  rollback하지 않는다.
- release 성공 시 SemVer 순서로 최신 5개를 보존한다.

activator는 `/etc/kit/deploy.env`의 private origin만 호출한다. edge, DNS, 인증서는 public
E2E에 필요하지만 이 외부 경로 장애가 정상 origin 배포를 되돌리지는 않는다.

## 4. rollback

site와 release는 독립적으로 되돌릴 수 있다. symlink를 직접 만들지 말고 apps-prod의
root 관리 command를 사용한다. `kit-rollback`은 배포와 같은 `.deploy.lock`을 획득하고,
고유 임시 directory에서 symlink를 원자 교체한 뒤 origin을 정확히 검증한다. 실패하면 이전
link를 복원한다. root wrapper는 kind와 identifier를 검증한 후 `runuser`로
`kit-deploy` 권한을 적용하므로 배포 계정 소유 경로를 root 권한으로 열지 않는다.

```sh
sudo kit-rollback release v0.1.0
sudo kit-rollback site SITE_ID_40_HEX
```

그 후 public HTTPS를 별도 확인한다. public 확인 실패는 origin rollback 성공 여부와 별개로
edge/DNS/TLS 장애일 수 있으므로 자동으로 재전환하지 않는다. 운영자가 origin과 public
상태를 확인한 뒤 명시적으로 다른 version/ID로 `kit-rollback`을 다시 실행한다.

## 5. 운영 확인

```sh
# apps-prod
cd /srv/data/apps/kit
sudo ./deploy.sh status
sudo ./deploy.sh logs
curl -fsS -H 'Host: kit.2juho.com' http://APPS_PROD_INTERNAL_IP:18080/healthz

# edge
sudo nginx -t
curl -fsS https://kit.2juho.com/healthz
curl -fsS https://kit.2juho.com/
curl -fsS https://kit.2juho.com/version.txt
curl -fsS https://kit.2juho.com/release.json
```

장애를 구분할 때는 apps-prod 사설 주소, edge에서 origin 내부 주소, public URL 순서로
확인한다. 첫 두 단계는 정상인데 public URL만 실패하면 edge TLS/DNS/firewall 경로를
확인한다.
