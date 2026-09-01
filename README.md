# kit

`kit`은 `work → develop → main` Git workflow와 Gitea review lifecycle을 하나의 Go CLI로 정리한 도구다.

핵심 역할은 다음과 같다.

- `kit compare`: local work queue에서 아직 base에 반영되지 않은 commit 확인
- `kit pick`: pending commit 선택 → review branch 생성 → push → Gitea PR 생성/재사용
- `kit review add`: 열린 Kit-managed PR에 추가 work commit 반영
- `kit review status/list/finish`: provider 상태 확인부터 merge 후 reconcile·sync·cleanup까지 관리
- `kit sync`: provider와 무관하게 최신 base를 반영하고 local `work` queue를 안전하게 재구성
- `kit backup`: sync/recovery backup 조회·복원·정리
- `kit doctor`: local/network/recovery 진단
- `kit worktree`, `kit branch-clean`: worktree와 안전한 local review branch 정리
- `kit port`, `kit process`: local port/process 조회와 안전한 signal mutation
- `kit update`: 검증된 self-update, update check, previous binary rollback

실행 중 필요한 핵심 외부 도구는 system `git`이다. `kit port`는 `lsof`를 사용한다. macOS에는 기본 제공되며 Ubuntu에서는 필요 시 `sudo apt install lsof`로 설치한다. commit selector는 binary에 포함된 terminal UI를 사용하므로 별도 `fzf` 설치가 필요하지 않다.

## 지원 환경

공식 지원 target은 다음 두 환경이다.

- Apple Silicon macOS: `darwin/arm64`
- Ubuntu 24.04 LTS x86-64: `linux/amd64`

GitHub CI에서 Ubuntu 24.04와 실제 Apple Silicon `macos-15` runner를 모두 사용해 `go vet`, `go test`, native build를 검증한다.

## 설치

```sh
curl -fsSL https://kit.2juho.com/install.sh | sh
```

설치 후:

```sh
kit version
kit update --check
```

## 저장소 최초 설정

기존 저장소에 설정값만 기록하려면:

```sh
kit config init
```

새 clone에서 `main`, `develop`, local-only `work`까지 초기화하려면:

```sh
kit config bootstrap
```

`bootstrap`은 configured `git.remote`를 fetch하고 missing local `main`/`develop`을 remote ref에서 만든다. `work`가 없으면 `develop`에서 만든다. 기존 `work`를 덮어쓰지 않으며 configured remote에 `work`가 존재하면 local-only queue 계약 위반으로 중단한다.

Gitea review API를 사용하려면 token을 한 번 등록한다.

```sh
kit auth login gitea --host git.company.example
kit auth status gitea --host git.company.example
```

macOS는 Keychain, Ubuntu는 Secret Service를 기본 저장소로 사용한다. `--store file`은 명시적인 fallback이며 credential file은 제한된 권한으로 저장한다. token은 status/list/JSON/log에 출력하지 않는다.

### Gitea fork에서 upstream PR 만들기

기본 same-repository workflow에서는 `git.remote` 하나만 사용하면 된다.

fork에서 upstream으로 PR을 보내려면 remote 역할을 명시적으로 나눈다.

```sh
git remote add upstream git@git.company.example:team/project.git
git remote add origin git@git.company.example:my-user/project.git

kit config set git.remote upstream
kit config set git.push-remote origin
kit doctor --network
```

의미는 다음과 같다.

```text
git.remote       upstream/base sync + PR target
git.push-remote  fork review branch push source
```

`git.push-remote`를 설정하지 않으면 자동으로 `git.remote`를 사용하므로 기존 repository 설정은 그대로 동작한다.

fork workflow에서는 review branch를 fork remote에 push하고 Gitea upstream repository에 `<fork-owner>:<branch>`를 source로 PR을 생성한다. `review add`도 fork branch를 갱신하며, `review finish` cleanup은 saved published tip이 정확히 일치하는 fork branch만 삭제한다. upstream에 같은 이름의 branch가 있어도 삭제하지 않는다.

현재 cross-repository review는 같은 Gitea host의 fork만 지원한다. 자세한 safety contract는 [docs/fork-review.md](docs/fork-review.md)를 참고한다.

## 권장 일상 workflow

```text
kit compare
  ↓
kit pick <review-branch>
  ↓
Gitea PR review / merge
  ↓
kit review finish <review-branch>
```

예:

```sh
kit compare
kit pick feat/login
# Gitea에서 merge
kit review finish feat/login
```

`review finish`는 provider에서 PR이 실제 `merged`인지 확인한 뒤 다음 순서로 동작한다.

1. upstream base fetch 및 fast-forward
2. PR 생성 시 저장한 source commit metadata로 squash merge를 reconcile
3. 남은 `work` commit을 최신 base 위에 재구성
4. exact published tip이 확인되는 Kit-managed local/published remote review branch만 정리
5. review state를 `synced`/`cleaned`로 기록

