#!/usr/bin/env bash

set -Eeuo pipefail

readonly KIT_ROOT="/srv/data/apps/kit/data"
readonly CORE_ACTIVATE_COMMAND="/usr/local/libexec/kit-activate-core"

fail() {
  echo "kit activate: $*" >&2
  exit 1
}

[[ -x $CORE_ACTIVATE_COMMAND && ! -L $CORE_ACTIVATE_COMMAND ]] || \
  fail "server-side activator core is unavailable"

if "$CORE_ACTIVATE_COMMAND" "$@"; then
  exit 0
else
  activate_status=$?
fi

# Historical activate.sh behavior can return status 1 after a site deployment
# has already completed successfully. Normalize only that exact, observable
# success state. All other core failures keep their original status.
if [[ $# -eq 3 && $1 == site && $activate_status -eq 1 ]]; then
  identifier=$2
  archive=$3
  if [[ $identifier =~ ^[0-9a-f]{40}$ && \
        $archive == "$KIT_ROOT/incoming/"* && \
        ${archive#"$KIT_ROOT/incoming/"} != */* && \
        ! -e $archive && \
        -L $KIT_ROOT/current-site && \
        $(readlink "$KIT_ROOT/current-site" 2>/dev/null || true) == "sites/$identifier" && \
        -d $KIT_ROOT/sites/$identifier && ! -L $KIT_ROOT/sites/$identifier ]]; then
    exit 0
  fi
fi

exit "$activate_status"
