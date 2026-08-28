# kit

`kit`은 반복해서 사용하던 개발 명령과 Git 작업 흐름을 하나의 Go CLI로 정리한 도구다.

- `kit compare`: source branch의 commit이 base branch에 반영됐는지 비교
- `kit pick`: 미반영 commit 선택, review branch 생성, push와 Gitea PR 생성을 처리
- `kit sync`: 최신 base를 반영하고 local `work` queue를 다시 구성
- `kit status`, `kit review`: cached legacy review metadata를 확인하는 고급 명령
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

# 1. work에서 아직 develop에 반영되지 않은 커밋 확인
kit compare

# 2. 필요한 커밋 선택 → branch 생성 → push → Gitea PR 생성 후 즉시 반환
kit pick feat/login

# 커밋이 많을 때: 미반영 커밋 전체를 selector 없이 선택
kit pick chore/refactor --all

# 로컬 branch만 만들고 PR을 생성하지 않는 고급 옵션
kit pick feat/local-only --local

# 3. PR이 Gitea에서 머지된 뒤 최신 develop과 work를 동기화
kit sync

# 고급 진단: cached legacy review metadata 확인
kit status
kit review list

# 설치본 정보와 update 확인
kit version
kit update
```

기계가 읽는 결과가 필요하면 `status`, `compare`, `sync`, `review`, `auth`,
`version`, `update`에 `--json`을 사용할 수 있다. `review submit --json`처럼 상태를 바꾸는
명령은 `--yes`를 함께 지정해야 한다.
다른 repository에서 실행하려면 `--cwd <path>`를 지정한다. `pick`은 full-screen selector를
사용하므로 기본 선택 흐름은 TTY에서만 실행된다. `--all`은 selector를 건너뛰고 미반영 commit
전체를 source 순서대로 선택한다. 확인 질문도 생략해야 하는 비대화형 환경에서는 `--yes`를 함께
지정한다.

일상 작업은 세 단계다.

```text
kit compare → kit pick <branch> → (Gitea에서 merge) → kit sync
```

`compare`는 `work`에서 아직 `develop`에 반영되지 않은 커밋을 읽기 전용으로 보여주고, 다음
`pick` 명령을 안내한다. `pick`은 선택한 미반영 커밋으로 review branch를 만들고 push한 뒤 Gitea
PR Create 요청을 한 번 수행하면 즉시 반환한다. PR Create 요청이 실패하거나 응답 metadata가 불완전하면 local/remote branch를 보존하므로,
Gitea에서 PR이 실제로 생성됐는지 먼저 수동으로 확인한 뒤 조치해야 한다.

`compare`, `status`, `sync`는 provider API를 호출하지 않는다. PR이 머지된 뒤 `kit sync`는
work를 재구성한 다음, 향후 Kit-created marker가 기록된 local review branch 중 Git이 현재 base에 포함됐음을
증명한 branch만 안전하게 정리한다. squash merge 또는 non-ancestor branch, Kit marker가 없는
branch와 보호 branch는 보존한다. remote branch는 건드리지 않으며, cleanup 실패는 sync를 실패시키지
않는 warning으로 남는다. 현재 checkout한 대상은 정확히 `origin/<branch>` upstream을 추적할 때만
`work`로 전환한 뒤 정리한다.

`status`, `review`, `backup`은 문제가 생겼을 때의 상세 진단·복구 경로다. 기존
`kit git status`, `kit git sync`, `kit git review ...`, `kit git work ...`와 `kit self ...`도
호환을 위해 계속 동작한다. 새 흐름에서는 중복 기능을 일상 명령으로 안내하지 않는다.

`kit sync`는 Git만 사용해 remote base를 fetch·fast-forward하고 `work`의 미반영 커밋을 최신
base 위에 재구성한다. Gitea merge 상태를 확인하지 않으므로, Gitea에서 merge가 완료된 뒤 실행한다.
`kit status`도 Git 상태와 cached legacy metadata만 표시한다.

`work`는 원격에 push하는 공유 branch가 아니라 로컬 commit queue다. `develop`이 바뀌면
`kit sync`가 기존 `work`를 backup branch로 보존한 뒤 최신 `develop`에서 `work`를 재구성한다.
재적용 대상은 `work`의 direct first-parent 일반 pending commit뿐이다. 충돌하면 임시 branch에서 파일을 해결하고 `git add`한 뒤
`continue`, 해당 commit을 제외하는 `skip`, 기존 work로 돌아가는 `abort`를 선택한다.
VS Code 등에서 해결 내용을 먼저 commit한 경우에도 `continue`를 선택하면 완료된 commit을
감지해 다음 commit으로 진행한다.
전체 재구성이 성공하기 전에는 `work`를 교체하지 않으며 abort나 입력 종료 시 기존
`work`와 checkout을 복원한다. `sync` 중 `develop`까지 갱신된 뒤 실패했다면 `develop`도
실행 전 hash로 되돌린다. 복원이 검증되면 그 실패 실행에서 만든 임시 backup은 자동으로
삭제하고, 검증이나 삭제가 실패하면 오류에 정확한 backup branch를 표시해 보존한다.
일부 충돌 해결 commit을 만든 뒤 재구성이 중단되면 해당 commit은 오류에 표시되는
`kit/recovery/*` branch에 별도로 보존한다. 성공한 sync 뒤에도 원본 `work` backup branch는 남으므로
다음 명령으로 확인·복원하거나 정리할 수 있다.

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

`kit sync`의 재구성은 merge commit과 그 side-parent에서만 reachable한 commit을 재적용하지 않는다.
실행 전 이 제외 범위를 경고하며, 성공해도 원본 `work`는 backup branch로 남아 복구할 수 있다. side 작업은
`work`에 merge하지 말고 필요한 변경을 direct commit으로 남기거나 별도 review branch로 올려 관리한다.

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

macOS login Keychain이 잠겨 token 조회에 사용자 상호작용이 필요하면, 일반 TTY에서 실행한
`pick` 또는 `review`는 `kit`이 한 번만 Keychain의 표준 비밀번호 prompt를 열고 같은 조회를
재시도한다. 비밀번호는 명령행 argument·환경 변수·파일에 전달하거나 저장하지 않는다. JSON,
CI, pipe처럼 대화형 TTY가 아닌 실행에서는 prompt를 열 수 없으므로 다음 명령으로 사용자가
먼저 해제한 뒤 원래 명령을 다시 실행해야 한다.

```sh
security unlock-keychain "$HOME/Library/Keychains/login.keychain-db"
```

자동 잠금 시간을 바꾸거나 잠금을 영구 해제하지 않는 것이 기본이다. Keychain 잠금은 세션과
비밀정보 보호를 위한 macOS 설정이며, kit가 이를 변경하지 않는다.

```sh
# Keychain을 사용할 수 없는 경우에만 사용하는 명시적 fallback
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
Git credential 또는 SSH key를 사용하며 API token과 분리한다.
Gitea API의 이식 가능한 create payload에는 draft flag가 없으므로 `review submit --draft`는
제목에 `WIP: `를 한 번만 붙인다. Gitea의 `WORK_IN_PROGRESS_PREFIXES` 설정에는 `WIP:`를
유지해야 draft로 표시된다.

`kit sync`는 PR merge 여부나 provider의 branch head를 검증하지 않는다. squash merge에서는
`cherry-pick -x` 기록과 patch-id로도 반영 여부를 확정하지 못할 수 있으므로, 필요한 커밋이
`compare`에서 남아 보이면 사용자가 Gitea와 Git history를 확인해 수동으로 판단해야 한다.
현재 자동 PR 흐름은 push remote와 PR 대상 repository가 같은 경우를 지원한다. fork에서
upstream으로 PR을 만드는 흐름은 push repository와 target repository 설정이 분리되어야 하므로
현재 범위에는 포함하지 않는다.

기본 `kit pick`은 branch를 만들기 전에 provider 설정과 credential을 먼저 검사한다. pick
완료 뒤에도 push를 시작하기 전에 submit이 실패하면 원 checkout으로 돌아가 생성한 local
branch와 picked state를 제거한다. push가 시작된 뒤의 timeout은 원격 적용 여부가 불확실할
수 있으므로 branch와 review metadata를 보존한다. Gitea에서 PR 생성 여부를 수동으로 확인한 뒤
필요한 조치를 한다.

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
