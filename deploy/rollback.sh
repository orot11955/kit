#!/usr/bin/env bash

set -Eeuo pipefail

readonly ACTIVATE_COMMAND="/usr/local/libexec/kit-activate"
readonly RUNUSER_COMMAND="/usr/sbin/runuser"
readonly DEPLOY_USER="kit-deploy"

if (( EUID != 0 )); then
  echo "kit rollback: run as root" >&2
  exit 1
fi
if [[ $# -ne 2 ]]; then
  echo "usage: kit-rollback <site|release> <identifier>" >&2
  exit 2
fi
[[ -x $ACTIVATE_COMMAND && ! -L $ACTIVATE_COMMAND ]] || {
  echo "kit rollback: server-side activator is unavailable" >&2
  exit 1
}
[[ -x $RUNUSER_COMMAND && ! -L $RUNUSER_COMMAND ]] || {
  echo "kit rollback: runuser is unavailable" >&2
  exit 1
}

case "$1" in
  site)
    [[ $2 =~ ^[0-9a-f]{40}$ ]] || {
      echo "kit rollback: invalid site id" >&2
      exit 2
    }
    ;;
  release)
    [[ $2 =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
      echo "kit rollback: invalid release version" >&2
      exit 2
    }
    ;;
  *)
    echo "usage: kit-rollback <site|release> <identifier>" >&2
    exit 2
    ;;
esac

exec "$RUNUSER_COMMAND" -u "$DEPLOY_USER" -- "$ACTIVATE_COMMAND" rollback "$1" "$2"
