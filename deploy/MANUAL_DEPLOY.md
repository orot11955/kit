# Forgejo 없이 수동 배포

이 절차는 Forgejo 서버가 복구될 때까지 build host에서 archive를 만들고, 기존
forced-command SSH 경로로 apps-prod에 전달한다. 서버에서 임의 shell을 실행하거나 배포
script를 archive에 포함하지 않는다.

## 전제 조건

- build host: Ubuntu 24.04 amd64, Go 1.23 이상, `git`, Bash, GNU tar/coreutils,
  OpenSSH client
- apps-prod와 edge: [README.md](./README.md)에 따라 origin, TLS, 방화벽, wrapper 준비 완료
- build host에 이 프로젝트 source가 Git clone 이외의 안전한 방법으로 복사되어 있음
- manual 전용 임시 private key와 검증된 apps-prod SSH host key가 있음. Forgejo Runner
  private key와 공유하지 않음

CLI의 `compare`, `pick` 및 관련 test는 local system `git`을 사용한다. Forgejo 서버는
필요하지 않지만 build host의 `git` 실행 파일은 필요하다.

아래 예시는 프로젝트 root에서 실행한다. 먼저 변수와 SSH 연결 대상을 실제 값으로 둔다.

```sh
export KIT_DEPLOY_HOST='APPS_PROD_INTERNAL_IP_OR_NAME'
export KIT_DEPLOY_PORT='22'
export KIT_DEPLOY_USER='kit-deploy'
KIT_KEY_DIR=$(mktemp -d)
chmod 0700 "$KIT_KEY_DIR"
export KIT_DEPLOY_KEY="$KIT_KEY_DIR/kit-manual-deploy-key"
```

임시 key가 없다면 build host에서 생성한다.

```sh
ssh-keygen -t ed25519 -f "$KIT_DEPLOY_KEY" -C kit-manual-deploy
ssh-keygen -lf "$KIT_DEPLOY_KEY.pub" -E sha256
```

public key는 apps-prod의 `/home/kit-deploy/.ssh/authorized_keys`에 다음처럼 등록한다.
출력한 SHA256 fingerprint를 운영 기록과 비교하고, `BUILD_HOST_IP` 제한과 `restrict`,
forced command identity를 제거하지 않는다.

```text
from="BUILD_HOST_IP",restrict,command="/usr/local/libexec/kit-ssh-wrapper manual" ssh-ed25519 AAAA... kit-manual-deploy
```

Forgejo용 key는 별도 keypair와 별도 line
`command="/usr/local/libexec/kit-ssh-wrapper forgejo"`를 사용한다. manual key를 Forgejo
secret에 넣거나 Forgejo key를 수동 build host에 복사하지 않는다.

host key는 별도 신뢰 경로로 확인해 build host의 `known_hosts`에 미리 등록한다. 배포 명령은
`StrictHostKeyChecking=yes`를 사용하며 host key 검증을 끄지 않는다.

## 1. 로컬 검증

```sh
make check
```

검증이 끝나기 전에는 배포 archive를 만들지 않는다.

## 2. docs 배포

수동 docs에는 Git commit이 없으므로 marker를 넣기 전 정적 파일 manifest의 SHA-256 앞
40자를 식별자로 사용한다. 그 ID를 `index.html`의 `kit-site-id` meta로 넣은 뒤 archive를
만든다. archive 자체의 hash를 archive 안에 넣는 순환 구조가 아니다. 이 값은 형식만
40자리 hex일 뿐 **실제 Git commit SHA가 아니다**. Forgejo가 복구되면 자동 배포는 실제
main commit SHA를 marker와 배포 ID로 사용한다.

```sh
set -Eeuo pipefail

rm -rf .dist/manual-site
mkdir -p .dist/manual-site/site
cp site/index.html site/styles.css site/app.js site/favicon.svg .dist/manual-site/site/
cp install.sh .dist/manual-site/site/install.sh

SITE_ID=$(
  cd .dist/manual-site/site
  sha256sum app.js favicon.svg index.html install.sh styles.css |
    sha256sum |
    awk '{print substr($1, 1, 40)}'
)
SITE_MARKER="<meta name=\"kit-site-id\" content=\"$SITE_ID\" />"
awk -v marker="$SITE_MARKER" '
  /<\/head>/ { print "    " marker; inserted++ }
  { print }
  END { if (inserted != 1) exit 1 }
' .dist/manual-site/site/index.html > .dist/manual-site/site/index.html.with-id
mv .dist/manual-site/site/index.html.with-id .dist/manual-site/site/index.html
[[ $(grep -Fc "$SITE_MARKER" .dist/manual-site/site/index.html) -eq 1 ]]
tar -C .dist/manual-site/site -czf .dist/manual-site/kit-site.tar.gz .

printf 'manual site id (not a Git SHA): %s\n' "$SITE_ID"

ssh -i "$KIT_DEPLOY_KEY" \
  -o BatchMode=yes \
  -o IdentitiesOnly=yes \
  -o StrictHostKeyChecking=yes \
  -p "$KIT_DEPLOY_PORT" \
  "$KIT_DEPLOY_USER@$KIT_DEPLOY_HOST" \
  "upload-site $SITE_ID" < .dist/manual-site/kit-site.tar.gz
```

