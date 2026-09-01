# kit — 현재 Architecture와 Safety Contract

이 문서는 `kit`의 현재 구현을 설명한다. 초기 roadmap나 미래 후보가 아니라 지금 코드에서 유지해야 하는 boundary와 invariant를 기준으로 한다.

## 1. 목적

`kit`은 범용 Git hosting CLI가 아니다. 다음 local workflow를 반복 가능하고 복구 가능하게 만드는 개인용 Go CLI다.

```text
main     stable / release 기준
develop  integration base
work     local-only commit queue
feat/*   일회성 review branch
```

권장 흐름:

```text
compare → pick → Gitea review/merge → review finish
```

`sync`는 provider lifecycle과 독립적인 Git-only queue 동기화 명령으로 유지한다.

## 2. 공식 지원 target

| OS | Architecture | Go target | CI |
|---|---|---|---|
| macOS | Apple Silicon | `darwin/arm64` | GitHub `macos-15` native runtime |
| Ubuntu 24.04 | x86-64 | `linux/amd64` | GitHub `ubuntu-24.04` |

macOS CI는 `go env GOOS=darwin`, `GOARCH=arm64`, `uname -m=arm64`를 확인한 뒤 Go test/native build/실행을 수행한다.

## 3. 주요 package boundary

```text
cmd/kit
  └─ process entrypoint / signal / exit code

internal/app
  ├─ CLI parsing and orchestration
  ├─ compare / pick / sync / review
  ├─ config / auth / doctor / update
  ├─ worktree / branch-clean extensions
  ├─ port / process extensions
  └─ user-facing rendering and mutation confirmation

internal/git
  ├─ system Git command execution
  ├─ branch/ref/config helpers
  ├─ applied detection
  ├─ worktree / cleanup helpers
  ├─ backup/recovery naming
  └─ sanitized tracing / typed command errors

internal/workflow
  └─ complex Git-only sync/rebuild/recovery algorithm

internal/review
  ├─ Gitea API adapter
  ├─ same-repository + Gitea fork review capabilities
  ├─ compatibility GitLab/Forgejo mapping
  ├─ secure HTTP client
  └─ review create/find/get/ping contracts

internal/reviewstate
  └─ local persisted provider/review lifecycle state

internal/pickstate
  └─ interrupted pick continuation checkpoint

internal/auth
  └─ Gitea credential storage and profile metadata

internal/update
  └─ release metadata, download verification, install/rollback

internal/procutil
  └─ local port/process inspection and Unix signal safety
```

`Application.Run`의 기존 contract는 유지하고, 이후 추가된 developer command/option은 `RunCLI` extension router를 사용한다. 이는 이미 큰 orchestration 파일을 더 키우지 않으면서 기존 embedding/test contract를 보존하기 위한 구조다.

## 4. Git execution boundary

runtime Git 작업은 shell string interpolation이 아니라 `exec.CommandContext("git", args...)` 형태로 수행한다.

보안/진단 contract:

- `KIT_*_TOKEN`은 Git child environment에서 제거
- HTTPS URL credential redaction
- `token`, `password`, `access_token`, `private_token` query redaction
- command argument에 있던 secret을 stderr가 되풀이해도 추가 redaction
- Git failure는 typed `CommandError`로 sanitized args와 exit code를 보존
- expected absence와 command failure를 exit code로 구분

`--verbose`는 sanitized Git command/API diagnostic 단계만 stderr에 출력한다. request body나 credential은 출력하지 않는다.

## 5. Workflow configuration

repository-local 기본값:

```text
git.provider = gitea
git.remote   = origin
git.stable   = main
git.base     = develop
git.source   = work
```

optional fork workflow:

```text
git.remote       = upstream   # base fetch/sync + review target
git.push-remote  = origin     # fork review branch push source
```

`git.push-remote`가 설정되지 않으면 `git.remote`로 fallback한다. 따라서 기존 same-repository workflow는 변경되지 않는다.

`kit config init`은 기본 설정을 기록하며 optional `git.push-remote`를 임의로 만들지 않는다.

`kit config bootstrap`은 새 clone bootstrap을 담당한다.

