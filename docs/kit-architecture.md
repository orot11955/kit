# kit — 개발 및 배포 계획

> macOS Apple Silicon과 Ubuntu에서 자주 쓰는 shell 명령을 하나의 Go CLI로 편하게 실행한다.

---

# 1. 목표

`kit`은 복잡한 범용 개발 환경 관리 도구가 아니다. 현재 사용 중인 shell 명령을
Go subcommand로 옮겨 다음 문제를 해결하는 개인용 CLI다.

- 여러 shell script를 따로 설치하고 이름을 기억해야 하는 문제
- macOS와 Ubuntu에서 조금씩 다른 실행 환경
- 새 기능을 추가할 때 argument, 출력, 오류 처리를 반복하는 문제
- 새 기기마다 binary를 직접 빌드하거나 복사해야 하는 문제
- 기존 설치본을 수동으로 교체해야 하는 문제

목표 사용자 흐름은 단순하게 유지한다.

```bash
# 최초 설치
curl -fsSL https://kit.2juho.com/install.sh | sh

# 설치 직후 현재 shell에서 사용
$HOME/.local/bin/kit version
$HOME/.local/bin/kit compare

# 새 terminal부터 PATH로 사용
kit compare
kit pick feat/login
kit pick feat/login --wait

# 이후 업데이트
kit update
```

---

# 2. 지원 대상

공식 지원 target은 다음 두 개다.

| OS | Architecture | Go target |
|---|---|---|
| macOS | Apple Silicon | `darwin/arm64` |
| Ubuntu 24.04 LTS | x86-64 | `linux/amd64` |

Intel Mac인 `darwin/amd64`는 빌드, 배포, 테스트 대상에서 제외한다.

Ubuntu ARM64와 다른 Linux distribution은 우연히 동작할 수 있지만 build, 배포, 테스트
대상이나 공식 지원으로 표현하지 않는다.

---

# 3. 명령 이름

기존 `hash`는 실제 동작을 설명하지 못하므로 변경하고, `pick`은 기존 사용성과 짧은
명령 이름을 유지한다.

| 기존 이름 | 새 이름 | 이유 |
|---|---|---|
| `ghash`, `kit hash` | `kit compare` | hash 생성이 아니라 source와 base 브랜치의 커밋 반영 상태를 비교하는 명령이기 때문 |
| `gpick` | `kit pick` | 기존 이름을 유지하면서 `kit` namespace 아래로 통합 |

`kit hash`는 checksum 또는 Git object hash 조회로 오해할 수 있으므로 제공하지 않는다.
`kit pick`은 source의 미반영 commit을 선택하여 새 branch에 적용하는 기존 `gpick`
workflow로 의미를 고정한다.

## 3.1 `kit compare`

`work`와 같은 source 브랜치의 커밋이 `develop`과 같은 base 브랜치에 반영됐는지
확인하는 read-only 명령이다.

반영 여부는 다음 두 방법으로 판단한다.

1. `git cherry-pick -x`가 기록한 원본 commit hash
2. 동일 변경을 찾기 위한 stable patch-id

기본 사용:

```bash
kit compare
kit compare work
kit compare work --base develop
kit compare work --base main --limit 10
```

기본값:

```text
source = work
base   = develop
```

출력은 반영 완료와 미반영 commit을 구분한다.

```text
STAT  HASH      DATE              MESSAGE
✓     329ab12f  2026-08-14 10:30  applied commit
●     a137bc91  2026-08-14 11:20  pending commit
```

## 3.2 `kit pick`

source 브랜치의 미반영 commit을 대화형으로 선택하고, base에서 새 브랜치를 만든 뒤
선택한 commit을 source history의 위상 순서대로 `git cherry-pick -x`하는 명령이다.

```bash
kit pick feat/login
kit pick feat/login --from work
kit pick feat/login --from work --base develop
```

첫 번째 positional argument는 새로 만들 branch다. 이를 commit hash로 해석하지 않는다.

```text
source의 미반영 commit 조회
  → 사용자 선택
  → 실행 내용 확인
  → base에서 새 branch 생성
  → git cherry-pick -x
```

working tree가 dirty하거나 대상 branch가 이미 있으면 아무것도 변경하지 않고 실패한다.
conflict가 발생하면 다음 Git 동작을 제공한다.

```text
continue → git cherry-pick --continue
skip     → git cherry-pick --skip
abort    → git cherry-pick --abort
```

commit timestamp만으로 순서를 정렬하지 않는다. timestamp는 임의로 수정될 수 있고
commit dependency 순서를 보장하지 않기 때문이다. source revision walk에서 얻은 순서를
역순으로 적용하여 부모 쪽 commit부터 처리한다.

commit 선택은 Go binary에 포함된 full-screen fuzzy multi-select UI를 사용한다. 외부
`fzf` executable은 요구하지 않으며 설치 직후 `kit pick`을 사용할 수 있어야 한다.
Go TUI library는 build dependency로 고정하여 최종 binary에 포함한다.

기본 key 계약:

```text
문자 입력       fuzzy 검색
↑ / ↓, j / k   cursor 이동
space           선택 / 해제
enter           선택 확정
esc             취소
ctrl+c          interrupt (exit 130)
```

