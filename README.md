# kit

`kit`은 반복해서 사용하던 개발 명령과 Git 작업 흐름을 하나의 Go CLI로 정리한 도구다.

- `kit git status`: `main/develop/work` 역할과 동기화 상태 확인
- `kit compare`: source branch의 commit이 base branch에 반영됐는지 비교
- `kit pick`: 미반영 commit을 내장 fuzzy UI에서 골라 새 branch에 적용
- `kit git sync`: base를 fast-forward하고 work를 미반영 commit만으로 안전하게 재구성
- `kit git publish`: 기능 branch를 push하고 GitLab MR 또는 Forgejo PR 주소 안내
- `kit git review submit`: 기능 branch push와 GitLab MR/Forgejo PR 생성을 한 번에 처리
- `kit git review wait`: foreground에서 merge 상태를 확인하고 완료를 알림
- `kit git review finish`: merge 확인 후 work 동기화와 로컬 branch 정리
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
# 저장소별 기본 역할 설정(main/develop/work, origin, provider)
kit config init

# 전체 workflow와 원격 추적 상태 확인
kit git status

# work의 commit이 develop에 반영됐는지 확인
kit compare

# source와 base를 직접 지정하고 최신 10개만 확인
kit compare work --base develop --limit 10

# work의 미반영 commit을 선택해 develop 기반의 새 branch에 적용
kit pick feat/login

# pick 직후 push와 MR/PR 생성까지 진행
kit pick feat/login --submit

# 생성 후 foreground에서 merge를 기다리고 완료 시 정리
kit pick feat/login --submit --wait

# 다른 개발자의 merge를 반영하고 work를 최신 develop 위에 재구성
kit git sync

# 현재 기능 branch push 후 review 생성 주소 확인
kit git publish

# 현재 기능 branch push 후 MR/PR을 실제 생성하거나 기존 요청 재사용
kit git review submit

# 다른 terminal에서 상태 확인 또는 merge 대기
kit git review status feat/login
kit git review wait feat/login

# merge 완료 확인 후 develop/work 갱신과 로컬 branch 삭제
kit git review finish feat/login

# 설치본 정보와 update 확인
kit version
kit update
```

기계가 읽는 결과가 필요하면 `status`, `compare`, `sync`, `publish`, `review`, `version`,
`update`에 `--json`을 사용할 수 있다. `review submit --json`과
`review finish --json`처럼 상태를 바꾸는 명령은 `--yes`를 함께 지정해야 한다.
다른 repository에서 실행하려면 `--cwd <path>`를 지정한다. `pick`은 full-screen selector를
사용하므로 TTY에서만 실행된다.

`work`는 원격에 push하는 공유 branch가 아니라 로컬 commit queue다. `develop`이 바뀌면
`kit git sync`가 기존 `work`를 먼저 backup한 뒤 최신 `develop` 위에 미반영 commit만
위상 순서대로 다시 적용한다. 충돌하면 임시 branch에서 파일을 해결하고 `git add`한 뒤
`continue`, 해당 commit을 제외하는 `skip`, 기존 work로 돌아가는 `abort`를 선택한다.
VS Code 등에서 해결 내용을 먼저 commit한 경우에도 `continue`를 선택하면 완료된 commit을
감지해 다음 commit으로 진행한다.
전체 재구성이 성공하기 전에는 `work`를 교체하지 않으며 abort나 입력 종료 시 기존
`work`와 backup을 유지한다. 일부 충돌 해결 commit을 만든 뒤 재구성이 중단되면 해당
commit은 오류에 표시되는 `kit/recovery/*` branch에 별도로 보존한다. 중단된 `pick`은
`kit pick --continue`, `--skip`, `--abort`로
이어갈 수 있다.

협업 repository는 GitLab, 개인 repository는 Forgejo를 저장소별로 설정한다.

```sh
kit config set git.provider gitlab   # 회사·협업 프로젝트
kit config set git.provider forgejo  # 개인 프로젝트
```

리뷰 API token은 저장소 설정이나 kit 상태 파일에 저장하지 않고 환경 변수로만 읽는다.
token이 다른 host로 전송되지 않도록 host도 함께 고정해야 한다.

```sh
# gitlab.com은 KIT_GITLAB_HOST를 생략할 수 있다.
export KIT_GITLAB_TOKEN='...'
export KIT_GITLAB_HOST='gitlab.company.example'  # self-hosted GitLab일 때 필수

export KIT_FORGEJO_TOKEN='...'
export KIT_FORGEJO_HOST='git.2juho.com'           # Forgejo는 항상 필수
```

host 값은 scheme이나 path 없는 정확한 소문자 `host[:port]` 형식이어야 한다. token에는
해당 repository 조회, branch push, MR/PR 생성에 필요한 최소 권한만 부여한다. `review wait`는
daemon이 아니라 현재 terminal에서만 동작하며 기본 15초 간격으로 확인한다.

`review finish`는 provider가 merge를 확인한 뒤에만 `develop`과 `work`를 동기화한다. 일반
merge는 `git branch -d`로 로컬 review branch를 자동 정리한다. squash merge라서 Git의
safe delete가 거부되면 내용을 확인한 뒤 `--force-delete`를 명시해야 한다. 원격 branch
삭제 여부는 provider 설정에 맡기며 kit가 임의로 강제 삭제하지 않는다.

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

Forgejo secrets, 배포 계정과 Nginx 설정 방법은 [deploy/README.md](deploy/README.md)를,
전체 설계와 결정 사항은 [docs/kit-architecture.md](docs/kit-architecture.md)를 참고한다.

dotfiles 동기화는 현재 범위에 포함하지 않는다. 우선 명령 기능과 설치·업데이트 경로를
안정화한 뒤 별도 단계로 다룬다.