일반 `kit sync`는 계속 Git-only다. provider API를 호출하지 않으며 PR lifecycle과 관계없이 base/work queue를 동기화해야 할 때 사용할 수 있다.

## compare

```sh
kit compare
kit compare work --base develop
kit compare --fetch --json
```

반영 여부는 다음을 사용한다.

1. `git cherry-pick -x` 원본 commit hash
2. stable patch-id

base patch history는 `git log | git patch-id` streaming pipeline으로 처리하고 candidate patch는 batch 계산한다. `-x`만으로 전부 판정 가능한 경우 patch scan을 생략한다.

## pick

대화형 선택:

```sh
kit pick feat/login
```

모든 pending commit 선택:

```sh
kit pick chore/refactor --all
```

commit을 명시하는 자동화 경로:

```sh
kit pick feat/login --commit <sha1> --commit <sha2> --yes
```

입력한 hash 순서와 무관하게 실제 cherry-pick은 source의 원래 pending 순서를 유지한다.

실행 계획만 확인:

```sh
kit pick feat/login --commit <sha> --dry-run --json
```

로컬 branch만 생성:

```sh
kit pick feat/local --local
```

비대화형 JSON mutation은 `--all` 또는 `--commit`과 `--yes`를 사용한다. conflict가 발생하면 기존 pick state를 보존하므로 다음 명령으로 이어갈 수 있다.

```sh
kit pick --continue
kit pick --skip
kit pick --abort
```

## review lifecycle

### PR 생성 / 재실행

`kit pick`은 기본적으로 push 후 Gitea PR을 생성한다. 동일 source/base의 open PR이 이미 있으면 새 PR을 만들지 않고 기존 PR을 재사용한다. fork mode에서는 fork owner까지 일치해야 같은 PR로 재사용한다.

현재 branch를 직접 submit할 수도 있다.

```sh
kit review submit
```

### 진행 중 PR에 commit 추가

```sh
kit review add feat/login
kit review add feat/login --commit <sha>
kit review add feat/login --all
```

다음 조건을 모두 확인한 뒤에만 추가한다.

- review state가 존재하고 provider PR이 `open`
- branch가 Kit-created marker를 가짐
- working tree clean
- local branch/upstream/published remote/provider tip이 저장된 `PublishedTip`과 일치
- base/source가 최신 상태
- 선택 commit이 현재 pending work commit

cherry-pick 중 conflict가 나면 추가 시작 전 review tip으로 전체 추가분을 rollback하고 remote는 변경하지 않는다. push 성공 후에만 review state를 갱신한다.

### 상태 조회

```sh
kit review status feat/login
kit review list
kit review list --refresh
```

`--refresh`는 저장된 active review를 provider에서 갱신한다. saved push remote나 target repository가 현재 설정과 달라지면 provider mutation/refresh를 중단한다.

### merge 완료

```sh
kit review finish feat/login
```

자동화에서는:

```sh
kit review finish feat/login --yes --json
```

JSON mutation은 반드시 `--yes`가 필요하다.

## sync / backup / recovery

`work`는 remote에 push하는 공유 branch가 아니라 local commit queue다.

```sh
kit sync
```

sync는:

- configured `git.remote` fetch/prune
- base fast-forward
- 기존 work backup 생성
- 최신 base에서 direct first-parent pending commit 재구성
- 성공 전까지 기존 work ref 유지
- 실패 시 original checkout/base/work 복원 및 검증
- 일부 conflict resolution commit이 생긴 뒤 실패하면 `kit/recovery/*`에 보존

backup 관리:

```sh
kit backup list
kit backup restore <backup-branch>
kit backup cleanup --dry-run
kit backup cleanup
kit backup cleanup --all
```

정상 sync backup은 의도적으로 남아 복구 지점으로 사용할 수 있다.

중단 상태 점검:

```sh
kit doctor --recovery
```

다음을 검사한다.

- interrupted kit pick state
- Git `CHERRY_PICK_HEAD`
- `kit/recovery/*`
- `kit/tmp/*`
- stale Kit-created branch marker
- saved review state

정상 `kit/backup/*`는 문제로 간주하지 않는다.

## doctor / verbose

기본 local 진단:

```sh
kit doctor
```

remote Git과 Gitea API까지 read-only 진단:

```sh
kit doctor --network
```

`--network`는 `git ls-remote`로 upstream stable/base를 확인하고 local-only source branch가 upstream remote에 없는지 검사한 뒤 Gitea target repository API를 ping한다. `git.push-remote`가 설정된 경우 fork source/target topology도 검사한다. fetch로 local tracking ref를 변경하지 않는다.

실행 command를 진단해야 할 때:

```sh
kit --verbose sync
kit doctor --network --verbose
```

`--verbose`는 Git/API 진단 단계를 stderr에 출력한다. token, URL credential, secret query 값은 redaction한다.

## worktree

```sh
kit worktree list
kit worktree add feat/login
kit worktree add feat/new ../kit-feat-new --create --base develop
kit worktree remove ../kit-feat-new
kit worktree prune
```

