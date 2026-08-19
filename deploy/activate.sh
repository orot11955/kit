#!/usr/bin/env bash

set -Eeuo pipefail

readonly KIT_ROOT="/srv/data/apps/kit/data"
readonly DEPLOY_CONFIG="/etc/kit/deploy.env"
readonly DEPLOY_USER="kit-deploy"
readonly MAX_ARCHIVE_ENTRIES=16
readonly MAX_ARCHIVE_BYTES=$((256 * 1024 * 1024))
readonly MAX_ARCHIVE_ENTRY_BYTES=$((128 * 1024 * 1024))
staging_to_clean=""
link_staging_to_clean=""
origin_response_to_clean=""
KIT_ORIGIN_BASE_URL=""
KIT_ORIGIN_HOST=""

cleanup() {
  if [[ -n $staging_to_clean && $staging_to_clean == "$KIT_ROOT/sites/.staging-"* ]]; then
    rm -rf -- "$staging_to_clean"
  elif [[ -n $staging_to_clean && $staging_to_clean == "$KIT_ROOT/releases/.staging-"* ]]; then
    rm -rf -- "$staging_to_clean"
  fi
  if [[ -n $link_staging_to_clean && $link_staging_to_clean == "$KIT_ROOT/.link-"* ]]; then
    rm -rf -- "$link_staging_to_clean"
  fi
  if [[ -n $origin_response_to_clean && $origin_response_to_clean == "$KIT_ROOT/incoming/.origin-"* ]]; then
    rm -f -- "$origin_response_to_clean"
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "kit deploy: $*" >&2
  exit 1
}

validate_runtime() {
  local deploy_uid curl_version curl_major curl_minor curl_patch
  deploy_uid=$(/usr/bin/id -u "$DEPLOY_USER") || fail "deployment user is unavailable: $DEPLOY_USER"
  (( EUID == deploy_uid )) || fail "activator must run as $DEPLOY_USER"

  curl_version=$(curl --disable --version | awk 'NR == 1 { print $2 }')
  [[ $curl_version =~ ^([0-9]+)\.([0-9]+)\.([0-9]+) ]] || fail "curl version is invalid"
  curl_major=${BASH_REMATCH[1]}
  curl_minor=${BASH_REMATCH[2]}
  curl_patch=${BASH_REMATCH[3]}
  if (( curl_major < 8 ||
        (curl_major == 8 && curl_minor < 4) )); then
    fail "curl 8.4.0 or newer is required for bounded origin downloads (found $curl_major.$curl_minor.$curl_patch)"
  fi
}

