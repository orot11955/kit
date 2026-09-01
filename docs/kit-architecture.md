# kit — 현재 Architecture와 Safety Contract

이 문서는 `kit`의 **현재 구현**과 유지해야 할 safety invariant를 설명한다. 완료된 roadmap 항목을 미래 범위처럼 적지 않고, 실제 코드와 CI가 보장하는 동작을 기준으로 한다.

## 1. 목적과 workflow

`kit`은 범용 Git hosting CLI가 아니라 다음 개인 개발 workflow를 반복 가능하고 복구 가능하게 만드는 Go CLI다.

```text
main     stable / release
develop  integration base
work     local-only commit queue
feat/*   review branch
```

권장 흐름:

```text
kit compare
  ↓
kit pick <review-branch>
  ↓
Gitea review / merge
  ↓
kit review finish <review-branch>
```

`kit sync`는 provider lifecycle과 분리된 Git-only queue synchronization 명령으로 유지한다.

## 2. 공식 지원 target

| OS | Architecture | CI |
|---|---|---|
| macOS | Apple Silicon (`darwin/arm64`) | GitHub `macos-15` native runtime |
| Ubuntu 24.04 | x86-64 (`linux/amd64`) | GitHub `ubuntu-24.04` |

macOS CI는 `GOOS=darwin`, `GOARCH=arm64`, `uname -m=arm64`를 확인한 뒤 test/native build/binary execution을 수행한다.

## 3. package boundary

```text
cmd/kit
  └─ process entrypoint / signal / exit code

internal/app
  ├─ CLI parsing and orchestration
  ├─ compare / pick / sync / review
  ├─ config / auth / doctor / update
  ├─ worktree / branch-clean
  ├─ port / process
  └─ rendering / confirmation / JSON contract

internal/git
  ├─ system Git execution
  ├─ refs / branches / config / worktree
  ├─ pending/applied classification
  ├─ backup/recovery helpers
  └─ typed sanitized command errors

internal/workflow
  └─ Git-only sync / rebuild / rollback transaction

internal/review
  ├─ canonical Gitea API adapter
  ├─ same-repository review
  ├─ Gitea fork → upstream review
  ├─ legacy GitLab / Forgejo compatibility
  └─ secure HTTP client / create-find-get-ping contracts

internal/reviewstate
  └─ persisted review lifecycle checkpoint

internal/pickstate
  └─ interrupted pick checkpoint

internal/auth
  └─ Gitea credential storage

internal/update
  └─ release verification / self-update / rollback

internal/procutil
  └─ local port/process inspection and signal safety
```

기존 `Application.Run` embedding contract는 유지하고 이후 command extension은 `RunCLI` routing으로 분리한다.

## 4. Git execution / secret boundary

runtime Git은 shell command string 조립이 아니라 argument array 기반 subprocess로 실행한다.

유지해야 할 contract:

- `KIT_*_TOKEN`을 Git child environment에서 제거
- URL credential / secret query redaction
- stderr가 command argument의 secret을 되풀이해도 추가 redaction
- Git failure는 sanitized args와 exit code를 가진 typed error로 변환
- expected absence와 실제 command failure를 구분
- `--verbose`는 sanitized diagnostic만 stderr에 기록

## 5. Repository workflow configuration

기본값:

```text
git.provider = gitea
git.remote   = origin
git.stable   = main
git.base     = develop
git.source   = work
```

fork review를 사용할 때만 optional push remote를 설정한다.

```text
git.remote       = upstream   # base fetch/sync + PR target
git.push-remote  = origin     # fork review branch push source
```

`git.push-remote`가 없으면 `git.remote`로 fallback하므로 기존 same-repository workflow는 동일하다.

`kit config bootstrap`은 configured `git.remote`를 기준으로 stable/base를 만들고 `work`를 local-only queue로 유지한다. remote `work`가 존재하면 contract 위반으로 중단한다.

## 6. Pending / applied classification

