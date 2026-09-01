#!/usr/bin/env bash

set -Eeuo pipefail

readonly API_ROOT="https://api.github.com"
readonly API_VERSION="2026-03-10"
readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

mode=check
repository=${GITHUB_REPOSITORY:-orot11955/kit}

usage() {
  cat <<'EOF'
Usage: scripts/github-protection.sh [--check|--apply] [--repo owner/name]

Environment:
  KIT_GITHUB_ADMIN_TOKEN  Fine-grained PAT or GitHub App token with
                          Administration read/write permission.
  GH_TOKEN                Fallback token name when KIT_GITHUB_ADMIN_TOKEN is unset.

Behavior:
  --check   Read main/develop protection and compare with .github/protection/*.json.
            This is the default and never changes repository settings.
  --apply   PUT the desired state for main/develop, then read it back and verify it.

The token is written only to a mode-0600 temporary header file and is never placed
in curl's command-line arguments.
EOF
}

while (($# > 0)); do
  case "$1" in
    --check)
      mode=check
      shift
      ;;
    --apply)
      mode=apply
      shift
      ;;
    --repo)
      (($# >= 2)) || { echo "github-protection: --repo requires owner/name" >&2; exit 2; }
      repository=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "github-protection: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ $repository =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || {
  echo "github-protection: invalid repository name: $repository" >&2
  exit 2
}

for command in curl python3 mktemp; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "github-protection: required command is missing: $command" >&2
    exit 1
  }
done

token=${KIT_GITHUB_ADMIN_TOKEN:-${GH_TOKEN:-}}
[[ -n $token ]] || {
  echo "github-protection: KIT_GITHUB_ADMIN_TOKEN (or GH_TOKEN) is required" >&2
  exit 1
}

auth_file=$(mktemp)
response_file=$(mktemp)
cleanup() {
  rm -f -- "$auth_file" "$response_file"
}
trap cleanup EXIT HUP INT TERM
chmod 0600 "$auth_file" "$response_file"
printf 'Authorization: Bearer %s\n' "$token" >"$auth_file"
unset token KIT_GITHUB_ADMIN_TOKEN GH_TOKEN || true

api_request() {
  local method=$1
  local url=$2
  local output=$3
  local payload=${4:-}
  local -a args=(
    -q
    --fail-with-body
    --silent
    --show-error
    --request "$method"
    --header "@$auth_file"
    --header 'Accept: application/vnd.github+json'
    --header "X-GitHub-Api-Version: $API_VERSION"
    --output "$output"
  )
  if [[ -n $payload ]]; then
    args+=(--header 'Content-Type: application/json' --data-binary "@$payload")
  fi
  if ! curl "${args[@]}" "$url"; then
    if [[ -s $output ]]; then
      cat "$output" >&2
      printf '\n' >&2
    fi
    return 1
  fi
}

verify_branch() {
  local branch=$1
  local desired=$2
  local actual=$3
  python3 - "$branch" "$desired" "$actual" <<'PY'
import json
import sys

branch, desired_path, actual_path = sys.argv[1:]
with open(desired_path, encoding="utf-8") as handle:
    desired = json.load(handle)
with open(actual_path, encoding="utf-8") as handle:
    actual = json.load(handle)

errors = []

required = actual.get("required_status_checks")
if not isinstance(required, dict):
    errors.append("required_status_checks is disabled")
else:
    if bool(required.get("strict")) != bool(desired["required_status_checks"]["strict"]):
        errors.append("required_status_checks.strict differs")
    desired_checks = sorted(item["context"] for item in desired["required_status_checks"]["checks"])
    actual_checks = required.get("checks")
    if isinstance(actual_checks, list):
        actual_names = sorted(item.get("context", "") for item in actual_checks)
    else:
        actual_names = sorted(required.get("contexts") or [])
    if actual_names != desired_checks:
        errors.append(f"required checks differ: got {actual_names!r}, want {desired_checks!r}")


def enabled(name):
    value = actual.get(name)
    if isinstance(value, dict):
        return bool(value.get("enabled"))
    return bool(value)

for name in (
    "enforce_admins",
    "required_linear_history",
    "allow_force_pushes",
    "allow_deletions",
    "block_creations",
    "required_conversation_resolution",
    "lock_branch",
    "allow_fork_syncing",
):
    if enabled(name) != bool(desired[name]):
        errors.append(f"{name} differs: got {enabled(name)!r}, want {bool(desired[name])!r}")

desired_reviews = desired.get("required_pull_request_reviews")
actual_reviews = actual.get("required_pull_request_reviews")
if desired_reviews is None:
    if actual_reviews is not None:
        errors.append("required_pull_request_reviews should be disabled")
else:
    if not isinstance(actual_reviews, dict):
        errors.append("required_pull_request_reviews is disabled")
    else:
        for name in (
            "dismiss_stale_reviews",
            "require_code_owner_reviews",
            "required_approving_review_count",
            "require_last_push_approval",
        ):
            if actual_reviews.get(name) != desired_reviews.get(name):
                errors.append(
                    f"required_pull_request_reviews.{name} differs: "
                    f"got {actual_reviews.get(name)!r}, want {desired_reviews.get(name)!r}"
                )

if desired.get("restrictions") is None and actual.get("restrictions") is not None:
    errors.append("push restrictions should be disabled")

if errors:
    print(f"{branch}: protection differs from desired state", file=sys.stderr)
    for error in errors:
        print(f"  - {error}", file=sys.stderr)
    raise SystemExit(1)

print(f"{branch}: protection matches desired state")
PY
}

for branch in main develop; do
  desired="$REPO_ROOT/.github/protection/$branch.json"
  [[ -f $desired && ! -L $desired ]] || {
    echo "github-protection: desired state is missing: $desired" >&2
    exit 1
  }
  python3 -m json.tool "$desired" >/dev/null

  endpoint="$API_ROOT/repos/$repository/branches/$branch/protection"
  if [[ $mode == apply ]]; then
    echo "$branch: applying protection"
    api_request PUT "$endpoint" "$response_file" "$desired"
  fi

  : >"$response_file"
  api_request GET "$endpoint" "$response_file"
  verify_branch "$branch" "$desired" "$response_file"
done