TTY가 아니면 full-screen UI를 시작하지 않고 명확한 오류를 반환한다. 자동화용 commit 지정
방식은 실제 필요가 생기면 별도 flag로 추가한다.

## 3.3 협업 Git workflow

branch 역할은 다음으로 고정한다.

```text
main     안정 version과 release tag 기준
develop  여러 개발자의 review branch가 합쳐지는 통합 branch
work     원격에 push하지 않는 local commit queue
feat/*   develop에서 만들어 Gitea PR로 올리는 일회성 branch
```

다른 개발자의 merge로 `develop`이 바뀌면 `work`에 단순 merge하지 않는다. merge conflict
해결이 merge commit에만 남으면 이후 개별 work commit을 뽑을 때 그 해결을 재사용할 수 없기
때문이다. `kit sync`는 `origin/develop`을 fast-forward한 뒤 기존 work backup을 만들고,
최신 develop에서 미반영 work commit만 위상 순서대로 재적용한다. 재구성 실패 시 기존 work,
원 checkout과 이번 실행 전 develop hash를 복원하고 모두 검증한다. 검증에 성공한 경우 그
실패 실행에서 생성한 자동 backup을 삭제하며, 복원 또는 삭제를 검증하지 못하면 backup을
남기고 정확한 이름을 오류에 포함한다. 재적용 충돌은 임시 branch에서 해결하며
`[c] continue`, `[s] skip`,
`[a] abort` 중 하나를 선택한다. continue 전에 충돌 파일을 수정하고 `git add`해야 한다.
외부 IDE에서 해결 commit까지 만든 경우에는 변경된 임시 branch HEAD를 감지해 continue로
처리한다.
외부 해결 commit 이후 다음 충돌이나 오류로 재구성이 중단되면 임시 HEAD를
`kit/recovery/*` branch로 보존한 뒤 원래 `work`와 checkout을 복원한다.
skip한 commit은 결과의 `skipped`로 표시되고 backup에는 남는다. 전체 적용이 끝나기 전에는
`work` branch를 이동하지 않는다.

```bash
kit status
kit status --cached
kit sync --dry-run
kit sync
kit backup list
kit backup restore <backup-branch>
kit backup cleanup --dry-run
kit backup cleanup [--all]
```

`cleanup`의 기본 대상은 현재 source branch에 대해 sync/refresh가 자동 생성한 backup이다.
`--all`은 같은 source의 manual backup과 restore 전 safety backup까지 포함한다. 삭제 전에는
목록과 확인을 제공하고 `--dry-run`은 아무 ref도 변경하지 않는다. `kit/recovery/*`와
`kit/tmp/*`는 이 명령의 대상이 아니다. restore가 실패한 경우에도 source ref와 원 checkout을
safety backup으로 복원·검증한 뒤 그 실패 실행에서 만든 safety backup만 삭제한다. 성공한
restore의 safety backup은 명시적으로 cleanup할 때까지 유지한다.

신규 backup ref는 `kit/backup/v2/<source-sha256>/<kind>/<id>` 형식으로 source 소유권과
종류를 분리한다. 구버전의 `/`→`-` 형식은 서로 다른 source를 구분할 수 없으므로 source가
정확히 `work`인 legacy ref만 list/restore/cleanup에서 호환한다. 다른 source의 legacy ref는
`git branch --list 'kit/backup/*'`로 읽기 전용 확인 후 운영자가 직접 판단한다.

`develop`이 `work`의 ancestor가 아니면 work는 `STALE`이다. `kit compare`는 경고와 상태를
표시하지만 read-only로 실행한다. 기본 `kit pick`은 원격 base를 fetch하고 동기화가
필요하면 `kit sync`를 먼저 실행한다. 사용자 지정 `--from`/`--base` 흐름에서는 branch
계약을 자동으로 추정하지 않고 `kit sync`를 안내한다. 장애 복구에만 `--allow-stale`을
명시적으로 사용할 수 있다.

`pick`은 commit을 하나씩 적용하고 `$(git rev-parse --git-path kit)/pick-state.json`에
진행 상태를 저장한다. process가 끝나도 다음 명령으로 이어갈 수 있다.

```bash
kit pick --continue
kit pick --skip
kit pick --abort
```

continue는 임의로 `git add -A`하지 않는다. 사용자가 해결한 파일을 직접 stage해야 한다.

## 3.4 Git hosting provider

local Git workflow는 server에 종속되지 않는다. 회사·협업 repository와 개인 repository,
kit 배포는 모두 Gitea를 사용한다. provider별 차이는 hosting adapter가 PR API 계약으로
흡수한다.

```bash
kit config init
kit config set git.provider gitea
```

repository local 설정은 `.git/config`의 `kit.git.*` key를 사용한다.
신규 repository의 기본 provider는 `gitea`다. `gitlab`·`forgejo` 값은 전환 중인 기존
설정을 읽고 같은 값으로 보존하는 경우에만 허용하며 새로 설정할 수 없다.

```text
git.provider = auto | gitea | generic
git.remote   = origin
git.stable   = main
git.base     = develop
git.source   = work
git.allow-insecure-http = false | true
```