- `git.remote` fetch
- remote stable/base 존재 확인
- missing local stable/base 생성
- missing source를 base에서 생성
- existing source는 덮어쓰지 않음
- remote source 존재 시 local-only queue 계약 위반으로 중단

config missing은 `ErrConfigNotSet`으로 실제 Git failure와 구분한다.

## 6. Pending/applied 판정

source queue commit이 base에 반영됐는지는 다음 순서로 판단한다.

1. base commit message의 `cherry picked from commit <sha>` (`git cherry-pick -x`)
2. stable patch-id

성능 contract:

- 모든 candidate가 `-x`로 확인되면 patch scan 생략
- base patch history는 `git log ... -p | git patch-id --stable` streaming pipeline
- candidate patch는 하나의 batch `git show`와 patch-id call로 계산

`compare`, 일반 `sync`는 provider metadata를 사용하지 않는다.

## 7. Pick transaction

`kit pick <branch>`는:

1. clean tree 확인
2. remote/base/source freshness 확인
3. pending commit 계산
4. interactive / `--all` / repeated `--commit` selection
5. source의 original pending order로 normalize
6. pick state 저장
7. base에서 target branch 생성 + Kit-created marker
8. commit별 `cherry-pick -x`
9. configured review push remote로 push
10. Gitea PR create-or-reuse
11. reviewstate 저장

`--dry-run`은 branch/ref를 변경하지 않는다.

noninteractive JSON mutation은 `--yes`가 필요하다.

conflict 발생 시 pickstate를 남기며:

```text
kit pick --continue
kit pick --skip
kit pick --abort
```

으로 process 종료 이후에도 재개할 수 있다.

push 시작 전 review initialization이 실패하면 original checkout으로 rollback하고 생성 branch/marker/state를 제거한다. push 시작 후에는 remote 결과가 불확실할 수 있으므로 local/remote branch를 보존한다.

## 8. Review lifecycle

reviewstate는 다음을 저장한다.

- provider / published push remote
- source review branch / target base
- PR number / URL / status
- original source commit hashes
- picked/published tip
- merge SHA / timestamps
- lifecycle stage

### same-repository와 fork model

기본 same-repository flow에서는 `git.remote`가 base sync, branch push, PR target을 모두 담당한다.

Gitea fork flow에서는:

```text
git.remote       upstream target repository
git.push-remote  fork source repository
```

로 역할을 분리한다.

cross-repository review 안전 조건:

- target/source provider가 모두 Gitea
- 두 remote가 같은 Gitea host
- source/target repository coordinate를 URL에서 해석 가능
- provider client와 token은 upstream target repository에 바인딩
- branch push/upstream/cleanup은 fork push remote에 바인딩

Gitea create PR의 source는 `<fork-owner>:<branch>` 형태로 전달한다.

saved review state의 `Remote`는 실제 published push remote를 저장한다. 이후 config의 push remote가 바뀌면 status/add/finish mutation은 중단한다. saved PR URL도 해석 가능한 target repository URL과 일치해야 provider refresh를 수행한다.

### submit

`review submit`은 configured push remote로 push한 뒤 동일 source/base의 open PR을 먼저 찾는다. fork mode에서는 fork owner까지 함께 일치해야 재사용한다. 존재하면 재사용하고 없으면 create한다. 따라서 API timeout 후 재실행이 idempotent한 방향으로 동작한다.

### add

`review add`는 이미 열린 Kit-managed PR에 pending work commit을 추가한다.

반드시 검증하는 조건:

- provider PR `open`
- Kit-created marker
- clean tree
- correct push-remote upstream
- local tip == push remote tip == provider source SHA == saved PublishedTip
- upstream base/source freshness

추가 cherry-pick 중 실패하면 시작 전 branch tip으로 rollback한다. fork mode에서도 push 성공 후에만 state를 갱신한다.

### status/list

`review status`는 단일 saved review를 provider에서 refresh한다.

`review list --refresh`는 active saved review를 provider에서 일괄 refresh한다.

### finish

`review finish`는 provider가 `merged`를 반환한 경우에만 mutation을 수행한다.

