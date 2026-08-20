# kit

`kit`은 반복해서 사용하던 개발 명령과 Git 작업 흐름을 하나의 Go CLI로 정리한 도구다.

- `kit status`: `main/develop/work`와 진행 중인 Gitea review를 한 화면에서 확인
- `kit compare`: source branch의 commit이 base branch에 반영됐는지 비교
- `kit pick`: 미반영 commit 선택, review branch 생성, push와 Gitea PR 생성을 한 번에 처리
- `kit sync`: merge 확인, base fast-forward, work 재구성과 로컬 review branch 정리
- `kit review`: review 대기·개별 복구를 위한 고급 명령
- `kit backup`: work backup 확인·복원·정리
- `kit auth login`: Gitea API token을 OS 보안 저장소에 한 번 등록
- `kit version`: 설치된 build 정보 확인
- `kit update`: checksum과 build metadata를 검증한 뒤 설치본 갱신

실행 중 필요한 외부 명령은 system `git`뿐이다. commit selector는 binary 안에 포함되어
별도 `fzf` 설치가 필요하지 않다.

## 설치

첫 production release 배포 후 다음 명령으로 설치한다.

```sh
curl -fsSL https://kit.2juho.com/install.sh | sh
```

공식 지원 대상은 다음 두 환경이다.

- Apple Silicon macOS (`darwin/arm64`)
- Ubuntu 24.04 LTS x86-64 (`linux/amd64`)

Intel Mac, Ubuntu ARM64, 다른 Linux distribution과 Windows는 공식 지원하지 않는다.

## 사용

```sh
# 저장소별 기본 역할 설정(main/develop/work, origin, Gitea provider)
kit config init

# Gitea 웹에서 발급한 API token을 한 번 등록(입력값은 화면에 표시되지 않음)
kit auth login gitea --host git.company.example

# 전체 workflow와 원격 추적 상태 확인
kit status

# 느린 Gitea API를 기다리지 않고 마지막으로 저장된 review 상태만 확인
kit status --cached

# work의 commit이 develop에 반영됐는지 확인
kit compare

# source와 base를 직접 지정하고 최신 10개만 확인
kit compare work --base develop --limit 10

# work의 미반영 commit을 선택해 branch를 만들고 push와 PR 생성
kit pick feat/login

# 로컬 branch만 만들고 PR을 생성하지 않는 고급 옵션
kit pick feat/local-only --local

# 생성 후 foreground에서 merge를 기다리고 완료 시 정리
kit pick feat/login --wait

# 다른 개발자의 merge를 반영하고 work를 최신 develop 위에 재구성
kit sync

# 다른 terminal에서 상태 확인 또는 merge 대기
kit status
kit review wait feat/login

# 여러 review가 동시에 머지되어 명시적인 선택이 필요할 때만 사용
kit review finish feat/login

# 설치본 정보와 update 확인
kit version
kit update
```

기계가 읽는 결과가 필요하면 `status`, `compare`, `sync`, `review`, `auth`,
`version`, `update`에 `--json`을 사용할 수 있다. `review submit --json`과
`review finish --json`처럼 상태를 바꾸는 명령은 `--yes`를 함께 지정해야 한다.
다른 repository에서 실행하려면 `--cwd <path>`를 지정한다. `pick`은 full-screen selector를
사용하므로 TTY에서만 실행된다.

일상 작업은 `kit status`, `kit pick <branch>`, `kit sync` 세 명령을 기준으로 한다.
기존 `kit git status`, `kit git sync`, `kit git review ...`, `kit git work ...`는 호환을 위해
계속 동작하지만 새 문서와 `다음` 안내에는 평탄화된 명령만 표시한다. `kit pick`은 기본적으로
PR까지 생성하며, 기존 `--submit`은 호환 옵션으로만 남는다.

`kit sync`는 추적 중인 review를 먼저 갱신한다. 머지된 review가 하나면 provider의 source SHA와
merge SHA를 검증한 뒤 work 동기화와 로컬 branch 정리를 이어서 수행한다. 동시에 머지된
review가 여러 개면 임의로 순서를 선택하지 않고 `kit review finish <branch>`를 안내한다.
review API를 의도적으로 건너뛰고 base만 갱신해야 하는 복구 상황에는 `kit sync --base-only`를
사용한다.

`kit status`는 최신 review 상태를 확인하되 전체 provider 조회를 5초 안에 끝낸다. Gitea가
느리거나 응답하지 않으면 해당 review는 저장된 상태로 표시하고 경고를 남긴다. 즉시 로컬 상태만
보고 싶을 때는 `kit status --cached`를 사용한다.