`git.allow-insecure-http`의 기본값은 `false`다. `true`여도 remote URL에 `http://`가
명시되어 있고 host가 RFC1918, loopback 또는 link-local literal IP인 Gitea에만 적용한다.
DNS hostname, public IP, SSH/scp remote에는 적용하지 않으며 remote scheme을 보고 자동으로
downgrade하지 않는다. redirect와 API가 반환한 review URL도 최초 scheme과 `host[:port]`가
정확히 같아야 한다. HTTP를 사용하는 모든 review command는 token이 평문 전송된다는 경고를
stderr에 출력한다.

HTTP review URL은 기존 schema 1 state에 저장되지만 HTTPS-only 구버전 kit는 해당 state를
읽지 못한다. binary를 downgrade하기 전에는 진행 중인 HTTP review를 finish/cleaned 상태로
정리하거나 `.git/kit/reviews/`를 별도로 백업한다. 설정을 unset하거나 `false`로 바꾸면 새
API 요청은 즉시 HTTPS 기본값으로 돌아간다.

`kit git publish`는 `main`, `develop`, `work` push를 차단하고 현재 review branch만 push한다.
Gitea API token은 필요로 하지 않으며 provider에 맞는 PR 생성 주소를 출력한다.
`kit git publish`는 기존 호환을 위해 push와 review 생성 URL 출력만 담당한다. 실제 review
수명주기는 별도 namespace에서 관리한다.

```bash
kit review submit
kit review status [branch]
kit review wait [branch]
kit review finish [branch]
kit review list
```

`submit`은 현재 branch를 push한 다음 같은 source/target의 열린 review를 정확히 조회한다.
하나면 재사용하고, 없으면 Gitea PR을 생성하며, 둘 이상이면 임의 선택하지
않고 중단한다. `kit pick`은 기본적으로 submit까지 수행하며 local branch 생성 전에 provider와
credential을 preflight한다. `--local`은 push와 PR 생성 없이 local branch만 만든다.
pick 완료 뒤 push 시도 전 submit이 실패하면 원 checkout을 검증하고 생성한 branch와 picked
state를 삭제한다. push 시도가 시작된 뒤에는 원격 반영 여부를 단정할 수 없으므로 local
branch와 review state를 보존해 같은 submit/status/wait 명령으로 복구한다.

상위 `kit sync`는 active review state를 provider에서 먼저 갱신한다. 머지된 review가 하나면
동일한 finish 검증과 work 재구성, local branch 삭제를 한 transaction으로 실행한다. 동시에
머지된 review가 여러 개면 순서를 추정하지 않고 `kit review finish <branch>`를 요구한다.
`--base-only`는 review API를 의도적으로 건너뛰는 복구 경로다. 기존 `kit git sync`는 자동
review finish를 수행하지 않는 호환 동작으로 유지한다.

상위 `kit status`는 active review를 provider에서 갱신하지만 전체 조회에 5초 제한을 둔다.
제한 안에 확인하지 못한 review는 저장된 상태와 refresh 경고를 함께 표시한다. `--cached`는
provider 요청 없이 저장된 review 상태만 읽는 빠른 경로다.

상태는 `.git/kit/reviews/<branch-sha256>.json`에 atomic write하며 token은 기록하지 않는다.
Gitea token은 `kit auth login gitea --host <host>`로 macOS Keychain 또는 Linux Secret
Service에 host별로 등록한다. keyring 자체가 profile을 열거하지 못하므로 kit는 token이
없는 profile metadata만 user config directory에 보관한다. Linux keyring backend가 없는
경우에는 명시적 동의로, macOS Keychain 장애 시에는 `--store file`을 직접 지정한 경우에만
mode `0600` local credential file을 사용할 수 있다. macOS에서는 자동 우회하지 않으며,
backend가 잠겼거나 권한 오류가 난 경우에도 file로 자동 우회하지 않는다. project 설정,
Git config, review state, JSON 출력과 log에는 token을 기록하지 않는다.

`KIT_GITEA_TOKEN`과 `KIT_GITEA_HOST`는 CI/일회성 override로만 지원하며 반드시 함께
지정한다. 환경 변수 쌍이 있으면 저장 credential보다 우선하고, 불완전한 쌍이면 실패한다.
host는 remote의 정확한 소문자 `host[:port]`와 일치해야 한다. 기본 HTTPS 또는 명시적으로
허용한 사설 IP HTTP origin에서 scheme이나 host가 바뀌는 redirect는 차단한다. 제품명이
드러나지 않는 사설 hostname은 `auto`로 판별하지 않고
`git.provider=gitea`를 요구한다.
PR 생성에 필요한 최소 API scope는 `write:repository`다. Git push는 이 API token을 쓰지
않고 기존 SSH key 또는 Git credential helper를 사용한다.
기존 `gitlab`·`forgejo` 설정과 schema 1 review state는 전환 중인 작업을 진단하고 복구할
수 있도록 호환 읽기와 기존 adapter를 유지하지만 신규 설정에는 사용하지 않는다.
Gitea create PR API에 공통 draft field가 없으므로 `--draft`는 `WIP: ` title prefix로
표현하고 server의 `WORK_IN_PROGRESS_PREFIXES`에 해당 prefix를 유지한다. remote source
branch 삭제 여부는 Gitea repository 정책에 맡기며 API token으로 임의 삭제하지 않는다.
현재 adapter는 configured remote를 push 대상과 PR base repository로 함께 사용한다. 따라서
같은 repository의 branch PR만 지원하며 fork → upstream PR은 별도의 head owner/remote 계약을
추가하기 전까지 명시적으로 지원하지 않는다.