`work` commit의 반영 여부는 다음 순서로 판단한다.

1. `git cherry-pick -x`가 남긴 original commit hash
2. stable patch-id

성능 contract:

- `-x`만으로 모든 candidate를 판정하면 patch scan 생략
- base patch history는 streaming `git log -p | git patch-id --stable`
- candidate patch-id는 batch 계산

일반 `compare`와 `sync`는 provider metadata를 사용하지 않는다.

## 7. Pick transaction

`kit pick <branch>`의 mutation 순서:

1. clean tree 확인
2. base/source/remote freshness 확인
3. pending commit 계산
4. interactive / `--all` / repeated `--commit` 선택
5. source의 original order로 normalize
6. pickstate checkpoint 저장
7. base에서 review branch 생성 + Kit-created marker
8. commit별 `cherry-pick -x`
9. configured push remote로 push
10. Gitea PR create-or-reuse
11. reviewstate 저장

중간 conflict는 process 종료 이후에도 다음 명령으로 재개 가능하다.

```sh
kit pick --continue
kit pick --skip
kit pick --abort
```

push 전 초기화 실패는 branch/marker/state와 checkout을 rollback한다. push가 시작된 뒤 결과가 불확실한 경우 local/remote state를 보존해 재시도 시 확인 가능하게 한다.

## 8. Review lifecycle와 fork model

reviewstate는 provider, published push remote, review/base branch, PR metadata, original source commits, picked/published tip, merge SHA와 lifecycle timestamps를 저장한다.

### Same repository

`git.remote`가 base sync, review branch push, PR target을 모두 담당한다.

### Gitea fork → upstream

```text
git.remote       upstream target
git.push-remote  fork source
```

안전 조건:

- source/target 모두 Gitea
- 동일 Gitea host
- repository coordinate를 remote URL에서 안전하게 해석 가능
- API token은 upstream target origin에 바인딩
- branch upstream/push/cleanup은 fork source remote에 바인딩
- PR head는 `<fork-owner>:<branch>`로 생성

open PR 재사용 시 fork owner/source/base까지 일치해야 한다.

saved push remote가 현재 config와 달라지면 `review add`/`finish` cleanup mutation을 거부한다. fork mode의 branch cleanup은 saved published tip과 fork remote tip이 정확히 일치할 때만 수행하며 upstream의 같은 이름 branch는 삭제하지 않는다.

### finish invariant

```text
provider merged 확인
  ↓
upstream base fast-forward
  ↓
provider-confirmed source commit reconcile
  ↓
normal Git-only sync
  ↓
managed local + published push-remote exact-tip cleanup
```

squash merge reconcile은 review lifecycle 안에서만 수행하며 일반 `kit sync`에는 provider dependency를 넣지 않는다.

## 9. Sync / recovery invariant

핵심 원칙은 **완료 전까지 original work를 잃지 않는다**이다.

일반 sync:

1. clean tree 확인
2. configured remote fetch/prune
3. base relation 검증
4. original base/work/checkout hash 기록
5. base fast-forward
6. original work backup 생성
7. temporary branch에서 latest base + pending first-parent commit rebuild
8. 성공 시에만 source ref 이동
9. original checkout 복원

실패 시 cherry-pick abort, source/base ref 복원, checkout 복원, hash 검증을 수행한다. 일부 conflict resolution commit이 생긴 뒤 실패한 경우 `kit/recovery/*`에 보존한다.

backup namespace:

```text
kit/backup/v2/<source-sha256>/<kind>/<id>
```

정상 backup은 recovery point이므로 `doctor --recovery`의 오류로 취급하지 않는다.

## 10. Doctor / recovery / network

```sh
kit doctor
kit doctor --network
kit doctor --recovery
```

`--network`는 local tracking ref를 변경하지 않고 `ls-remote`와 provider API로 stable/base/source 및 Gitea target을 검사한다. `git.push-remote`가 있으면 source/target fork topology도 검증한다.

