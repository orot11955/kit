#!/usr/bin/env bash

set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
cd "$script_dir"

command=${1:-up}

validate_origin_bind() {
  [[ -f .env ]] || {
    echo "missing $script_dir/.env; copy .env.example and set KIT_ORIGIN_BIND" >&2
    exit 1
  }

  local bind_ip
  bind_ip=$(awk -F= '$1 == "KIT_ORIGIN_BIND" { value=$2 } END { print value }' .env)
  [[ $bind_ip =~ ^10\.([0-9]{1,3}\.){2}[0-9]{1,3}$ ||
     $bind_ip =~ ^192\.168\.([0-9]{1,3}\.)[0-9]{1,3}$ ||
     $bind_ip =~ ^172\.(1[6-9]|2[0-9]|3[01])\.([0-9]{1,3}\.)[0-9]{1,3}$ ]] || {
    echo "KIT_ORIGIN_BIND must be an RFC1918 IPv4 address, got: ${bind_ip:-empty}" >&2
    exit 1
  }
  ip -4 -o address show | awk '{print $4}' | cut -d/ -f1 | grep -Fxq "$bind_ip" || {
    echo "KIT_ORIGIN_BIND is not assigned to an apps-prod interface: $bind_ip" >&2
    exit 1
  }
}

case "$command" in
  up)
    validate_origin_bind
    docker compose -f compose.yml config --quiet
    docker compose -f compose.yml up -d --remove-orphans
    ;;
  down)
    docker compose -f compose.yml down
    ;;
  restart)
    docker compose -f compose.yml restart kit-origin
    ;;
  status)
    docker compose -f compose.yml ps
    ;;
  logs)
    docker compose -f compose.yml logs --tail=200 -f kit-origin
    ;;
  config)
    validate_origin_bind
    docker compose -f compose.yml config
    ;;
  *)
    echo "usage: $0 {up|down|restart|status|logs|config}" >&2
    exit 2
    ;;
esac