`wait`는 daemon이나 OS notification을 추가하지 않는 foreground polling이다. merge가
확인되면 알림과 다음 명령을 출력한다. 개별 provider 요청 timeout은 60초이며 일시적 network
timeout은 stderr에 retry 상태를 표시하고 다음 poll에서 다시 요청한다. 명시적인 인증·계약
오류는 즉시 중단하고 `Ctrl-C` 또는 전체 `--timeout`은 state를 유지한 채 종료한다.
`--yes`를 주면 `finish`까지 이어간다. `finish`는
provider가 merge한 review인지, local branch가 submit 시점 tip과 같은지 확인한 후에만
`develop` fetch/fast-forward와 `work` 재구성을 실행한다. review state에 보존한 원본 work
commit 목록은 squash merge에서도 해당 작업을 안전하게 제거하는 신뢰 집합으로 사용한다.
일반적인 `sync`의 `-x`/patch-id 판정은 review state가 없을 때의 fallback으로 유지한다.

로컬 review branch는 먼저 `git branch -d`로 정리한다. provider merge와 tip 검증을 모두
통과했지만 squash 때문에 safe delete가 실패한 경우에만 사용자가 `--force-delete`를
명시할 수 있다. kit는 remote branch를 직접 강제 삭제하지 않는다.

---

# 4. CLI 공통 규칙

```text
kit [global flags] <command> [arguments] [flags]
```

global flag는 실제 사용하는 milestone에 맞춰 추가한다.

| Flag | 도입 | 의미 |
|---|---|---|
| `--no-color` | v0.1 | ANSI color 비활성화 |
| `--yes` | v0.1 | mutation 명령의 확인 생략 |
| `--cwd <path>` | v0.2 | 해당 directory를 기준으로 실행 |
| `--json` | v0.2 | 자동화 가능한 JSON 출력 |
| `--verbose` | v0.2 | 실행한 하위 명령과 진단 정보 출력 |

초기 exit code 계약:

| Code | 의미 |
|---|---|
| `0` | 성공, 변경할 항목 없음, 명시적 사용자 취소 |
| `1` | validation 또는 실행 실패 |
| `2` | 잘못된 argument나 flag |
| `3` | 해결이 필요한 cherry-pick conflict가 남음 |
| `130` | `SIGINT`로 중단 |

원칙:

- read-only 명령과 mutation 명령을 분리한다.
- command가 직접 `os.Exit`하지 않는다.
- Git 실행은 공통 runner와 Git service를 사용한다.
- 오류는 원인, 사용자가 할 수 있는 조치, 일관된 exit code를 제공한다.
- 사람이 읽는 출력은 `kit · <command>`(결과), `kit ! <notice>`(알림), `$ kit ...`(다음
  명령) 형식을 공통으로 사용하며 색상 없이도 의미가 구분돼야 한다.
- `NO_COLOR`, `--no-color`, non-TTY에서는 ANSI escape를 출력하지 않는다.
- v0.1부터 command는 결과 구조를 반환하고, v0.2의 JSON 출력은 같은 구조를 사용한다.
- 공개하지 않은 기능을 위한 framework를 미리 만들지 않는다.

---

# 5. 최소 구조

```text
kit/
├── README.md
├── go.mod
├── go.sum
├── Makefile
├── install.sh
│
├── cmd/
│   └── kit/
│       └── main.go
│
├── internal/
│   ├── app/
│   ├── buildinfo/
│   ├── clierror/
│   ├── git/
│   ├── hosting/
│   ├── review/
│   ├── reviewstate/
│   ├── selector/
│   ├── ui/
│   └── update/
│
├── site/
│   ├── index.html
│   ├── styles.css
│   ├── app.js
│   └── favicon.svg
│
├── deploy/
│   ├── activate.sh
│   ├── ssh-wrapper.sh
│   ├── generate-release-metadata.sh
│   ├── apps-prod/
│   └── edge/
│
└── .gitea/
    └── workflows/
        ├── ci.yml
        ├── docs.yml
        └── release.yml
```

Go unit·integration test는 대상 package의 `*_test.go`에 함께 둔다. 임시 Git repository는
test에서 생성하므로 별도 fixture directory를 유지하지 않는다.

초기에는 다음을 만들지 않는다.

- 별도 배포 backend
- 복잡한 release metadata migration 계층
- `uninstall.sh`를 포함한 여러 관리 script
- plugin framework
- 범용 setup engine
- package manager abstraction
- 필요하지 않은 OS abstraction

OS별 차이가 실제 command에서 생기면 해당 기능 가까이에 작은 interface를 추가한다.