activation이 성공한 뒤 public E2E를 별도로 확인한다. retry 후 실패해도 이 command는
rollback을 호출하지 않는다.

```sh
PUBLIC_CURL=(
  curl --fail --silent --show-error
  --retry 2 --retry-delay 1 --retry-connrefused
  --connect-timeout 5 --max-time 30
)
mkdir -p .dist/manual-site/public
fetch_site_once() {
  local asset url
  for asset in index.html styles.css app.js favicon.svg install.sh; do
    url="https://kit.2juho.com/$asset"
    [[ $asset != index.html ]] || url=https://kit.2juho.com/
    "${PUBLIC_CURL[@]}" "$url" -o ".dist/manual-site/public/$asset" || return
    cmp -s ".dist/manual-site/site/$asset" ".dist/manual-site/public/$asset" || return
  done
  [[ $(grep -Fc "$SITE_MARKER" .dist/manual-site/public/index.html) -eq 1 ]]
}
PUBLIC_VERIFIED=false
for attempt in 1 2 3 4 5; do
  if fetch_site_once; then
    PUBLIC_VERIFIED=true
    break
  fi
  sleep $((attempt * 2))
done
[[ $PUBLIC_VERIFIED == true ]]
```

## 3. release 배포

> Production SemVer는 immutable이다. activator는 기존 version 덮어쓰기와 현재 version
> 이하 배포를 거부한다. 단순 테스트용 version을 production에 올리면 그 version을 나중에
> 정식 릴리스로 재사용할 수 없으므로, 실제 공개할 version만 선택한다.

`VERSION`은 현재 공개 version보다 높은 `vMAJOR.MINOR.PATCH`로 지정한다.

```sh
curl -fsS https://kit.2juho.com/version.txt || true
export VERSION='v0.1.0'
```

Git commit 대신 source archive SHA-256 앞 40자를 build 식별자로 사용한다. 이
`SOURCE_ID` 역시 **실제 Git commit SHA가 아니다**. source archive는 배포하지 않아도
release와 함께 안전한 내부 위치에 반드시 보관해 나중에 build 입력을 추적할 수 있게 한다.
현재 source가 이미 신뢰할 수 있는 별도 경로로 전달됐다는 전제이며, `SOURCE_ID` 자체는
source의 작성자나 출처를 인증하지 않는다.

```sh
set -Eeuo pipefail

rm -rf .dist/manual-release
mkdir -p .dist/manual-release/artifacts .dist/manual-release/build-input

if find go.mod go.sum cmd internal -type l -print -quit | grep -q .; then
  echo 'source input must not contain symbolic links' >&2
  exit 1
fi

tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
  -czf .dist/manual-release/source.tar.gz go.mod go.sum cmd internal
SOURCE_ID=$(sha256sum .dist/manual-release/source.tar.gz | awk '{print substr($1, 1, 40)}')
PUBLISHED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS="-s -w -X kit/internal/buildinfo.Version=$VERSION -X kit/internal/buildinfo.Commit=$SOURCE_ID -X kit/internal/buildinfo.BuildDate=$PUBLISHED_AT"

tar -xzf .dist/manual-release/source.tar.gz -C .dist/manual-release/build-input
PROJECT_ROOT=$PWD

(
  cd .dist/manual-release/build-input
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
    go build -trimpath -ldflags "$LDFLAGS" \
    -o "$PROJECT_ROOT/.dist/manual-release/artifacts/kit_darwin_arm64" ./cmd/kit
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "$LDFLAGS" \
    -o "$PROJECT_ROOT/.dist/manual-release/artifacts/kit_linux_amd64" ./cmd/kit
)

{
  sha256sum .dist/manual-release/source.tar.gz
  go version
  go env -json GOVERSION GOOS GOARCH GOPROXY GOSUMDB
} > .dist/manual-release/build-record.txt

(
  cd .dist/manual-release/artifacts
  sha256sum kit_darwin_arm64 kit_linux_amd64 > checksums.txt
)

deploy/generate-release-metadata.sh \
  .dist/manual-release/artifacts \
  "$VERSION" \
  "$SOURCE_ID" \
  "$PUBLISHED_AT" \
  https://kit.2juho.com

.dist/manual-release/artifacts/kit_linux_amd64 version --json
(
  cd .dist/manual-release/artifacts
  sha256sum -c checksums.txt
)
tar -C .dist/manual-release/artifacts \
  -czf .dist/manual-release/kit-release.tar.gz .
```

출력된 `version`, `commit`, `build_date`, `target`이 각각 입력한 version, `SOURCE_ID`, 시간,
`linux/amd64`와 일치하는지 확인한 뒤 업로드한다.