`work`는 원격에 push하는 공유 branch가 아니라 로컬 commit queue다. `develop`이 바뀌면
`kit sync`가 기존 `work`를 먼저 backup한 뒤 최신 `develop` 위에 미반영 commit만
위상 순서대로 다시 적용한다. 충돌하면 임시 branch에서 파일을 해결하고 `git add`한 뒤
`continue`, 해당 commit을 제외하는 `skip`, 기존 work로 돌아가는 `abort`를 선택한다.
VS Code 등에서 해결 내용을 먼저 commit한 경우에도 `continue`를 선택하면 완료된 commit을
감지해 다음 commit으로 진행한다.
전체 재구성이 성공하기 전에는 `work`를 교체하지 않으며 abort나 입력 종료 시 기존
`work`와 checkout을 복원한다. `sync` 중 `develop`까지 갱신된 뒤 실패했다면 `develop`도
실행 전 hash로 되돌린다. 복원이 검증되면 그 실패 실행에서 만든 임시 backup은 자동으로
삭제하고, 검증이나 삭제가 실패하면 오류에 정확한 backup branch를 표시해 보존한다.
일부 충돌 해결 commit을 만든 뒤 재구성이 중단되면 해당 commit은 오류에 표시되는
`kit/recovery/*` branch에 별도로 보존한다. 성공한 sync/restore의 backup은 다음 명령으로
확인하고 정리할 수 있다.

```sh
kit backup list
kit backup cleanup --dry-run       # 자동 sync backup만 미리 보기
kit backup cleanup                 # 확인 후 자동 sync backup 삭제
kit backup cleanup --all           # 수동·restore safety backup까지 포함
```

`cleanup`은 현재 설정된 `work`의 `kit/backup/*`만 대상으로 하며 `kit/recovery/*`와
`kit/tmp/*`는 건드리지 않는다. 신규 backup은 source 원문 hash로 분리되어 `a/b`와
`a-b`처럼 표시 이름이 비슷한 branch도 서로 삭제하지 않는다. 구버전 형식은 기본 source가
정확히 `work`인 backup만 자동 인식한다. 다른 source로 만든 구버전 backup은 소유권을
증명할 수 없어 `git branch --list 'kit/backup/*'`로 확인한 뒤 Git에서 직접 정리해야 한다.
중단된 `pick`은
`kit pick --continue`, `--skip`, `--abort`로
이어갈 수 있다.

회사와 개인 repository는 모두 Gitea provider를 사용하며 `kit config init`의 기본값도
`gitea`다. `auto`를 별도로 선택한 경우 remote hostname에 `gitea`가 포함되지 않은 사설
도메인은 제품을 안전하게 판별할 수 없으므로 Gitea로 명시 설정해야 한다.

```sh
kit config set git.provider gitea
```

사내 Gitea가 TLS 없이 사설 IP의 HTTP로만 제공되는 경우에는 repository별로 명시적으로
허용할 수 있다. remote URL도 `http://`로 설정되어 있고 host가 RFC1918/loopback/link-local
literal IP일 때만 적용되며, hostname이나 public IP에는 사용할 수 없다.

```sh
kit config set git.allow-insecure-http true
```

이 설정은 API token을 네트워크에 평문으로 전송한다. kit는 자동으로 HTTPS에서 HTTP로
낮추지 않으며 review API를 사용하는 command마다 stderr에 경고한다. 가능해지는 즉시
HTTPS reverse proxy로 전환하고 설정을 `false`로 되돌리는 것을 권장한다.

리뷰 API token은 프로젝트나 `.zshrc`에 넣지 않는다. Gitea의 **사용자 설정 →
Applications → Manage Access Tokens**에서 `write:repository` 권한으로 token을 발급한 뒤,
아래 명령의 숨김 입력에 한 번 붙여 넣는다. Gitea는 발급 직후에만 token 원문을 보여주므로
그 화면에서 바로 등록한다.

```sh
kit auth login gitea --host git.company.example
kit auth status gitea --host git.company.example

# 회사와 개인 Gitea가 다르면 host별로 각각 한 번 등록한다.
kit auth login gitea --host git.home.example
kit auth list
```

macOS에서는 Keychain, Ubuntu에서는 Secret Service를 기본 저장소로 사용한다. Linux에
Secret Service가 없는 환경에서는 확인 후, macOS Keychain을 사용할 수 없는 환경에서는
`--store file`을 명시한 경우에만 권한이 제한된 local file을 사용할 수 있다. macOS에서는
file 저장으로 자동 전환하지 않는다. 저장된 token은 `auth status`, `auth list`, `--json`,
log에 출력하지 않는다.
token을 폐기하거나 다시 발급했다면 `kit auth logout gitea --host <host>` 후 다시 login한다.