## 5.1 의존성 정책

`kit`은 가능한 기능을 Go binary에 compile하여 포함하고, 사용자에게 별도 utility 설치를
요구하지 않는다.

| 영역 | 처리 방식 | 사용자 외부 의존성 |
|---|---|---|
| Commit selector | Go TUI library를 binary에 포함 | 없음 |
| HTTP download / update | Go standard library | 없음 |
| SHA-256 검증 | Go standard library | 없음 |
| SemVer 비교 | Go package 또는 내부 구현을 binary에 포함 | 없음 |
| Git 조회 / mutation | system `git` executable 호출 | `git` |
| 최초 bootstrap | POSIX shell installer | `/bin/sh`, `curl`, SHA-256 command |

Git은 내장 재구현하거나 binary에 묶지 않는다. `compare`는 Git의 stable patch-id와 실제
repository config를 사용하고, `cherry-pick`은 conflict state, hook, continue / skip /
abort 동작을 Git과 동일하게 유지해야 하기 때문이다. command 실행 전에 `git` 존재 여부와
지원 version을 검사하고 없으면 OS별 설치 방법을 안내한다. 최소 지원 version은 Git
`2.34.0`으로 고정한다.

Go dependency는 최종 실행 파일에 정적으로 포함하므로 사용자는 TUI library 등을 따로
설치하지 않는다. 새 dependency는 기능 구현에 필요한 최소 package만 추가하고 version을
`go.mod`와 `go.sum`에 고정한다.

---

# 6. 설치

설치에는 repository의 `install.sh` 하나만 사용한다. 이 script는 설치 bootstrap만
담당하며 CLI 기능이나 환경 구성 로직을 포함하지 않는다.

```bash
curl -fsSL https://kit.2juho.com/install.sh | sh
```

`install.sh`가 하는 일은 다음으로 제한한다.

```text
1. uname으로 OS와 architecture 확인
2. Linux는 /etc/os-release로 Ubuntu 24.04인지 확인하고 두 지원 target 중 하나로 변환
3. version.txt에서 현재 stable SemVer tag 확인
4. 해당 version directory에서 target binary와 checksums.txt 다운로드
5. binary의 SHA-256 checksum 확인
6. 실행 권한 설정
7. ~/.local/bin/kit에 설치
8. kit version 실행으로 확인
```

download URL은 항상 최신 production deployment를 가리킨다.

```text
https://kit.2juho.com/version.txt
https://kit.2juho.com/downloads/vX.Y.Z/kit_darwin_arm64
https://kit.2juho.com/downloads/vX.Y.Z/kit_linux_amd64
https://kit.2juho.com/downloads/vX.Y.Z/checksums.txt
```

installer는 JSON parser나 Gitea API를 사용하지 않는다. `version.txt`는 `vX.Y.Z` 한
줄만 허용하며 installer가 형식을 검증한다. version directory는 release 후 변경하지
않으므로 binary와 checksum이 다른 build에서 섞이지 않는다.

설치 원칙:

- 기본 실행에서 `sudo`를 사용하지 않는다.
- checksum이 다르면 설치하지 않는다.
- 지원하지 않는 OS 또는 architecture면 다운로드 전에 종료한다.
- 임시 binary는 최종 설치 directory에 만들고 검증 후 atomic rename한다.
- 기존 binary가 있으면 새 binary 검증이 끝날 때까지 유지한다.
- `${HOME}/.local/bin`이 `PATH`에 없을 때만 shell 시작 파일에 한 줄을 idempotent하게
  추가하고, 무엇을 변경했는지 출력한다.
- zsh는 `~/.zshrc`, bash/sh는 `~/.profile`을 대상으로 한다.
- shell 시작 파일을 변경하기 전에 같은 directory에 backup을 만든다.
- 현재 shell에서는 설치된 binary의 절대 경로로 검증하고, 사용자는 새 terminal부터
  바로 `kit`을 사용할 수 있어야 한다.
- `curl` 또는 SHA-256 도구가 없으면 검증을 생략하지 않고 필요한 명령을 안내한다.

child process인 installer는 현재 parent shell의 `PATH`를 변경할 수 없다. 따라서 설치
직후 같은 shell에서는 `${HOME}/.local/bin/kit`으로 실행하고, shell 시작 파일 변경은
새 terminal부터 적용된다는 점을 완료 메시지에 명확히 표시한다.

복잡한 installer option, channel, profile, 별도 uninstall 과정은 실제 필요가 생길 때까지
추가하지 않는다.

---

# 7. Docs 웹과 자동 배포

`https://kit.2juho.com`은 항상 접근 가능한 단일 페이지 docs이자 최신 binary 배포
endpoint다. 홈 네트워크의 web server가 정적 파일을 제공하고 Cloudflare가 DNS와 외부
hostname을 관리한다. `kit.2juho.com`은 Cloudflare DNS-only record로 public origin에 직접
연결하며 Cloudflare HTTP proxy와 VPN을 거치지 않는다.

