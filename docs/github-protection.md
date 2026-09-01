# GitHub branch protection 운영

GitHub upstream의 `main`/`develop` 보호 설정은 `.github/protection/*.json`을 desired state로 사용한다.
실제 repository settings 변경은 `scripts/github-protection.sh`가 GitHub REST branch-protection API로 수행한다.

## 정책

| Branch | PR required | Required checks | Admin enforcement | Force push | Delete |
|---|---:|---|---:|---:|---:|
| `main` | yes | `check-linux`, `check-macos-arm64` | yes | blocked | blocked |
| `develop` | no | `check-linux`, `check-macos-arm64` | yes | blocked | blocked |

`main`의 approving review count는 `0`이다. 개인 repository에서 자기 PR을 스스로 승인할 수 없기 때문에 review approval을 필수화하지 않으면서도 모든 변경이 PR merge 경로를 거치게 하기 위한 설정이다. unresolved review conversation은 `main` merge 전에 해결되어야 한다.

`develop`은 PR-only로 잠그지 않는다. `main` 승격 후 두 branch를 동일 commit으로 맞추는 fast-forward 운영을 유지하기 위해서다. 단, required status checks는 유지하므로 새 `main` merge commit을 `develop`에 fast-forward할 때는 **그 SHA의 main push Verify가 성공한 뒤** ref를 이동해야 한다.

## 필요한 token

다음 중 하나를 환경 변수로 제공한다.

```text
KIT_GITHUB_ADMIN_TOKEN
GH_TOKEN
```

권장값은 별도 fine-grained PAT 또는 GitHub App token이며 대상 repository에 `Administration: Read and write` 권한만 부여한다. 일반 개발 token이나 Gitea token과 공유하지 않는다.

스크립트는 token을 curl command-line argument에 넣지 않는다. mode `0600` 임시 header file에만 기록하고 종료 시 삭제한다.

## 현재 상태 확인

기본 동작은 read-only check다.

```sh
KIT_GITHUB_ADMIN_TOKEN=... scripts/github-protection.sh --check
```

다른 repository를 명시하려면:

```sh
KIT_GITHUB_ADMIN_TOKEN=... \
  scripts/github-protection.sh --check --repo orot11955/kit
```

현재 GitHub 설정이 desired state와 다르면 어떤 항목이 다른지 출력하고 non-zero로 종료한다.

## 적용

```sh
KIT_GITHUB_ADMIN_TOKEN=... scripts/github-protection.sh --apply
```

각 branch에 desired state를 PUT한 뒤 다시 GET하여 실제 설정이 일치하는지 검증한다. 적용 중 API 오류가 발생하면 성공으로 간주하지 않는다.

## 적용 후 branch 운영

기능 개발:

```text
feat/* → PR → develop
```

stable 승격:

```text
develop → PR → main
main push Verify 성공
main SHA → develop fast-forward
```

`main` merge 직후 Verify가 끝나기 전에 `develop` ref를 새 main SHA로 이동하면 develop의 required checks 때문에 GitHub가 거부할 수 있다. 이 경우 protection을 우회하지 말고 해당 SHA의 Verify 성공을 확인한 뒤 fast-forward한다.

## CI에서 검증하는 범위

`make check`는 실제 repository settings를 변경하지 않는다. 대신 다음을 검증한다.

- desired-state JSON syntax
- reconcile script Bash syntax
- fake GitHub API를 사용한 정상 `--check`
- protection drift 감지
- `--apply`가 `main`/`develop` 모두 PUT 후 GET read-back을 수행하는지

실제 settings 적용은 높은 권한 token이 필요한 maintainer operation이므로 repository CI secret으로 자동 주입하지 않는다.