`worktree remove`는 branch를 삭제하지 않는다. 새 branch 생성은 `--create`를 명시해야 한다.

## branch-clean

기본은 dry-run이다.

```sh
kit branch-clean
kit branch-clean --json
```

실제 삭제:

```sh
kit branch-clean --delete
kit branch-clean --delete --yes --json
```

대상은 Kit-created marker가 있는 local review branch로 제한된다. 다음은 항상 보존한다.

- configured stable/base/source
- 현재 checkout branch
- 다른 worktree가 사용 중인 branch
- `kit/backup/*`, `kit/recovery/*`, `kit/tmp/*`
- base에 반영되지 않은 commit이 남은 branch
- 안전하게 판정하기 어려운 review-side merge history

remote branch는 이 명령이 삭제하지 않는다.

## port / process

port를 사용 중인 local process 조회:

```sh
kit port 3000
kit port 3000 --json
```

TCP listener와 UDP binding을 보여준다.

해당 port를 사용하는 process에 signal 전송:

```sh
kit port kill 3000
kit port kill 3000 --signal KILL --yes
```

PID 직접 조회/종료:

```sh
kit process 1234
kit process 1234 --json
kit process kill 1234 --signal TERM
```

지원 signal은 `TERM`, `KILL`, `INT`, `HUP`, `QUIT`이며 기본은 `TERM`이다. PID 1 이하와 현재 실행 중인 kit process에는 signal을 보내지 않는다. mutation JSON은 `--yes`가 필요하다.

`port`/`process`는 Git repository 밖에서도 사용할 수 있다.

## update / rollback

최신 version 확인만:

```sh
kit update --check
```

업데이트:

```sh
kit update
```

다운로드 binary는 checksum과 embedded build metadata를 검증한 뒤 설치한다. 실제 교체 직전에 기존 실행 파일을 `<kit>.previous`로 보존한다.

직전 설치본으로 rollback:

```sh
kit update --rollback
```

rollback은 network를 사용하지 않고 previous binary 자체를 실행해 version/commit/target metadata를 검증한 뒤 transactional swap한다. symlink나 special file은 previous binary로 인정하지 않는다.

## JSON / 자동화 계약

지원되는 command에서는 `--json`으로 machine-readable 결과를 받을 수 있다. 상태를 변경하는 JSON 명령은 원칙적으로 `--yes`를 요구한다.

대표적인 자동화 예:

```sh
kit compare --json
kit pick feat/bot --commit <sha> --yes --json
kit review list --refresh --json
kit review finish feat/bot --yes --json
kit doctor --network --json
kit doctor --recovery --json
kit port 3000 --json
kit process 1234 --json
kit update --check --json
```

JSON/CI/pipe 실행에서는 macOS Keychain unlock prompt를 자동으로 띄우지 않는다. Keychain이 잠겨 있으면 interactive terminal에서 먼저 해제한 뒤 재실행한다.

## Gitea / HTTP 보안

canonical review provider는 Gitea다.

```sh
kit config set git.provider gitea
```

사설 literal IP의 HTTP Gitea가 불가피한 경우에만 repository별로 명시 허용할 수 있다.

```sh
kit config set git.allow-insecure-http true
```

이 경우 API token이 평문 네트워크로 전송되므로 HTTPS reverse proxy 사용을 우선한다. kit는 HTTPS에서 HTTP로 자동 downgrade하지 않는다.

`KIT_GITEA_TOKEN`과 `KIT_GITEA_HOST` 환경 override는 반드시 함께 설정해야 한다. Git push credential과 Gitea API token은 서로 별개다. fork workflow에서도 review API credential은 upstream target host에 바인딩되고 Git push credential은 `git.push-remote`에 사용된다.

## 개발 / CI

Go 1.23 이상이 필요하다.

```sh
make check
make build
./bin/kit version
```

`make check`는 다음을 포함한다.

- gofmt
- `go vet ./...`
- `go test ./...`
- shell syntax checks
- production `deploy/ssh-wrapper.sh` forced-command rejection black-box tests

GitHub Verify는:

- `ubuntu-24.04`: `make check`
- `macos-15`: 실제 `darwin/arm64` 확인 + Go test/native build/실행

을 수행한다.

## 배포

- GitHub upstream `main`/`develop`은 Gitea로 mirror된다.
- `main` push는 docs/site 배포 경로의 기준이다.
- `origin/main`에 포함된 `vX.Y.Z` tag만 release 대상이다.
- release binary는 checksum/build metadata를 검증한다.
- deploy SSH key는 forced-command wrapper를 통과하며 허용 operation/identifier만 수신한다.

서버 구성과 forced-command/activator 상세는 [deploy/README.md](deploy/README.md)를 참고한다.

## 호환 namespace

기존 `kit git ...`, `kit self ...` namespace는 기존 사용자와 automation 호환을 위해 유지한다. 새 workflow에서는 top-level command를 우선 사용한다.

현재 architecture와 safety boundary는 [docs/kit-architecture.md](docs/kit-architecture.md)에 정리되어 있다.
