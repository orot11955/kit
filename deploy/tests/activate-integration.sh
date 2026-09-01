#!/usr/bin/env bash

set -Eeuo pipefail

readonly KIT_ROOT="/srv/data/apps/kit/data"
readonly ACTIVATOR="/usr/local/libexec/kit-activate"
readonly CORE_ACTIVATOR="/usr/local/libexec/kit-activate-core"
readonly SSH_WRAPPER="/usr/local/libexec/kit-ssh-wrapper"
readonly DEPLOY_CONFIG="/etc/kit/deploy.env"
readonly DEPLOY_USER="kit-deploy"

[[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_OS:-} == Linux ]] || {
  echo "activate integration: this destructive production-path fixture only runs on GitHub Actions Linux" >&2
  exit 1
}
(( EUID == 0 )) || {
  echo "activate integration: run as root on the ephemeral GitHub runner" >&2
  exit 1
}

created_user=0
server_pid=""
tmp=""

cleanup() {
  set +e
  if [[ -n $server_pid ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf -- /srv/data/apps/kit
  rm -rf -- /etc/kit
  rm -f -- "$ACTIVATOR" "$CORE_ACTIVATOR" "$SSH_WRAPPER"
  if (( created_user )); then
    userdel "$DEPLOY_USER" 2>/dev/null || true
  fi
  [[ -z $tmp ]] || rm -rf -- "$tmp"
}
trap cleanup EXIT HUP INT TERM

fail() {
  echo "activate integration: $*" >&2
  exit 1
}

is_rfc1918() {
  local ip=$1 first second third fourth
  [[ $ip =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
  IFS=. read -r first second third fourth <<<"$ip"
  for octet in "$first" "$second" "$third" "$fourth"; do
    (( 10#$octet <= 255 )) || return 1
  done
  (( 10#$first == 10 ||
     (10#$first == 172 && 10#$second >= 16 && 10#$second <= 31) ||
     (10#$first == 192 && 10#$second == 168) ))
}

private_ip=""
while IFS= read -r candidate; do
  if is_rfc1918 "$candidate"; then
    private_ip=$candidate
    break
  fi
done < <(ip -4 -o addr show scope global | awk '{ split($4, address, "/"); print address[1] }')
[[ -n $private_ip ]] || fail "GitHub runner has no RFC1918 IPv4 address"

if ! id "$DEPLOY_USER" >/dev/null 2>&1; then
  useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin "$DEPLOY_USER"
  created_user=1
fi

install -d -o root -g root -m 0755 /srv/data/apps/kit
install -d -o "$DEPLOY_USER" -g "$DEPLOY_USER" -m 0755 \
  "$KIT_ROOT" "$KIT_ROOT/incoming" "$KIT_ROOT/sites" "$KIT_ROOT/releases"
install -d -o root -g root -m 0755 /usr/local/libexec /etc/kit
install -o root -g root -m 0755 deploy/activate.sh "$CORE_ACTIVATOR"
install -o root -g root -m 0755 deploy/activate-entrypoint.sh "$ACTIVATOR"
install -o root -g root -m 0755 deploy/ssh-wrapper.sh "$SSH_WRAPPER"

tmp=$(mktemp -d)
origin_root="$tmp/origin"
port_file="$tmp/origin.port"
server_script="$tmp/origin-server.py"
mkdir -p "$origin_root"

cat >"$server_script" <<'PY'
import http.server
import pathlib
import sys

host, root, port_file = sys.argv[1:4]

class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=root, **kwargs)

    def log_message(self, format, *args):
        pass

server = http.server.ThreadingHTTPServer((host, 0), Handler)
pathlib.Path(port_file).write_text(str(server.server_port), encoding="utf-8")
server.serve_forever()
PY

python3 "$server_script" "$private_ip" "$origin_root" "$port_file" &
server_pid=$!
for _ in $(seq 1 100); do
  [[ -s $port_file ]] && break
  kill -0 "$server_pid" 2>/dev/null || fail "origin server exited before publishing its port"
  sleep 0.05
done
[[ -s $port_file ]] || fail "origin server did not publish a port"
origin_port=$(cat "$port_file")
[[ $origin_port =~ ^[0-9]+$ ]] || fail "origin server returned an invalid port"

cat >"$DEPLOY_CONFIG" <<EOF
KIT_ORIGIN_BASE_URL=http://$private_ip:$origin_port
KIT_ORIGIN_HOST=kit.2juho.com
EOF
chown root:root "$DEPLOY_CONFIG"
chmod 0644 "$DEPLOY_CONFIG"

clear_origin() {
  find "$origin_root" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
}

make_site_tree() {
  local directory=$1 commit=$2
  mkdir -p "$directory"
  cat >"$directory/index.html" <<EOF
<!doctype html>
<html><head><meta name="kit-site-id" content="$commit" /></head><body>kit site $commit</body></html>
EOF
  printf 'body { font-family: sans-serif; } /* %s */\n' "$commit" >"$directory/styles.css"
  printf 'console.log("kit %s");\n' "$commit" >"$directory/app.js"
  printf '<svg xmlns="http://www.w3.org/2000/svg"><text>kit %s</text></svg>\n' "$commit" >"$directory/favicon.svg"
  cat >"$directory/install.sh" <<EOF
#!/bin/sh
set -eu
printf '%s\n' 'kit $commit'
EOF
  chmod 0755 "$directory/install.sh"
}

sync_origin_site() {
  local directory=$1
  clear_origin
  cp -a "$directory"/. "$origin_root"/
}

make_site_archive() {
  local directory=$1 commit=$2 archive
  archive="$tmp/upload-site-$commit.tar.gz"
  tar -C "$directory" -czf "$archive" index.html styles.css app.js favicon.svg install.sh
  printf '%s' "$archive"
}

make_release_tree() {
  local directory=$1 version=$2
  mkdir -p "$directory"
  printf 'darwin binary %s\n' "$version" >"$directory/kit_darwin_arm64"
  printf 'linux binary %s\n' "$version" >"$directory/kit_linux_amd64"
  (
    cd "$directory"
    sha256sum kit_darwin_arm64 kit_linux_amd64 >checksums.txt
  )
  printf '%s\n' "$version" >"$directory/version.txt"
  cat >"$directory/release.json" <<EOF
{
  "schema_version": 1,
  "version": "$version",
  "build": "aaaaaaaa",
  "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "published_at": "2026-09-01T00:00:00Z",
  "downloads": {}
}
EOF
}

sync_origin_release() {
  local directory=$1 version=$2
  clear_origin
  mkdir -p "$origin_root/downloads/$version"
  cp "$directory/kit_darwin_arm64" "$directory/kit_linux_amd64" "$directory/checksums.txt" \
    "$origin_root/downloads/$version/"
  cp "$directory/version.txt" "$directory/release.json" "$origin_root/"
}

make_release_archive() {
  local directory=$1 version=$2 archive
  archive="$tmp/upload-release-$version.tar.gz"
  tar -C "$directory" -czf "$archive" \
    kit_darwin_arm64 kit_linux_amd64 checksums.txt version.txt release.json
  printf '%s' "$archive"
}

upload() {
  local operation=$1 identifier=$2 archive=$3
  runuser -u "$DEPLOY_USER" -- env \
    SSH_ORIGINAL_COMMAND="$operation $identifier" \
    SSH_CONNECTION="$private_ip 54321 $private_ip 22" \
    "$SSH_WRAPPER" gitea <"$archive"
}

activate_direct_site() {
  local identifier=$1 source_archive=$2 archive
  archive="$KIT_ROOT/incoming/direct-site-$identifier.tar.gz"
  cp "$source_archive" "$archive"
  chown "$DEPLOY_USER:$DEPLOY_USER" "$archive"
  chmod 0600 "$archive"
  runuser -u "$DEPLOY_USER" -- "$ACTIVATOR" site "$identifier" "$archive"
}

rollback() {
  local kind=$1 identifier=$2
  runuser -u "$DEPLOY_USER" -- "$ACTIVATOR" rollback "$kind" "$identifier"
}

assert_current_site() {
  local commit=$1
  [[ -L $KIT_ROOT/current-site ]] || fail "current-site is not a symlink"
  [[ $(readlink "$KIT_ROOT/current-site") == "sites/$commit" ]] || \
    fail "current-site points to $(readlink "$KIT_ROOT/current-site"), expected sites/$commit"
  [[ -d $KIT_ROOT/sites/$commit && ! -L $KIT_ROOT/sites/$commit ]] || \
    fail "site directory is missing for $commit"
}

assert_current_release() {
  local version=$1
  [[ -L $KIT_ROOT/current-release ]] || fail "current-release is not a symlink"
  [[ $(readlink "$KIT_ROOT/current-release") == "releases/$version" ]] || \
    fail "current-release points to $(readlink "$KIT_ROOT/current-release"), expected releases/$version"
  [[ -d $KIT_ROOT/releases/$version && ! -L $KIT_ROOT/releases/$version ]] || \
    fail "release directory is missing for $version"
}

assert_incoming_empty() {
  [[ -z $(find "$KIT_ROOT/incoming" -mindepth 1 -maxdepth 1 -type f -print -quit) ]] || \
    fail "wrapper left an incoming archive or origin response behind"
}

commit1=$(printf '1%.0s' {1..40})
commit2=$(printf '2%.0s' {1..40})
commit3=$(printf '3%.0s' {1..40})
commit4=$(printf '4%.0s' {1..40})
site1="$tmp/site1"
site2="$tmp/site2"
site3="$tmp/site3"
make_site_tree "$site1" "$commit1"
make_site_tree "$site2" "$commit2"
make_site_tree "$site3" "$commit3"

# Direct invocation goes through the official entrypoint and must return zero
# even though the historical core returns 1 after this exact successful state.
sync_origin_site "$site1"
archive1=$(make_site_archive "$site1" "$commit1")
activate_direct_site "$commit1" "$archive1"
assert_current_site "$commit1"
assert_incoming_empty
cmp -s "$KIT_ROOT/sites/$commit1/index.html" "$site1/index.html" || fail "activated site content differs"
[[ $(stat -c '%U' "$KIT_ROOT/sites/$commit1") == "$DEPLOY_USER" ]] || fail "activated site is not owned by deploy user"

# Subsequent deployment uses the full forced-command wrapper path.
sync_origin_site "$site2"
archive2=$(make_site_archive "$site2" "$commit2")
upload upload-site "$commit2" "$archive2"
assert_current_site "$commit2"
assert_incoming_empty

sync_origin_site "$site1"
rollback site "$commit1"
assert_current_site "$commit1"

archive3=$(make_site_archive "$site3" "$commit3")
if upload upload-site "$commit3" "$archive3"; then
  fail "origin mismatch deployment unexpectedly succeeded"
fi
assert_current_site "$commit1"
assert_incoming_empty
[[ ! -e $KIT_ROOT/sites/$commit3 ]] || fail "failed origin verification left a new site directory"

malicious="$tmp/upload-site-$commit4.tar.gz"
python3 - "$malicious" <<'PY'
import io
import sys
import tarfile

path = sys.argv[1]
with tarfile.open(path, "w:gz") as archive:
    data = b"escape\n"
    info = tarfile.TarInfo("../escape")
    info.size = len(data)
    archive.addfile(info, io.BytesIO(data))
PY
if upload upload-site "$commit4" "$malicious"; then
  fail "traversal archive unexpectedly succeeded"
fi
assert_current_site "$commit1"
assert_incoming_empty
[[ ! -e $KIT_ROOT/sites/$commit4 ]] || fail "traversal archive created a site destination"
[[ ! -e $KIT_ROOT/escape && ! -e /srv/data/apps/kit/escape ]] || fail "traversal archive escaped the staging directory"

version1=v1.0.0
version2=v1.0.1
version3=v1.0.2
release1="$tmp/release1"
release2="$tmp/release2"
release3="$tmp/release3"
make_release_tree "$release1" "$version1"
make_release_tree "$release2" "$version2"
make_release_tree "$release3" "$version3"

sync_origin_release "$release1" "$version1"
release_archive1=$(make_release_archive "$release1" "$version1")
upload upload-release "$version1" "$release_archive1"
assert_current_release "$version1"
assert_incoming_empty

sync_origin_release "$release2" "$version2"
release_archive2=$(make_release_archive "$release2" "$version2")
upload upload-release "$version2" "$release_archive2"
assert_current_release "$version2"
assert_incoming_empty

sync_origin_release "$release1" "$version1"
rollback release "$version1"
assert_current_release "$version1"

# Allow artifact verification for v1.0.2, then deliberately serve stale top-level
# metadata. This forces failure after current-release has moved and verifies that
# the activator restores the previous symlink and removes the failed release.
sync_origin_release "$release3" "$version3"
cp "$release1/version.txt" "$release1/release.json" "$origin_root/"
release_archive3=$(make_release_archive "$release3" "$version3")
if upload upload-release "$version3" "$release_archive3"; then
  fail "release metadata mismatch unexpectedly succeeded"
fi
assert_current_release "$version1"
assert_incoming_empty
[[ ! -e $KIT_ROOT/releases/$version3 ]] || fail "failed release metadata verification left a release directory"

echo "activate integration: direct entrypoint, forced-command site/release activation, rollback, origin failure rollback, and traversal rejection passed"