기존 홈 인프라의 public IP 또는 DDNS, router 443 forwarding, firewall, HTTPS 인증서와
edge Nginx reverse proxy를 재사용하고 설정은 직접 관리한다. edge가 TLS를 종료하고
apps-prod의 사설 IP `18080`에 있는 Docker 정적 origin으로 전달한다. public endpoint는
정적 docs, installer, release metadata와 download file만 제공하며 apps-prod origin port,
private Gitea, Gitea Runner, SSH deploy endpoint는 외부에 노출하지 않는다.

## 7.1 단일 페이지 구성

한 페이지에서 다음 정보를 제공한다.

```text
kit 소개
한 줄 설치 명령
지원 OS / architecture
kit compare 사용법
kit pick 사용법
OS별 직접 다운로드 button
현재 version / build commit / 배포 시각
최근 변경 내용
kit update 사용법
private Gitea source는 public page에 노출하지 않음
```

초기 웹은 `site/index.html`, `styles.css`, `app.js`로 구현한다. React나 별도 static site
generator는 사용하지 않는다. 기능 설명과 변경 내용은 main branch의 `index.html`에
작성하며, 핵심 설치법과 command 설명은 JavaScript 없이도 읽을 수 있어야 한다.
`app.js`는 `release.json`을 읽어 현재 build 정보와 OS별 download link만 갱신한다.
기능을 변경할 때 page의 command 설명과 최근 변경 항목도 함께 수정한다. deploy workflow가
source file을 다시 commit하지는 않는다.

workflow는 다음 최소 metadata를 생성한다.

```json
{
  "schema_version": 1,
  "version": "v0.1.0",
  "build": "<short-commit-sha>",
  "commit": "<full-commit-sha>",
  "published_at": "<UTC ISO-8601>",
  "downloads": {
    "darwin-arm64": {
      "url": "/downloads/v0.1.0/kit_darwin_arm64",
      "sha256": "<sha256>"
    }
  }
}
```

이 파일은 `https://kit.2juho.com/release.json`으로 배포한다. website와 Go updater는
동일한 metadata를 사용하므로 별도의 표시 version을 관리하지 않는다. shell installer는
구현을 단순하게 유지하기 위해 고정 download path와 `checksums.txt`를 사용한다.
`downloads`에는 두 지원 target을 모두 포함한다.

## 7.2 저장 구조

docs와 CLI release를 분리하여 main 배포가 기존 다운로드를 지우지 않게 한다.

```text
/srv/data/apps/kit/data/
├── sites/
│   └── <main-commit>/
├── releases/
│   └── vX.Y.Z/
│       ├── version.txt
│       ├── release.json
│       ├── checksums.txt
│       └── kit_<os>_<arch>
├── current-site -> sites/<main-commit>
└── current-release -> releases/vX.Y.Z
```

web server route:

```text
/                  → current-site
/install.sh        → current-site/install.sh
/version.txt       → current-release/version.txt
/release.json      → current-release/release.json
/downloads/vX.Y.Z/ → releases/vX.Y.Z/
```

apps-prod Docker Nginx는 host data directory를 `/srv/kit`에 read-only mount한다. edge는
host filesystem을 직접 읽지 않고 이 origin으로 reverse proxy한다. origin Nginx는
directory listing을 비활성화하고 download binary에
`application/octet-stream`과 적절한 `Content-Disposition` header를 제공한다. 정적
download에는 실행 권한이 필요하지 않으며 installer가 검증 후 local 실행 권한을 설정한다.

새 docs 또는 release는 임시 directory에서 검증한 후 symlink를 atomic하게 교체한다.
문제 발생 시 symlink를 직전 directory로 되돌려 rollback한다.

Proxmox 내부의 CI Runner 서버와 배포 서버는 분리한다. Gitea Runner는 build 결과를
제한된 SSH deploy 계정으로 배포 서버의 staging directory에 전송한다. deploy 계정은
`/srv/data/apps/kit/data` 아래의 staging 배치, 검증, symlink 교체에 필요한
권한만 가진다. SSH key에는
Runner source IP, `restrict`와 server-side forced command를 적용한다. workflow가 저장소의
임의 script를 원격 shell에서 실행하지 않으며, root 소유 wrapper가 허용된 site/release
upload 형식만 받아 고정된 activator를 호출한다.

## 7.3 Main branch docs deployment

`main` push는 docs만 갱신하며 CLI binary와 stable version은 변경하지 않는다.

```text
main push
  → docs source 검증
  → site directory 조립
  → preview / staging smoke test
  → sites/<main-commit> 배치
  → current-site symlink 교체
```

docs workflow가 실패하면 기존 `current-site`를 유지한다. 느리게 끝난 이전 workflow가
최신 문서를 덮어쓰지 않도록 배포 직전에 대상 commit이 현재 main HEAD인지 확인한다.

## 7.4 Version tag CLI release

`vX.Y.Z` tag push에서만 binary와 stable download metadata를 갱신한다.

```text
vX.Y.Z tag push
  → unit / integration test
  → 두 target build
  → kit version smoke test
  → target binary와 checksums.txt 생성
  → version.txt와 release.json 생성
  → SSH로 배포 서버 staging directory에 전송
  → releases/vX.Y.Z 배치
  → download URL smoke test
  → current-release symlink 교체
  → 정상 release 최신 5개를 제외한 이전 directory 정리
```

