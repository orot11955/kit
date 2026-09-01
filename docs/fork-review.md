# Gitea fork → upstream review workflow

`kit`은 기본적으로 하나의 repository remote에서 base sync, review branch push, PR target을 모두 처리한다.

Gitea fork를 사용하는 경우에는 역할을 두 remote로 나눌 수 있다.

```text
git.remote       = upstream/base/review target
git.push-remote  = fork/review branch source
```

`git.push-remote`가 설정되지 않으면 기존처럼 `git.remote`가 push remote 역할도 맡는다.

## 예시

```sh
git remote add upstream git@git.example.com:team/project.git
git remote add origin git@git.example.com:juho/project.git

kit config set git.provider gitea
kit config set git.remote upstream
kit config set git.push-remote origin
kit config set git.base develop
kit config set git.source work

kit doctor --network
```

그 뒤 daily workflow는 동일하다.

```sh
kit compare
kit pick feat/login
# upstream repository에서 PR review / merge
kit review finish feat/login
```

## 동작

`kit pick` / `kit review submit`은 review branch를 `git.push-remote`에 push한다.

Gitea PR API는 `git.remote`가 가리키는 upstream repository를 target으로 사용하고, fork source는 다음 형태로 전달한다.

```text
<fork-owner>:<review-branch>
```

예:

```text
juho:feat/login → team/project:develop
```

동일 fork owner/source branch/base의 open PR이 이미 존재하면 새 PR 대신 기존 PR을 재사용한다.

`kit review add`도 base freshness는 upstream remote 기준으로 검사하지만 review branch upstream/tip/push는 fork remote 기준으로 검사한다.

`kit review finish`는 provider가 merge를 확인한 뒤 exact published tip이 일치하는 fork review branch만 정리한다. upstream repository에 우연히 같은 이름의 branch가 있어도 삭제 대상으로 사용하지 않는다.

## 안전 제한

현재 cross-repository review는 다음 조건을 모두 만족해야 한다.

- provider가 Gitea
- upstream과 fork가 같은 Gitea host
- 양쪽 remote URL에서 repository owner/name을 해석할 수 있음
- review API token은 upstream target host에 대한 credential
- Git push credential은 `git.push-remote`에 대한 Git credential

서로 다른 Gitea host 간 PR, GitLab/Forgejo cross-repository review는 현재 자동화하지 않는다.

설정이나 saved review state의 push remote가 바뀌면 `review status/add/finish`는 안전하게 중단한다. saved review URL도 현재 upstream target repository와 일치하는지 검증한다.

## 기존 저장소

기존 same-repository workflow는 변경할 필요가 없다.

```sh
kit config unset git.push-remote
```

또는 `git.push-remote`를 전혀 설정하지 않으면:

```text
push remote = git.remote
```

으로 동작한다.
