#!/usr/bin/env bash

set -Eeuo pipefail
set -f

readonly KIT_ROOT="/srv/data/apps/kit/data"
readonly ACTIVATE_COMMAND="/usr/local/libexec/kit-activate"
readonly MAX_ARCHIVE_BYTES=$((100 * 1024 * 1024))
readonly UPLOAD_TIMEOUT_SECONDS=300
archive=""
identity=${1:-legacy}

[[ $# -le 1 ]] || {
  echo "kit deploy SSH: wrapper accepts at most one identity" >&2
  exit 1
}
# forgejo is accepted only so an already-installed forced-command key keeps working
# during the Gitea key rotation. New authorized_keys entries must use gitea.
case "$identity" in
  manual|gitea|forgejo|legacy) ;;
  *)
    echo "kit deploy SSH: invalid wrapper identity" >&2
    exit 1
    ;;
esac

fail() {
  echo "kit deploy SSH: $*" >&2
  exit 1
}

cleanup() {
  if [[ -n $archive && $archive == "$KIT_ROOT/incoming/"* ]]; then
    rm -f -- "$archive"
  fi
}
trap cleanup EXIT HUP INT TERM

# Validate the forced command before touching server paths or the activator. This
# keeps malformed requests side-effect free and makes the allowlist boundary
# independently testable on an unprivileged CI runner.
read -r operation identifier extra <<<"${SSH_ORIGINAL_COMMAND:-}"
[[ -n $operation && -n $identifier && -z ${extra:-} ]] || fail "invalid command"

case "$operation" in
  upload-site)
    mode=site
    [[ $identifier =~ ^[0-9a-f]{40}$ ]] || fail "invalid site commit"
    ;;
  upload-release)
    mode=release
    [[ $identifier =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
      fail "invalid release version"
    ;;
  *)
    fail "command is not allowed"
    ;;
esac

[[ -x $ACTIVATE_COMMAND && ! -L $ACTIVATE_COMMAND ]] || fail "server-side activator is unavailable"

source_address=${SSH_CONNECTION%% *}
[[ $source_address =~ ^[0-9A-Fa-f:.]+$ ]] || source_address=unknown
logger -t kit-deploy \
  "identity=$identity source=$source_address operation=$operation identifier=$identifier accepted" || true

umask 077
mkdir -p "$KIT_ROOT/incoming"
exec 8>"$KIT_ROOT/.upload.lock"
flock -n 8 || fail "another kit upload is already running"
archive=$(mktemp "$KIT_ROOT/incoming/${mode}-${identity}-${identifier}.XXXXXX.tar.gz")

# Bound data accepted from the Runner so a compromised key cannot fill the disk with one request.
if ! timeout --foreground "$UPLOAD_TIMEOUT_SECONDS" \
    dd bs=1048576 count=101 status=none of="$archive"; then
  fail "archive upload timed out or failed"
fi
archive_size=$(wc -c <"$archive")
(( archive_size > 0 && archive_size <= MAX_ARCHIVE_BYTES )) || fail "archive size is invalid"

# activate.sh historically returned 1 after a successful site activation because
# its final release predicate was false. Do not broadly mask activator failures:
# normalize only that exact status when the full site success postcondition is
# already visible (archive consumed, exact current-site target, destination exists).
if "$ACTIVATE_COMMAND" "$mode" "$identifier" "$archive"; then
  :
else
  activate_status=$?
  site_success=0
  if [[ $mode == site && $activate_status -eq 1 && ! -e $archive && \
        -L $KIT_ROOT/current-site && \
        $(readlink "$KIT_ROOT/current-site" 2>/dev/null || true) == "sites/$identifier" && \
        -d $KIT_ROOT/sites/$identifier && ! -L $KIT_ROOT/sites/$identifier ]]; then
    site_success=1
  fi
  (( site_success == 1 )) || exit "$activate_status"
fi

archive=""
logger -t kit-deploy \
  "identity=$identity source=$source_address operation=$operation identifier=$identifier completed" || true