필수 release 조건:

- tag가 `vX.Y.Z` SemVer 형식이어야 한다.
- 같은 version directory를 덮어쓰지 않는다.
- test나 artifact 검증이 실패하면 `current-release`를 변경하지 않는다.
- 두 binary, checksum, version identifier, release metadata가 모두 있어야 한다.
- `kit version`에 version, commit, build target, build time을 주입한다.
- 새 binary 정보가 `release.json`과 일치해야 한다.
- `version.txt`, `release.json`은 web server에서 `no-cache` 또는 동등한 revalidation
  header를 제공한다.
- cleanup은 새 release의 smoke test와 `current-release` 교체가 성공한 뒤 실행한다.
- 현재 release와 rollback 대상인 직전 release는 cleanup 도중 삭제하지 않는다.

Gitea Actions workflow는 `.gitea/workflows/`에 둔다. untrusted PR을 검사하는 `kit-ci`
Runner와 배포 secret을 사용하는 `kit-deploy` Runner는 OS account, work directory, cache,
등록 token과 network 권한을 분리한다. deploy Runner는 보호된 main/tag workflow만 받고
배포 서버의 forced-command SSH endpoint 외 다른 Proxmox service에 접근하지 않는다.
label 자체는 권한 경계가 아니므로 홈 kit repository는 배포 책임자만 branch write가
가능하게 제한한다. 다른 개발자에게 same-repository write를 허용할 때는 배포 workflow와
secret을 PR을 받지 않는 별도 deployment repository/Gitea scope로 먼저 분리한다.

---

# 8. CLI 업데이트

```bash
kit version
kit update
```

`kit update`는 Go 코드로 구현한다. 별도 update shell script를 만들지 않는다.

```text
https://kit.2juho.com/release.json 조회
  → 현재 SemVer와 stable version 비교
  → 현재 OS / architecture download 선택
  → 임시 파일에 다운로드
  → release.json의 SHA-256 검증
  → kit version smoke test
  → 기존 binary 교체
```

규칙:

- 이미 최신이면 성공으로 종료한다.
- checksum 검증 전에는 binary를 실행하지 않는다.
- download 또는 검증 실패 시 기존 binary를 그대로 둔다.
- 실행 중인 binary의 실제 경로를 확인하고, installer로 설치한
  `${HOME}/.local/bin/kit`과 일치할 때만 직접 갱신한다.
- 다른 경로나 symlink를 통해 실행됐다면 임의의 binary를 교체하지 않고 재설치 방법을
  안내한다.
- 임시 binary는 설치 directory에 만들고 검증 후 atomic rename한다.
- write 권한이 없으면 `sudo`를 실행하지 않고 재설치 방법을 안내한다.
- background update와 자동 downgrade는 하지 않는다.
- release version이 현재 version보다 클 때만 update한다.
- 교체 전 새 binary의 version, commit, target, build time이 `release.json`과 일치하는지
  확인한다.

---

# 9. 테스트

## Unit test

- argument와 flag parsing
- `compare`의 반영 / 미반영 분류
- `cherry-pick` 후보 filtering과 source 위상 순서 정렬
- fuzzy 검색, cursor 이동, 다중 선택, 확정과 취소
- non-TTY에서 selector 시작 거부
- patch-id 또는 `-x` 기록에 따른 중복 제외
- 오류와 exit code mapping
- Human / JSON 결과 일치
- OS / architecture asset 이름 선택
- SemVer와 release metadata parsing
- rollback된 이전 deployment를 자동 downgrade로 설치하지 않음

## Integration test

임시 Git repository를 만들어 검증한다.

- source에만 존재하는 commit 비교
- patch가 이미 base에 적용된 경우
- 새 branch 생성과 `cherry-pick -x`
- dirty working tree 거부
- 기존 branch 거부
- conflict의 continue / skip / abort
- process 종료 후 `kit pick --continue` 재개
- stale work에서 pick 차단
- sync 성공 시 applied commit 제거와 pending commit 보존
- sync conflict의 continue / skip / abort와 실제 pending/skipped 수
- sync abort 또는 입력 종료 시 기존 work, checkout, 갱신된 base 복원과 실패 backup 정리
- work backup cleanup의 dry-run, 확인, 기본/`--all` 범위와 부분 삭제 실패
- local/remote target branch 충돌 차단
- Gitea remote URL과 review 주소 생성

## CI / Deploy smoke test

- Apple Silicon macOS runner에서 `darwin/arm64` build와 실행
- Ubuntu 24.04 LTS에서 `linux/amd64` build와 실행
- deploy binary에 실행 권한을 설정한 후 `kit version` 실행
- checksum 불일치 artifact 설치 거부
- 이전 설치본에서 `kit update` 성공
- single-page content, `release.json`, 두 download link 응답 확인
- JavaScript를 끈 상태에서도 설치 명령과 command 설명이 보이는지 확인
- desktop과 mobile viewport에서 설치 명령과 download button 사용 확인

---

# 10. 구현 순서

## v0.1 — shell 명령 통합과 설치