`--recovery`는 pickstate, `CHERRY_PICK_HEAD`, `kit/recovery/*`, `kit/tmp/*`, stale Kit marker, saved reviewstate를 검사한다.

## 11. Credential / HTTP boundary

Gitea credential 우선순위:

1. `KIT_GITEA_TOKEN` + exact `KIT_GITEA_HOST`
2. stored credential

stored credential 기본 backend:

- macOS: Keychain
- Ubuntu: Secret Service
- explicit fallback: permission-restricted local file

review API는 repository target origin과 exact match해야 하며 redirect도 같은 origin으로 제한한다.

plain HTTP는 다음 조건을 모두 만족하는 Gitea만 명시 허용한다.

- repository remote 자체가 HTTP
- `git.allow-insecure-http=true`
- literal private / loopback / link-local IP

## 12. Worktree / local branch cleanup

`kit worktree`는 native Git worktree를 관리한다. `remove`는 worktree만 제거하고 branch를 자동 삭제하지 않는다.

`kit branch-clean`은 기본 dry-run이며 Kit-created local review branch만 대상으로 한다.

항상 보존하는 대상:

- stable/base/source
- current branch
- 다른 worktree가 사용 중인 branch
- backup/recovery/tmp namespace
- unapplied commit이 남은 branch
- merge topology 때문에 안전 판정이 불가능한 branch

remote review branch 삭제는 provider-confirmed `review finish`에서만 수행한다.

## 13. Port / process developer utility

```sh
kit port 3000
kit port kill 3000 --signal TERM
kit process 1234
kit process kill 1234 --signal TERM
```

mutation safety:

- 기본 `SIGTERM`
- TERM/KILL/INT/HUP/QUIT만 허용
- PID <= 1 거부
- 현재 kit process 거부
- signal 전 존재/권한 preflight
- JSON mutation은 `--yes` 필수

Git repository 외부에서도 사용할 수 있다.

## 14. Self-update / rollback

production update는 release metadata → asset origin → SHA-256 → downloaded binary build metadata를 모두 검증한 뒤 설치한다.

실제 교체 직전에 current binary를 `<kit>.previous`로 보존하고 설치 후 metadata를 다시 검증한다.

`kit update --rollback`은 network 없이 previous binary를 실행·검증한 뒤 transactional swap한다. symlink/special file은 rollback source로 거부한다.

## 15. Deploy trust boundary

공식 배포 경로:

```text
forced-command SSH
  ↓
deploy/ssh-wrapper.sh
  ↓
/srv/data/apps/kit/data/incoming
  ↓
root-owned /usr/local/libexec/kit-activate
  ↓
origin byte verification
  ↓
atomic current-site / current-release symlink
```

중요한 boundary:

- manual / Gitea deploy key 분리
- dedicated `kit-deploy` account
- forced-command operation + identifier allowlist
- upload byte/time limit
- `.upload.lock` / `.deploy.lock`
- fixed root-owned activator path
- archive traversal/link/special-file 거부
- release checksum/metadata 검증
- origin response와 activated file byte 비교
- rollback도 같은 lock/order와 exact target 검증 사용

### Privileged production-path CI

Ubuntu GitHub Verify는 ephemeral runner에서 실제 production constant를 사용한다.

```text
/srv/data/apps/kit/data
/etc/kit/deploy.env
/usr/local/libexec/kit-activate
kit-deploy system user
```

fixture는 `GITHUB_ACTIONS=true`, Linux, root 조건이 아니면 실행을 거부한다.

검증 범위:

- forced-command wrapper의 stdin archive upload
- validated identifier가 activator까지 전달되는지
- site activation → second activation → rollback
- site origin mismatch 자동 rollback + 신규 destination 제거
- traversal archive 거부
- release v1 → v2 activation
- release rollback
- release metadata mismatch rollback
- archive consumption / symlink target / ownership postcondition