load_origin_config() {
  [[ -f $DEPLOY_CONFIG && ! -L $DEPLOY_CONFIG ]] || \
    fail "root-managed origin config is missing: $DEPLOY_CONFIG"
  [[ $(stat -c '%u' "$DEPLOY_CONFIG") == 0 ]] || fail "origin config must be owned by root"

  local permissions
  permissions=$(stat -c '%a' "$DEPLOY_CONFIG")
  [[ $permissions =~ ^[0-7]{3,4}$ ]] || fail "origin config permissions are invalid"
  (( (8#$permissions & 0022) == 0 )) || fail "origin config must not be group/other writable"

  local line key value
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%$'\r'}
    [[ -z $line || $line == \#* ]] && continue
    [[ $line == *=* ]] || fail "invalid origin config line"
    key=${line%%=*}
    value=${line#*=}
    [[ -n $value && $value != *[[:space:]]* ]] || fail "invalid origin config value: $key"
    case "$key" in
      KIT_ORIGIN_BASE_URL) KIT_ORIGIN_BASE_URL=${value%/} ;;
      KIT_ORIGIN_HOST) KIT_ORIGIN_HOST=$value ;;
      *) fail "unknown origin config key: $key" ;;
    esac
  done <"$DEPLOY_CONFIG"

  [[ $KIT_ORIGIN_BASE_URL =~ ^http://[^[:space:]/?#]+$ ]] || \
    fail "KIT_ORIGIN_BASE_URL must be an explicit HTTP origin without a path"
  [[ $KIT_ORIGIN_HOST == kit.2juho.com ]] || fail "KIT_ORIGIN_HOST must be kit.2juho.com"

  local authority ip port first second third fourth octet
  authority=${KIT_ORIGIN_BASE_URL#http://}
  [[ $authority == *:* ]] || fail "KIT_ORIGIN_BASE_URL must include an explicit port"
  ip=${authority%:*}
  port=${authority##*:}
  [[ $ip =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ && $port =~ ^[0-9]{1,5}$ ]] || \
    fail "KIT_ORIGIN_BASE_URL must use an RFC1918 IPv4 address and port"
  IFS=. read -r first second third fourth <<<"$ip"
  for octet in "$first" "$second" "$third" "$fourth"; do
    (( 10#$octet <= 255 )) || fail "KIT_ORIGIN_BASE_URL contains an invalid IPv4 address"
  done
  (( 10#$port >= 1 && 10#$port <= 65535 )) || fail "KIT_ORIGIN_BASE_URL port is invalid"
  if ! (( 10#$first == 10 ||
         (10#$first == 172 && 10#$second >= 16 && 10#$second <= 31) ||
         (10#$first == 192 && 10#$second == 168) )); then
    fail "KIT_ORIGIN_BASE_URL must use an RFC1918 IPv4 address"
  fi
}

origin_get() {
  local path=$1
  local output=$2
  local max_bytes=$3
  curl --disable -fsS \
    --retry 4 \
    --retry-delay 1 \
    --retry-connrefused \
    --connect-timeout 3 \
    --max-time 20 \
    --max-filesize "$max_bytes" \
    --noproxy '*' \
    -H "Host: $KIT_ORIGIN_HOST" \
    "$KIT_ORIGIN_BASE_URL$path" \
    -o "$output"
}

origin_matches_file() {
  local path=$1
  local expected=$2
  local expected_size
  expected_size=$(stat -c '%s' "$expected")
  [[ $expected_size =~ ^[0-9]+$ ]] || fail "expected origin file size is invalid"
  origin_response_to_clean=$(mktemp "$KIT_ROOT/incoming/.origin-XXXXXXXX")
  if ! origin_get "$path" "$origin_response_to_clean" "$expected_size" || \
     ! cmp -s "$expected" "$origin_response_to_clean"; then
    rm -f -- "$origin_response_to_clean"
    origin_response_to_clean=""
    return 1
  fi
  rm -f -- "$origin_response_to_clean"
  origin_response_to_clean=""
}

validate_archive() {
  local archive=$1
  [[ $archive == "$KIT_ROOT/incoming/"* ]] || fail "archive must be under $KIT_ROOT/incoming"
  [[ ${archive#"$KIT_ROOT/incoming/"} != */* ]] || fail "archive path must not contain subdirectories"
  [[ -f $archive && ! -L $archive ]] || fail "archive is missing or is a symlink: $archive"

  local entries entry verbose_entries
  entries=$(tar -tzf "$archive") || fail "archive cannot be listed: $archive"
  [[ -n $entries ]] || fail "archive is empty: $archive"
  while IFS= read -r entry; do
    [[ $entry != /* && $entry != ../* && $entry != *'/../'* && $entry != '..' ]] || \
      fail "unsafe archive entry: $entry"
  done <<<"$entries"

  verbose_entries=$(tar -tvzf "$archive") || fail "archive metadata cannot be read: $archive"
  if awk '{ kind=substr($1, 1, 1); if (kind != "-" && kind != "d") bad=1 } END { exit bad ? 0 : 1 }' \
      <<<"$verbose_entries"; then
    fail "archive contains links or special files"
  fi

  local entry_count total_bytes largest_entry
  read -r entry_count total_bytes largest_entry <<<"$(
    awk '{ size=$3 + 0; count++; total+=size; if (size > largest) largest=size }
         END { printf "%d %.0f %.0f\n", count, total, largest }' <<<"$verbose_entries"
  )"
  [[ $entry_count =~ ^[0-9]+$ && $total_bytes =~ ^[0-9]+$ && $largest_entry =~ ^[0-9]+$ ]] || \
    fail "archive size metadata is invalid"
  (( entry_count <= MAX_ARCHIVE_ENTRIES )) || fail "archive contains too many entries"
  (( total_bytes <= MAX_ARCHIVE_BYTES )) || fail "archive expands beyond the total size limit"
  (( largest_entry <= MAX_ARCHIVE_ENTRY_BYTES )) || fail "archive contains an oversized entry"
}

extract_archive() {
  local archive=$1
  local destination=$2
  mkdir -p "$destination"
  tar --no-same-owner --no-same-permissions -xzf "$archive" -C "$destination"
  [[ -z $(find "$destination" -type l -print -quit) ]] || fail "archive must not contain symbolic links"
}

make_public_readable() {
  local destination=$1
  find "$destination" -type d -exec chmod 0755 {} +
  find "$destination" -type f -exec chmod 0644 {} +
}

atomic_link() {
  local link_name=$1
  local target=$2
  link_staging_to_clean=$(mktemp -d "$KIT_ROOT/.link-${link_name}.XXXXXXXX")
  ln -s "$target" "$link_staging_to_clean/link"
  mv -Tf "$link_staging_to_clean/link" "$KIT_ROOT/$link_name"
  rmdir "$link_staging_to_clean"
  link_staging_to_clean=""
}

remove_or_rollback_link() {
  local link_name=$1
  local previous=$2
  if [[ -n $previous && -e "$KIT_ROOT/$previous" ]]; then
    atomic_link "$link_name" "$previous"
  else
    rm -f -- "$KIT_ROOT/$link_name"
  fi
}

validate_previous_link() {
  local kind=$1
  local previous=$2
  [[ -z $previous ]] && return
  case "$kind" in
    site) [[ $previous =~ ^sites/[0-9a-f]{40}$ ]] || fail "current-site link is invalid" ;;
    release) [[ $previous =~ ^releases/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
      fail "current-release link is invalid" ;;
    *) fail "invalid link kind" ;;
  esac
  [[ -d "$KIT_ROOT/$previous" && ! -L "$KIT_ROOT/$previous" ]] || fail "current link target is invalid"
}

read_current_link() {
  local link_name=$1
  local kind=$2
  local output
  if [[ -e "$KIT_ROOT/$link_name" && ! -L "$KIT_ROOT/$link_name" ]]; then
    fail "$link_name must be a symbolic link"
  fi
  output=$(readlink "$KIT_ROOT/$link_name" 2>/dev/null || true)
  validate_previous_link "$kind" "$output"
  printf '%s' "$output"
}

validate_site_directory() {
  local directory=$1
  local site_id=$2
  local entry name
  for file in index.html styles.css app.js favicon.svg install.sh; do
    [[ -s "$directory/$file" && ! -L "$directory/$file" ]] || fail "site file is missing: $file"
  done
  while IFS= read -r entry; do
    name=${entry#"$directory/"}
    case "$name" in
      index.html|styles.css|app.js|favicon.svg|install.sh) ;;
      *) fail "unexpected public site file: $name" ;;
    esac
  done < <(find "$directory" -mindepth 1 -maxdepth 1 -print)
  grep -Fq 'kit' "$directory/index.html" || fail "site index smoke test failed"
  [[ $(grep -Fc "<meta name=\"kit-site-id\" content=\"$site_id\" />" "$directory/index.html") -eq 1 ]] || \
    fail "site index must contain exactly one matching deployment id"
  sh -n "$directory/install.sh"
}

verify_site_origin() {
  local directory=$1
  origin_matches_file / "$directory/index.html" &&
    origin_matches_file /styles.css "$directory/styles.css" &&
    origin_matches_file /app.js "$directory/app.js" &&
    origin_matches_file /favicon.svg "$directory/favicon.svg" &&
    origin_matches_file /install.sh "$directory/install.sh"
}

validate_checksums_file() {
  local checksums=$1
  awk '
    NF != 2 { bad=1; next }
    $1 !~ /^[0-9a-f]{64}$/ { bad=1; next }
    {
      file=$2
      sub(/^\*/, "", file)
      if (file == "kit_darwin_arm64") darwin++
      else if (file == "kit_linux_amd64") linux++
      else bad=1
    }
    END { exit !(bad == 0 && darwin == 1 && linux == 1 && NR == 2) }
  ' "$checksums" || fail "checksums.txt must contain exactly the two release binaries"
}

validate_release_directory() {
  local directory=$1
  local version=$2
  local file
  for file in kit_darwin_arm64 kit_linux_amd64 checksums.txt version.txt release.json; do
    [[ -s "$directory/$file" && ! -L "$directory/$file" ]] || fail "release file is missing: $file"
  done
  [[ $(tr -d '\r\n' <"$directory/version.txt") == "$version" ]] || \
    fail "version.txt does not match $version"
  grep -Fq "\"version\": \"$version\"" "$directory/release.json" || \
    fail "release.json version mismatch"
  validate_checksums_file "$directory/checksums.txt"
  (
    cd "$directory"
    sha256sum -c checksums.txt
  )
}

verify_release_artifacts_origin() {
  local directory=$1
  local version=$2
  local artifact
  for artifact in kit_darwin_arm64 kit_linux_amd64 checksums.txt; do
    origin_matches_file "/downloads/$version/$artifact" "$directory/$artifact" || return 1
  done
}

verify_release_metadata_origin() {
  local directory=$1
  origin_matches_file /version.txt "$directory/version.txt" &&
    origin_matches_file /release.json "$directory/release.json"
}

deploy_site() {
  local commit=$1
  local archive=$2
  [[ $commit =~ ^[0-9a-f]{40}$ ]] || fail "invalid site commit: $commit"
  validate_archive "$archive"

  local destination="$KIT_ROOT/sites/$commit"
  local created_destination=0
  if [[ -e $destination ]]; then
    [[ -d $destination && ! -L $destination ]] || fail "existing site target is invalid: $destination"
    validate_site_directory "$destination" "$commit"
  else
    local staging
    staging=$(mktemp -d "$KIT_ROOT/sites/.staging-${commit}.XXXXXX")
    staging_to_clean=$staging
    extract_archive "$archive" "$staging"

    validate_site_directory "$staging" "$commit"

    make_public_readable "$staging"

    mv "$staging" "$destination"
    staging_to_clean=""
    created_destination=1
  fi
  make_public_readable "$destination"
  local previous
  previous=$(read_current_link current-site site)
  atomic_link current-site "sites/$commit"

  if ! verify_site_origin "$destination"; then
    remove_or_rollback_link current-site "$previous"
    (( created_destination == 0 )) || rm -rf -- "$destination"
    fail "origin site verification failed; current-site was rolled back"
  fi
  rm -f -- "$archive"
  echo "site activated: $commit"
}

deploy_release() {
  local version=$1
  local archive=$2
  [[ $version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
    fail "invalid release version: $version"
  validate_archive "$archive"

  local destination="$KIT_ROOT/releases/$version"
  local previous current_version
  previous=$(read_current_link current-release release)
  current_version=${previous#releases/}
  if [[ -e $destination ]]; then
    if [[ -d $destination && ! -L $destination && $previous == "releases/$version" ]]; then
      validate_release_directory "$destination" "$version"
      verify_release_artifacts_origin "$destination" "$version" && \
        verify_release_metadata_origin "$destination" || \
        fail "active release does not match the origin"
      rm -f -- "$archive"
      echo "release already active without overwrite: $version"
      return
    fi
    fail "release already exists and will not be overwritten: $version"
  fi
  if [[ $current_version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    [[ $(printf '%s\n%s\n' "$current_version" "$version" | sort -V | tail -n 1) == "$version" && $current_version != "$version" ]] || \
      fail "new release must be greater than current release $current_version"
  fi

  local staging
  staging=$(mktemp -d "$KIT_ROOT/releases/.staging-${version}.XXXXXX")
  staging_to_clean=$staging
  extract_archive "$archive" "$staging"

  validate_release_directory "$staging" "$version"

  make_public_readable "$staging"

  mv "$staging" "$destination"
  staging_to_clean=""

  if ! verify_release_artifacts_origin "$destination" "$version"; then
    rm -rf -- "$destination"
    fail "origin release artifact verification failed before activation"
  fi

  atomic_link current-release "releases/$version"
  if ! verify_release_metadata_origin "$destination"; then
    remove_or_rollback_link current-release "$previous"
    rm -rf -- "$destination"
    fail "origin release metadata verification failed; current-release was rolled back"
  fi

  mapfile -t releases < <(
    find "$KIT_ROOT/releases" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' |
      grep -E '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' |
      sort -V
  )
  if (( ${#releases[@]} > 5 )); then
    local remove_count=$(( ${#releases[@]} - 5 ))
    local old_release
    for old_release in "${releases[@]:0:remove_count}"; do
      [[ $old_release =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || continue
      [[ $old_release != "$version" && "releases/$old_release" != "$previous" ]] || continue
      rm -rf -- "$KIT_ROOT/releases/$old_release"
    done
  fi

  rm -f -- "$archive"
  echo "release activated: $version"
}

rollback_target() {
  local kind=$1
  local identifier=$2
  local link_name target previous
  case "$kind" in
    site)
      [[ $identifier =~ ^[0-9a-f]{40}$ ]] || fail "invalid site id: $identifier"
      link_name=current-site
      target="sites/$identifier"
      [[ -d "$KIT_ROOT/$target" && ! -L "$KIT_ROOT/$target" ]] || fail "site rollback target is missing"
      validate_site_directory "$KIT_ROOT/$target" "$identifier"
      ;;
    release)
      [[ $identifier =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
        fail "invalid release version: $identifier"
      link_name=current-release
      target="releases/$identifier"
      [[ -d "$KIT_ROOT/$target" && ! -L "$KIT_ROOT/$target" ]] || fail "release rollback target is missing"
      validate_release_directory "$KIT_ROOT/$target" "$identifier"
      ;;
    *) fail "usage: activate.sh rollback <site|release> <identifier>" ;;
  esac

  previous=$(read_current_link "$link_name" "$kind")
  atomic_link "$link_name" "$target"

  if [[ $kind == site ]]; then
    if ! verify_site_origin "$KIT_ROOT/$target"; then
      remove_or_rollback_link "$link_name" "$previous"
      fail "origin site rollback verification failed; previous link was restored"
    fi
  else
    if ! verify_release_artifacts_origin "$KIT_ROOT/$target" "$identifier" || \
       ! verify_release_metadata_origin "$KIT_ROOT/$target"; then
      remove_or_rollback_link "$link_name" "$previous"
      fail "origin release rollback verification failed; previous link was restored"
    fi
  fi
  echo "$kind rolled back to: $identifier"
}

[[ $# -eq 3 ]] || fail "usage: activate.sh <site|release> <id> <archive> | activate.sh rollback <site|release> <id>"
mode=$1

validate_runtime
mkdir -p "$KIT_ROOT/incoming" "$KIT_ROOT/sites" "$KIT_ROOT/releases"
if [[ $mode == rollback ]]; then
  exec 8>"$KIT_ROOT/.upload.lock"
  flock -n 8 || fail "another kit upload or rollback is already running"
fi
exec 9>"$KIT_ROOT/.deploy.lock"
flock -n 9 || fail "another kit deployment is already running"
load_origin_config
case "$mode" in
  site|release)
    identifier=$2
    archive=$3
    [[ $mode == site ]] && deploy_site "$identifier" "$archive"
    [[ $mode == release ]] && deploy_release "$identifier" "$archive"
    ;;
  rollback)
    rollback_target "$2" "$3"
    ;;
  *) fail "unknown deployment mode: $mode" ;;
esac