```text
CLI parser와 공통 runner
Git service
kit compare
kit pick
kit version
Human output
--no-color / --yes
필수 오류 처리
두 target build
single-page docs
main docs workflow
version tag release workflow
install.sh
```

완료 조건:

- 기존 `ghash`의 반영 상태 확인을 `kit compare`로 대체할 수 있다.
- 기존 `gpick`의 branch 생성과 cherry-pick 흐름을 `kit pick`으로 대체할 수 있다.
- 깨끗한 macOS Apple Silicon과 Ubuntu에서 한 줄 명령으로 설치된다.
- 설치 직후 `${HOME}/.local/bin/kit version`이 실행되고, 새 terminal에서는 `kit`으로
  두 command가 실행된다.
- `main` 배포 후 `kit.2juho.com`의 기능 설명과 변경 내용이 반영된다.
- `vX.Y.Z` tag 배포 후 version 정보와 두 download가 함께 반영된다.

## v0.2 — 업데이트와 자동화 출력

```text
kit update
--json
--cwd
--verbose
```

완료 조건:

- v0.1 설치본을 binary 재설치 없이 최신 version으로 올릴 수 있다.
- update 실패 시 기존 CLI가 계속 실행된다.

## v0.3 — 협업 Git workflow

```text
kit status
kit sync
kit pick / --local / --wait
kit review submit / status / wait / finish / list
kit backup list / create / restore / cleanup
legacy kit git namespace compatibility
kit auth login / status / list / logout
repository-local kit.git.* config
Gitea provider와 legacy provider 호환
중단 가능한 pick state
```

완료 조건:

- 다른 개발자의 merge 후 최신 develop 위에 pending work commit만 재구성한다.
- sync 실패 시 기존 work, checkout과 develop을 검증된 상태로 되돌리고 실패 backup을 정리한다.
- stale work에서는 새 review branch 생성을 차단한다.
- 회사와 개인 Gitea repository에서 같은 local 명령을 사용한다.
- review 생성, merge 확인, work sync와 안전한 local branch 정리를 한 흐름으로 실행한다.
- release tag는 origin/main에 포함된 commit만 허용한다.

## v0.4 — 필요한 shell 명령 추가

실제 반복 사용 중인 shell script만 하나씩 Go subcommand로 옮긴다.

후보:

```bash
kit worktree
kit branch-clean
kit port
kit process
```

명령을 추가하기 전에 다음을 확인한다.

- 반복해서 사용하는 명령인가?
- OS별 차이를 CLI가 숨겨 주는 가치가 있는가?
- 기존 command의 option으로 해결하는 것보다 독립 command가 명확한가?

dotfiles는 현재 version roadmap에서 제외한다. 명령 기능이 충분히 안정된 뒤 사용자가
다시 요청할 때 별도 요구사항으로 설계하며, 지금은 관련 command, config, dependency를
미리 만들지 않는다.

---

# 11. 확정 사항

확정된 운영 조건:

- DNS: Cloudflare
- origin: 개인 홈 네트워크
- network: Cloudflare HTTP proxy 사용 안 함
- access: Cloudflare DNS-only를 통한 인터넷 직접 공개
- HTTPS: 기존 reverse proxy와 인증서 관리 체계 재사용
- source: 협업 project, 개인 project와 kit source는 각 환경의 private Gitea repository
- CI/CD: 업무 project와 kit은 각 Gitea instance의 Actions와 분리된 Gitea Runner 정책 사용
- Proxmox: 개발, 배포, metrics, CI Runner 서버 분리
- deploy: CI Runner에서 제한된 SSH 계정으로 배포 서버에 전송
- `main` push: docs만 배포
- `vX.Y.Z` tag push: CLI binary와 stable release metadata 배포
- release retention: 최신 5개
- Ubuntu: 24.04 LTS `linux/amd64`만 지원
- Git workflow 기본값: `base=develop`, `source=work`
- commit selector: 외부 `fzf` 없는 full-screen fuzzy multi-select 내장 UI
- installer: shell 설정 backup 후 `~/.local/bin` PATH 자동 추가
- update command: `kit update`
- web server: edge Nginx와 apps-prod Docker Nginx, config 직접 관리
- release version: 수동 `git tag vX.Y.Z` push, tag를 단일 version source로 사용

v0.1 구현에 필요한 결정은 모두 완료됐다. 미확정 항목 없이 명령 기능 구현을 시작할 수
있다.

---

# 12. 현재 하지 않을 것

- Intel Mac 지원
- Windows 지원
- 여러 설치 / 업데이트 script
- 별도 배포 backend
- 범용 package manager 또는 machine profile manager
- VPN, server, secret manager
- plugin system과 workflow engine
- 사용하지 않는 기능을 위한 대규모 abstraction
- installer의 PATH bootstrap을 제외한 dotfiles와 shell 설정 동기화 기능

---

# 13. 한 문장 정의

> `kit`은 macOS Apple Silicon과 Ubuntu에서 반복 사용하던 shell 명령을 명확한 Go
> subcommand로 통합하고, `kit.2juho.com`에서 한 줄로 설치하고 스스로 업데이트할 수 있는
> 개인용 CLI다.
