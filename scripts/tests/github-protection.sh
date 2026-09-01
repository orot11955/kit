#!/usr/bin/env bash

set -Eeuo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
cleanup() { rm -rf -- "$tmp"; }
trap cleanup EXIT HUP INT TERM
mkdir -p "$tmp/bin"

cat >"$tmp/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

method=GET
output=""
url=""
while (($# > 0)); do
  case "$1" in
    --request)
      method=$2
      shift 2
      ;;
    --output)
      output=$2
      shift 2
      ;;
    --header|--data-binary)
      shift 2
      ;;
    -q|--fail-with-body|--silent|--show-error)
      shift
      ;;
    http*)
      url=$1
      shift
      ;;
    *)
      shift
      ;;
  esac
done

[[ -n $output && -n $url ]] || exit 91
branch=${url##*/}
if [[ -n ${FAKE_CURL_LOG:-} ]]; then
  printf '%s %s\n' "$method" "$url" >>"$FAKE_CURL_LOG"
fi

reviews='null'
conversation=false
if [[ $branch == main ]]; then
  reviews='{"dismiss_stale_reviews":true,"require_code_owner_reviews":false,"required_approving_review_count":0,"require_last_push_approval":false}'
  # Strip the shell-escaping backslashes before embedding the object into JSON.
  reviews=${reviews//\\"/"}
  conversation=true
elif [[ $branch != develop ]]; then
  exit 92
fi

force=false
if [[ ${FAKE_PROTECTION_MISMATCH:-0} == 1 && $branch == main ]]; then
  force=true
fi

cat >"$output" <<JSON
{
  "required_status_checks": {
    "strict": true,
    "contexts": ["check-linux", "check-macos-arm64"],
    "checks": [
      {"context":"check-linux","app_id":123},
      {"context":"check-macos-arm64","app_id":123}
    ]
  },
  "enforce_admins": {"enabled": true},
  "required_pull_request_reviews": $reviews,
  "restrictions": null,
  "required_linear_history": {"enabled": false},
  "allow_force_pushes": {"enabled": $force},
  "allow_deletions": {"enabled": false},
  "block_creations": {"enabled": false},
  "required_conversation_resolution": {"enabled": $conversation},
  "lock_branch": {"enabled": false},
  "allow_fork_syncing": {"enabled": false}
}
JSON
EOF
chmod 0755 "$tmp/bin/curl"

export PATH="$tmp/bin:$PATH"
export KIT_GITHUB_ADMIN_TOKEN=test-token

if ! "$repo_root/scripts/github-protection.sh" --check --repo orot11955/kit >/dev/null; then
  echo "github-protection test: matching protection check failed" >&2
  exit 1
fi

if FAKE_PROTECTION_MISMATCH=1 "$repo_root/scripts/github-protection.sh" --check --repo orot11955/kit >/dev/null 2>&1; then
  echo "github-protection test: drift was not detected" >&2
  exit 1
fi

log="$tmp/curl.log"
if ! FAKE_CURL_LOG="$log" "$repo_root/scripts/github-protection.sh" --apply --repo orot11955/kit >/dev/null; then
  echo "github-protection test: apply/read-back failed" >&2
  exit 1
fi
for branch in main develop; do
  grep -Fq "PUT https://api.github.com/repos/orot11955/kit/branches/$branch/protection" "$log" || {
    echo "github-protection test: missing PUT for $branch" >&2
    exit 1
  }
  grep -Fq "GET https://api.github.com/repos/orot11955/kit/branches/$branch/protection" "$log" || {
    echo "github-protection test: missing read-back GET for $branch" >&2
    exit 1
  }
done

echo "github-protection tests passed"