```sh
# Keychain 복구가 어려울 때만 사용하는 명시적 fallback
kit auth login gitea --host git.company.example --store file
```

`KIT_GITEA_TOKEN`과 `KIT_GITEA_HOST`는 CI 또는 한 번만 실행할 override에서만 함께
사용한다. 둘 중 하나만 설정하면 kit는 저장된 token으로 조용히 대체하지 않고 오류를
반환한다.

```sh
KIT_GITEA_HOST=git.company.example KIT_GITEA_TOKEN='...' kit review submit
```

GitLab/Forgejo에서 진행 중이던 review ID와 URL은 Gitea로 자동 이식하지 않는다. 기존
review를 정리한 뒤 새 remote와 provider를 설정하고 같은 review branch에서
`kit review submit`을 다시 실행해 Gitea PR을 생성한다. 완료된 legacy review state는
기존 설치와 전환 기록을 읽을 수 있도록 schema 1 호환을 유지한다.

host 값은 scheme이나 path 없는 정확한 소문자 `host[:port]` 형식이어야 한다. PR 생성에는
Gitea의 `write:repository` scope가 필요하다. 이 scope는 repository API 쓰기 권한이므로
Gitea 사용자 자체도 필요한 repository에만 접근하도록 제한한다. branch push 인증은 기존
Git credential 또는 SSH key를 사용하며 API token과 분리한다. `review wait`는
daemon이 아니라 현재 terminal에서만 동작하며 기본 15초 간격으로 확인한다. 개별 API
요청은 최대 60초 기다리고 network timeout은 review 실패로 확정하지 않고 다음 poll에서
재시도한다. `Ctrl-C`로 언제든 중단할 수 있으며 review state는 그대로 유지된다.
Gitea API의 이식 가능한 create payload에는 draft flag가 없으므로 `review submit --draft`는
제목에 `WIP: `를 한 번만 붙인다. Gitea의 `WORK_IN_PROGRESS_PREFIXES` 설정에는 `WIP:`를
유지해야 draft로 표시된다.

`review finish`는 provider가 merge를 확인한 뒤에만 `develop`과 `work`를 동기화한다. 일반
merge는 `git branch -d`로 로컬 review branch를 자동 정리한다. squash merge라서 Git의
safe delete가 거부되면 내용을 확인한 뒤 `--force-delete`를 명시해야 한다. 원격 branch
삭제 여부는 provider 설정에 맡기며 kit가 임의로 강제 삭제하지 않는다.
현재 자동 PR 흐름은 push remote와 PR 대상 repository가 같은 경우를 지원한다. fork에서
upstream으로 PR을 만드는 흐름은 push repository와 target repository 설정이 분리되어야 하므로
현재 범위에는 포함하지 않는다.

기본 `kit pick`은 branch를 만들기 전에 provider 설정과 credential을 먼저 검사한다. pick
완료 뒤에도 push를 시작하기 전에 submit이 실패하면 원 checkout으로 돌아가 생성한 local
branch와 picked state를 제거한다. push가 시작된 뒤의 timeout은 원격 적용 여부가 불확실할
수 있으므로 branch와 review state를 보존하고 `kit review status` 또는 `wait`로 재개한다.

## 개발

Go 1.23 이상이 필요하다.

```sh
make check
make build
./bin/kit version
```

release build에서는 Git tag, commit과 UTC build time을 다음 symbol에 ldflags로 주입한다.

```text
kit/internal/buildinfo.Version
kit/internal/buildinfo.Commit
kit/internal/buildinfo.BuildDate
```

## 배포

- `main` push는 `site/`와 `install.sh`를 docs server에 배포한다.
- `origin/main`에 포함된 `vX.Y.Z` tag push만 두 target binary와 `release.json`을 생성하고
  stable release를 전환한다.
- apps-prod는 `/srv/data/apps/kit/data`의 immutable directory와 atomic
  symlink를 Docker 정적 origin에 read-only로 제공한다.
- Cloudflare DNS-only 요청은 edge Nginx에서 TLS를 종료한 뒤 apps-prod 내부 origin으로
  reverse proxy한다.

Gitea secrets, 배포 계정과 Nginx 설정 방법은 [deploy/README.md](deploy/README.md)를,
전체 설계와 결정 사항은 [docs/kit-architecture.md](docs/kit-architecture.md)를 참고한다.

dotfiles 동기화는 현재 범위에 포함하지 않는다. 우선 명령 기능과 설치·업데이트 경로를
안정화한 뒤 별도 단계로 다룬다.