```text
provider merged 확인
  ↓
upstream base-only fast-forward
  ↓
provider-confirmed source commits reconcile
  ↓
normal Git-only sync
  ↓
managed local + published push-remote review branch exact-tip cleanup
```

이 provider-aware reconcile은 squash merge를 안전하게 처리하기 위한 별도 boundary다. 일반 `sync`의 Git-only contract는 유지된다.

branch cleanup은 saved PublishedTip과 현재 local/push-remote tip이 정확히 일치할 때만 수행한다. fork mode에서 upstream repository의 같은 이름 branch는 cleanup 대상이 아니다.

자동화:

```sh
kit review finish <branch> --yes --json
```

## 9. Sync / rebuild invariant

`kit sync`의 핵심 원칙은 **완료 전까지 original work를 잃지 않는다**이다.

일반 흐름:

1. clean tree 확인
2. configured `git.remote` fetch/prune
3. base remote relation 검증
4. original base/work/checkout hash 기록
5. base fast-forward
6. original work backup 생성
7. temporary branch에서 latest base + pending first-parent commit rebuild
8. 성공한 경우에만 source ref 이동
9. original checkout 복원

실패 시:

- in-progress cherry-pick abort
- original source/base ref 복원
- original checkout 복원
- hash 검증
- 필요 시 partial resolved commits를 `kit/recovery/*`에 보존

자동 backup ref는 source ownership을 포함한 namespace를 사용한다.

```text
kit/backup/v2/<source-sha256>/<kind>/<id>
```

정상 backup은 recovery point이므로 자동 문제로 취급하지 않는다.

## 10. Recovery diagnostics

```sh
kit doctor --recovery
```

검사 항목:

- pickstate 존재
- `CHERRY_PICK_HEAD`
- `kit/recovery/*`
- `kit/tmp/*`
- stale Kit-created marker
- saved/active reviewstate count
- retained backup count (informational)

`--network`과 `--recovery`는 각각 network health와 local interrupted state라는 다른 boundary를 검사하므로 별도 실행한다.

## 11. Network diagnostics

```sh
kit doctor --network
```

read-only 원칙:

- `git ls-remote`로 upstream stable/base 조회
- upstream source absence 확인
- local remote-tracking ref를 변경하지 않음
- Gitea authenticated target repository API ping
- `git.push-remote`가 있으면 source/target Gitea topology 검증

기본 `kit doctor`는 network를 사용하지 않는다.

## 12. Credential / HTTP boundary

canonical provider는 Gitea다.

credential 우선순위:

1. `KIT_GITEA_TOKEN` + exact `KIT_GITEA_HOST` pair
2. stored credential

둘 중 environment 변수 하나만 존재하면 실패한다.

stored credential:

- macOS: Keychain
- Ubuntu: Secret Service
- explicit fallback: permission-restricted local file

review API origin은 target repository origin과 exact match해야 하며 redirect도 같은 origin만 허용한다.

HTTP는 다음 조건을 모두 만족하는 Gitea에만 명시 허용한다.

- repository remote 자체가 HTTP
- `git.allow-insecure-http=true`
- literal private/loopback/link-local IP

public IP/hostname HTTP는 허용하지 않는다.

JSON/CI/non-TTY에서는 macOS Keychain interactive unlock prompt를 자동으로 열지 않는다.

## 13. Worktree / branch cleanup

`kit worktree`는 native Git worktree를 관리한다.

- existing local branch attach
- `--create --base`로 명시적 새 branch 생성
- remove는 worktree만 제거하고 branch 보존
- prune은 stale administrative data 정리

`kit branch-clean`은 기본 dry-run이다.

삭제 후보는 Kit-created local branch 중:

- base의 ancestor이거나
- branch의 모든 direct first-parent nonmerge commit이 base에 applied

인 경우다.

보호 대상:

- stable/base/source
- current branch
- any worktree checkout branch
- backup/recovery/tmp namespace
- unapplied commit branch
- review-side merge 때문에 안전하게 분류할 수 없는 branch

