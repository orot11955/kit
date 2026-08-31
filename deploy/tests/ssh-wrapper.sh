#!/usr/bin/env bash

set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
WRAPPER="$ROOT/deploy/ssh-wrapper.sh"

fail() {
  echo "ssh-wrapper test: $*" >&2
  exit 1
}

assert_reject() {
  local name=$1
  local expected=$2
  local identity=$3
  local command=$4
  shift 4

  local output status
  set +e
  output=$(SSH_ORIGINAL_COMMAND="$command" SSH_CONNECTION="127.0.0.1 12345 127.0.0.1 22" \
    bash "$WRAPPER" "$identity" "$@" </dev/null 2>&1)
  status=$?
  set -e

  (( status != 0 )) || fail "$name unexpectedly succeeded"
  [[ $output == *"$expected"* ]] || fail "$name: expected '$expected', got '$output'"
}

assert_no_side_effect() {
  local path=$1
  [[ ! -e $path ]] || fail "unexpected side effect: $path exists"
}

# Identity and argument boundaries.
assert_reject "invalid identity" "invalid wrapper identity" "root" "upload-site 0000000000000000000000000000000000000000"
assert_reject "extra wrapper args" "accepts at most one identity" "gitea" "upload-site 0000000000000000000000000000000000000000" extra

# SSH_ORIGINAL_COMMAND must be exactly two tokens.
assert_reject "empty command" "invalid command" "gitea" ""
assert_reject "missing identifier" "invalid command" "gitea" "upload-site"
assert_reject "extra token" "invalid command" "gitea" "upload-site 0000000000000000000000000000000000000000 extra"
assert_reject "shell suffix" "invalid command" "gitea" "upload-site 0000000000000000000000000000000000000000 ; id"

# Only the two upload operations are accepted.
assert_reject "unknown operation" "command is not allowed" "gitea" "shell 0000000000000000000000000000000000000000"
assert_reject "git command" "command is not allowed" "gitea" "git-upload-pack repo"

# Site identifiers are lowercase full SHA-1 strings only.
assert_reject "short site commit" "invalid site commit" "gitea" "upload-site deadbeef"
assert_reject "uppercase site commit" "invalid site commit" "gitea" "upload-site ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD"
assert_reject "site traversal" "invalid site commit" "gitea" "upload-site ../../etc/passwd"

# Releases use strict SemVer without prerelease/build metadata or leading zeroes.
assert_reject "release without v" "invalid release version" "gitea" "upload-release 1.2.3"
assert_reject "release leading zero" "invalid release version" "gitea" "upload-release v01.2.3"
assert_reject "release prerelease" "invalid release version" "gitea" "upload-release v1.2.3-rc1"
assert_reject "release traversal" "invalid release version" "gitea" "upload-release ../../v1.2.3"

# Parser rejection happens before the production filesystem is touched.
assert_no_side_effect "/srv/data/apps/kit/data/.upload.lock"

echo "ssh-wrapper rejection tests passed"