```sh
ssh -i "$KIT_DEPLOY_KEY" \
  -o BatchMode=yes \
  -o IdentitiesOnly=yes \
  -o StrictHostKeyChecking=yes \
  -p "$KIT_DEPLOY_PORT" \
  "$KIT_DEPLOY_USER@$KIT_DEPLOY_HOST" \
  "upload-release $VERSION" < .dist/manual-release/kit-release.tar.gz
```

## 4. 배포 결과 확인

activation이 끝난 뒤 public `version.txt`, `release.json`, `checksums.txt`는 로컬 산출물과
정확히 비교하고 두 binary는 공개 다운로드본의 SHA-256을 확인한다. public 실패는 edge,
DNS, TLS 문제일 수 있으므로 자동 rollback을 실행하지 않는다.

```sh
PUBLIC_CURL=(
  curl --fail --silent --show-error
  --retry 2 --retry-delay 1 --retry-connrefused
  --connect-timeout 5 --max-time 120
)
mkdir -p .dist/manual-release/public
fetch_exact() {
  local expected=$1
  local url=$2
  local output=$3
  local attempt
  for attempt in 1 2 3 4 5; do
    if "${PUBLIC_CURL[@]}" "$url" -o "$output" && cmp -s "$expected" "$output"; then
      return
    fi
    sleep $((attempt * 2))
  done
  echo "public content does not match: $url" >&2
  return 1
}

fetch_exact .dist/manual-release/artifacts/version.txt \
  https://kit.2juho.com/version.txt .dist/manual-release/public/version.txt
fetch_exact .dist/manual-release/artifacts/release.json \
  https://kit.2juho.com/release.json .dist/manual-release/public/release.json
fetch_exact .dist/manual-release/artifacts/checksums.txt \
  "https://kit.2juho.com/downloads/$VERSION/checksums.txt" \
  .dist/manual-release/public/checksums.txt
for artifact in kit_darwin_arm64 kit_linux_amd64; do
  fetch_exact ".dist/manual-release/artifacts/$artifact" \
    "https://kit.2juho.com/downloads/$VERSION/$artifact" \
    ".dist/manual-release/public/$artifact"
done

(
  cd .dist/manual-release/public
  sha256sum -c checksums.txt
)
```

public 장애와 별개로 명시적 rollback이 필요하다고 운영자가 판단한 경우 apps-prod에서만
다음을 실행한다. `kit-rollback`은 기존 배포 lock을 공유하고, origin 검증 실패 시 이전
link를 복원한다. root wrapper는 입력을 검증한 뒤 activator를 `kit-deploy` 권한으로 낮춰
실행한다. rollback 후 public smoke는 위와 별도로 다시 수행한다.

```sh
sudo kit-rollback release v0.1.0
sudo kit-rollback site SITE_ID_40_HEX
```

설치 script는 바로 pipe하기 전에 내려받아 검토할 수 있다.

```sh
curl -fsS https://kit.2juho.com/install.sh -o /tmp/kit-install.sh
sh -n /tmp/kit-install.sh
less /tmp/kit-install.sh
```

임시 HOME 설치 smoke test:

```sh
KIT_TEST_HOME=$(mktemp -d)
HOME="$KIT_TEST_HOME" SHELL=/bin/bash sh /tmp/kit-install.sh
"$KIT_TEST_HOME/.local/bin/kit" version --json
```

## `kit update` E2E 제한

실제 update E2E에는 설치할 이전 version과 업데이트할 다음 version, 즉 production에 연속된
두 version을 배포해야 한다. 현재 updater의 endpoint는 `https://kit.2juho.com`으로 고정되어
있어 별도 staging domain을 사용할 수 없다. update만 시험하려고 production version 두
개를 소비하지 않는다. 첫 실제 version을 수동 배포하고, 다음 정식 version 또는 endpoint
설정 기능이 생겼을 때 E2E를 수행하는 것이 안전하다.

## Forgejo 복구 후

1. 임시 manual public key를 apps-prod `authorized_keys`에서 제거한다.
2. 임시 private key를 build host에서 안전하게 폐기한다.
3. Runner 전용 key와 `from="RUNNER_IP"` 제한을 등록한다.
4. 수동 배포 version보다 높은 새 SemVer tag로 자동 release를 시작한다.
5. 수동 `SOURCE_ID`는 Git SHA가 아니므로 해당 release metadata를 Git commit으로 해석하지
   않는다.

임시 key를 폐기하기 전에 apps-prod의 `authorized_keys`에서 해당 public key 한 줄을 먼저
제거한다. 그다음 build host에서 임시 key directory를 삭제한다.

```sh
rm -f "$KIT_DEPLOY_KEY" "$KIT_DEPLOY_KEY.pub"
rmdir "$KIT_KEY_DIR"
```

`.dist/manual-release/source.tar.gz`와 `build-record.txt`는 관리자 경로를 통해
`/srv/data/backups/kit/<version>/` 같은 비공개 backup 위치에 보존한다. forced-command
deploy key는 이 backup 경로에 접근할 권한을 갖지 않는다.