remote branch는 `branch-clean`이 삭제하지 않는다. provider-confirmed remote review branch cleanup은 `review finish`에서만 수행한다.

## 14. Port / process developer utility

`kit port`는 `lsof`를 사용해 local TCP listener와 UDP binding을 조회한다.

```sh
kit port 3000
kit port kill 3000 --signal TERM
```

`kit process`는 `ps`를 사용해 PID/PPID/user/elapsed/command를 조회하고 명시적 signal mutation을 수행한다.

```sh
kit process 1234
kit process kill 1234 --signal TERM
```

mutation contract:

- 기본 signal은 `SIGTERM`
- TERM/KILL/INT/HUP/QUIT만 허용
- PID <= 1 거부
- 현재 실행 중인 kit process 거부
- signal 전에 target 존재/권한 preflight
- JSON mutation은 `--yes` 필수

이 developer utility들은 Git repository 외부에서도 동작한다. port 조회에는 `lsof`가 필요하며 macOS에는 기본 제공되고 Ubuntu에서는 별도 설치가 필요할 수 있다.

## 15. Self-update / rollback

production update sequence:

1. HTTPS release metadata fetch
2. schema/version/commit/build/asset origin validation
3. binary download with size limit
4. SHA-256 verification
5. downloaded binary `version --json` execution and build metadata comparison
6. current installed binary를 `<kit>.previous`에 보존
7. atomic replacement
8. installed binary metadata 재검증

`kit update --check`는 metadata만 읽고 filesystem을 변경하지 않는다.

`kit update --rollback`은 network 없이 previous binary를 실행·검증한 뒤 current/previous를 transactional swap한다. symlink/special file은 rollback source로 거부한다.

## 16. Deploy trust boundary

GitHub가 upstream source이고 main/develop push는 Gitea로 mirror된다.

release/deploy에서 중요한 boundary:

- GitHub/Gitea action dependency SHA pinning
- tag가 `origin/main` ancestry 안에 있어야 release 가능
- build checksum + release metadata 생성
- SSH host key strict verification
- manual/Gitea deploy key separation
- dedicated deploy account
- forced-command wrapper
- fixed root-owned activator path
- archive size/time limits
- upload lock

`deploy/ssh-wrapper.sh`는 malformed `SSH_ORIGINAL_COMMAND`를 filesystem/activator 접근 전에 검증한다.

Linux CI는 production wrapper를 black-box 실행해 identity, token count, operation allowlist, SHA/SemVer validation, shell/traversal 입력 거부를 검증한다.

privileged activator의 실제 `/srv` atomic activation/rollback은 서버 trust boundary 안에 있으며 현재 GitHub unprivileged CI에서 end-to-end로 실행하지 않는다.

## 17. CI

GitHub Verify:

### Ubuntu

```text
gofmt
go vet ./...
go test ./...
shell syntax
forced-command rejection integration tests
```

### macOS Apple Silicon

```text
GOOS=darwin
GOARCH=arm64
uname -m=arm64
gofmt
go vet ./...
go test ./...
native build
kit version --json execution
```

Gitea mirror 성공 여부도 GitHub workflow에서 확인한다.

## 18. Compatibility

다음은 compatibility namespace로 유지한다.

- `kit git ...`
- `kit self ...`
- legacy reviewstate provider values (`gitlab`, `forgejo`) 읽기
- legacy `work` backup namespace는 ownership을 증명할 수 있는 기본 `work`에 한해 제한적으로 처리
- `git.push-remote`가 없는 기존 repository는 `git.remote`를 push remote로 사용

새 기능과 문서는 top-level command와 Gitea canonical workflow를 기준으로 한다.

## 19. 현재 의도적으로 남겨 둔 범위

다음은 현재 core workflow 밖의 후속 범위다.

- privileged deploy activator의 실제 `/srv` CI end-to-end fixture
- GitHub repository branch protection/ruleset의 자동 설정
- 서로 다른 host 또는 GitLab/Forgejo의 cross-repository review

이 영역을 추가하더라도 Git-only sync, provider-confirmed review mutation, exact-tip cleanup, original-work rollback invariant를 깨지 않는 것이 우선이다.