`deploy/activate.sh`에는 CI 전용 path override나 test mode를 넣지 않는다.

### 알려진 activator compatibility debt

`activate.sh`의 historical `site|release` dispatch는 site activation 자체가 성공한 뒤 마지막 false release predicate 때문에 direct invocation에서 exit status `1`이 될 수 있다. production SSH wrapper는 이 상태를 일반적으로 무시하지 않는다.

다음 조건을 **모두** 만족하는 정확한 site success postcondition일 때만 status `1`을 정상화한다.

- activator status가 정확히 `1`
- incoming archive가 소비됨
- `current-site`가 정확히 `sites/<validated-id>`를 가리킴
- 해당 destination directory가 존재하며 symlink가 아님

그 외 activator failure는 원래 exit status를 그대로 전파한다. direct activator dispatch 자체를 정리하는 것은 별도 기술부채로 남아 있다.

## 16. GitHub / Gitea CI

GitHub Verify의 Ubuntu job:

```text
gofmt
go vet ./...
go test ./...
shell syntax
forced-command rejection tests
GitHub protection fake-API regression tests
privileged /srv deployment integration
```

macOS Apple Silicon job:

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

GitHub `main`/`develop` push는 별도 workflow로 Gitea에 mirror하며 성공 여부를 GitHub Actions에서 확인한다.

## 17. GitHub branch protection desired state

GitHub upstream protection policy는 다음 파일이 source of truth다.

```text
.github/protection/main.json
.github/protection/develop.json
```

운영 도구:

```sh
scripts/github-protection.sh --check
scripts/github-protection.sh --apply
```

정책 요약:

- `main`: PR 경로 필수, approving review count 0, Linux/macOS checks 필수, admin enforcement, conversation resolution, force/delete 차단
- `develop`: Linux/macOS checks 필수, admin enforcement, force/delete 차단; main merge SHA의 verified fast-forward를 허용하기 위해 PR-only는 사용하지 않음

`--apply`는 Administration read/write credential이 필요한 명시적 maintainer operation이다. 높은 권한 token은 repository CI에 자동 주입하지 않는다.

스크립트는 token을 curl argv에 넣지 않고 mode `0600` temporary header file로 전달하며 PUT 후 GET read-back을 검증한다.

CI는 실제 repository settings를 변경하지 않고 fake GitHub API로 다음을 검증한다.

- matching state check
- drift detection
- main/develop PUT
- apply 후 read-back GET

main merge 후 develop을 새 main SHA로 맞출 때는 **해당 main SHA의 push Verify가 성공한 뒤** fast-forward한다.

실제 운영 절차는 [github-protection.md](./github-protection.md)를 따른다.

## 18. Compatibility

다음 compatibility는 유지한다.

- `kit git ...`
- `kit self ...`
- legacy reviewstate의 GitLab / Forgejo provider 값 읽기
- ownership을 증명할 수 있는 legacy `work` backup namespace 처리
- `git.push-remote`가 없는 repository는 `git.remote`를 push remote로 사용

새 기능과 문서는 top-level command와 Gitea canonical workflow를 기준으로 한다.

## 19. 현재 남은 범위

현재 core daily workflow, fork review, native CI, privileged deploy fixture, branch-protection desired-state 자동화는 구현되어 있다.

남은 항목은 다음과 같다.

- Administration credential을 사용한 GitHub protection desired state의 **실제 repository settings 적용**
- `activate.sh` site-mode historical direct exit-status dispatch 정리
- 서로 다른 host 또는 GitLab/Forgejo의 cross-repository review 지원 여부 결정
- 큰 orchestration 파일(`internal/app`)의 추가 구조 분리는 기능 변경과 분리해 점진적으로 진행

이후 변경에서도 다음 invariant를 우선한다.

- Git-only sync
- provider-confirmed review mutation
- exact-tip branch cleanup
- original-work rollback safety
- secret/token non-disclosure
- production deploy path에 CI 전용 bypass를 넣지 않음
