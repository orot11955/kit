#!/usr/bin/env bash

set -Eeuo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: $0 <release-dir> <version> <commit> <published-at> <base-url>" >&2
  exit 2
fi

release_dir=$1
version=$2
commit=$3
published_at=$4
base_url=${5%/}

[[ $version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
  echo "invalid release version: $version" >&2
  exit 2
}
[[ $commit =~ ^[0-9a-f]{40}$ ]] || {
  echo "invalid commit: $commit" >&2
  exit 2
}
[[ $published_at =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || {
  echo "invalid published-at: $published_at" >&2
  exit 2
}
[[ $base_url == https://* ]] || {
  echo "base URL must use HTTPS" >&2
  exit 2
}
[[ -d $release_dir && ! -L $release_dir ]] || {
  echo "release directory is missing or is a symlink: $release_dir" >&2
  exit 2
}

for artifact in kit_darwin_arm64 kit_linux_amd64 checksums.txt; do
  [[ -s "$release_dir/$artifact" && ! -L "$release_dir/$artifact" ]] || {
    echo "missing release artifact: $artifact" >&2
    exit 1
  }
done

checksum_for() {
  local artifact=$1
  awk -v file="$artifact" '
    ($2 == file || $2 == "*" file) { value=$1; count++ }
    END { if (count == 1) print value }
  ' "$release_dir/checksums.txt"
}

darwin_sha=$(checksum_for kit_darwin_arm64)
linux_sha=$(checksum_for kit_linux_amd64)
[[ $darwin_sha =~ ^[0-9a-f]{64}$ && $linux_sha =~ ^[0-9a-f]{64}$ ]] || {
  echo "checksums.txt does not contain exactly one valid checksum per binary" >&2
  exit 1
}

short_commit=${commit:0:12}
printf '%s\n' "$version" >"$release_dir/version.txt"
printf '%s\n' \
  '{' \
  '  "schema_version": 1,' \
  "  \"version\": \"$version\"," \
  "  \"build\": \"$short_commit\"," \
  "  \"commit\": \"$commit\"," \
  "  \"published_at\": \"$published_at\"," \
  '  "downloads": {' \
  '    "darwin-arm64": {' \
  "      \"url\": \"/downloads/$version/kit_darwin_arm64\"," \
  "      \"sha256\": \"$darwin_sha\"" \
  '    },' \
  '    "linux-amd64": {' \
  "      \"url\": \"/downloads/$version/kit_linux_amd64\"," \
  "      \"sha256\": \"$linux_sha\"" \
  '    }' \
  '  }' \
  '}' >"$release_dir/release.json"

