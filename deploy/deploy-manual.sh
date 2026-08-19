#!/usr/bin/env bash

set -Eeuo pipefail

# --------------------------------------------------
# Config
# --------------------------------------------------

DEPLOY_HOST="${DEPLOY_HOST:-root@apps-prod.home}"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-https://kit.2juho.com}"
REMOTE_ROOT="${REMOTE_ROOT:-/srv/data/apps/kit/data}"

DIST_DIR=".dist/manual-release"
ARTIFACT_DIR="$DIST_DIR/artifacts"

# --------------------------------------------------
# Helpers
# --------------------------------------------------

die() {
  echo "error: $*" >&2
  exit 1
}

info() {
  printf '\n==> %s\n' "$*"
}

# --------------------------------------------------
# Preconditions
# --------------------------------------------------

[[ -f go.mod ]] || die "프로젝트 루트에서 실행해주세요."
[[ -d cmd ]] || die "cmd 디렉터리가 없습니다."
[[ -d internal ]] || die "internal 디렉터리가 없습니다."

command -v go >/dev/null || die "go가 없습니다."
command -v ssh >/dev/null || die "ssh가 없습니다."
command -v scp >/dev/null || die "scp가 없습니다."
command -v sha256sum >/dev/null || die "sha256sum이 없습니다."
command -v curl >/dev/null || die "curl이 없습니다."

# --------------------------------------------------
# Version
# --------------------------------------------------

CURRENT_VERSION="$(
  curl -fsS "$PUBLIC_BASE_URL/version.txt" 2>/dev/null || true
)"

if [[ "$CURRENT_VERSION" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  MAJOR="${BASH_REMATCH[1]}"
  MINOR="${BASH_REMATCH[2]}"
  PATCH="${BASH_REMATCH[3]}"

  VERSION="v${MAJOR}.${MINOR}.$((PATCH + 1))"
else
  VERSION="v0.1.0"
fi

# 직접 version 지정 가능
if [[ -n "${1:-}" ]]; then
  VERSION="$1"
fi

[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  die "invalid version: $VERSION"

info "Release"
echo "current : ${CURRENT_VERSION:-none}"
echo "next    : $VERSION"

# --------------------------------------------------
# Check
# --------------------------------------------------

info "Running checks"

make check

# --------------------------------------------------
# Source ID
# --------------------------------------------------

info "Generating source ID"

SOURCE_ID="$(
  {
    find cmd internal -type f -print
    printf '%s\n' go.mod go.sum
  } |
    LC_ALL=C sort |
    while IFS= read -r file; do
      printf '%s\0' "$file"
      cat "$file"
    done |
    sha256sum |
    cut -c1-40
)"

[[ "$SOURCE_ID" =~ ^[0-9a-f]{40}$ ]] ||
  die "invalid source id: $SOURCE_ID"

BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "source  : $SOURCE_ID"
echo "date    : $BUILD_DATE"

# --------------------------------------------------
# Build
# --------------------------------------------------

info "Building"

rm -rf "$DIST_DIR"
mkdir -p "$ARTIFACT_DIR"

LDFLAGS="-s -w \
-X kit/internal/buildinfo.Version=$VERSION \
-X kit/internal/buildinfo.Commit=$SOURCE_ID \
-X kit/internal/buildinfo.BuildDate=$BUILD_DATE"

CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
  go build \
  -trimpath \
  -ldflags "$LDFLAGS" \
  -o "$ARTIFACT_DIR/kit_darwin_arm64" \
  ./cmd/kit

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build \
  -trimpath \
  -ldflags "$LDFLAGS" \
  -o "$ARTIFACT_DIR/kit_linux_amd64" \
  ./cmd/kit

# --------------------------------------------------
# Checksums
# --------------------------------------------------

info "Generating checksums"

(
  cd "$ARTIFACT_DIR"

  sha256sum \
    kit_darwin_arm64 \
    kit_linux_amd64 \
    > checksums.txt
)

# --------------------------------------------------
# Metadata
# --------------------------------------------------

info "Generating release metadata"

deploy/generate-release-metadata.sh \
  "$ARTIFACT_DIR" \
  "$VERSION" \
  "$SOURCE_ID" \
  "$BUILD_DATE" \
  "$PUBLIC_BASE_URL"

# --------------------------------------------------
# Local verification
# --------------------------------------------------

info "Verifying artifacts"

(
  cd "$ARTIFACT_DIR"
  sha256sum -c checksums.txt
)

case "$(uname -s)/$(uname -m)" in
  Darwin/arm64)
    "$ARTIFACT_DIR/kit_darwin_arm64" version --json
    ;;
  Linux/x86_64)
    "$ARTIFACT_DIR/kit_linux_amd64" version --json
    ;;
  *)
    echo "skip: 현재 호스트에서 실행 가능한 release artifact 없음 ($(uname -s)/$(uname -m))"
    ;;
esac

# --------------------------------------------------
# Remote preparation
# --------------------------------------------------

REMOTE_RELEASE="$REMOTE_ROOT/releases/$VERSION"

info "Preparing remote release"
echo "$DEPLOY_HOST:$REMOTE_RELEASE"

if ssh "$DEPLOY_HOST" "test -e '$REMOTE_RELEASE'"; then
  die "$VERSION already exists on apps-prod"
fi

ssh "$DEPLOY_HOST" \
  "mkdir -p '$REMOTE_RELEASE'"

# --------------------------------------------------
# Upload
# --------------------------------------------------

info "Uploading"

scp \
  "$ARTIFACT_DIR/kit_darwin_arm64" \
  "$ARTIFACT_DIR/kit_linux_amd64" \
  "$ARTIFACT_DIR/checksums.txt" \
  "$ARTIFACT_DIR/version.txt" \
  "$ARTIFACT_DIR/release.json" \
  "$DEPLOY_HOST:$REMOTE_RELEASE/"

# --------------------------------------------------
# Remote verification
# --------------------------------------------------

info "Verifying remote files"

ssh "$DEPLOY_HOST" "
  cd '$REMOTE_RELEASE' &&
  sha256sum -c checksums.txt
"

# --------------------------------------------------
# Activate
# --------------------------------------------------

info "Activating $VERSION"

ssh "$DEPLOY_HOST" "
  set -e

  cd '$REMOTE_ROOT'

  rm -f .current-release.new
  ln -s 'releases/$VERSION' .current-release.new
  mv -Tf .current-release.new current-release
"

# --------------------------------------------------
# Public verification
# --------------------------------------------------

info "Verifying public release"

sleep 1

PUBLIC_VERSION="$(
  curl -fsS "$PUBLIC_BASE_URL/version.txt"
)"

[[ "$PUBLIC_VERSION" == "$VERSION" ]] ||
  die "public version mismatch: expected=$VERSION actual=$PUBLIC_VERSION"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

for artifact in \
  kit_darwin_arm64 \
  kit_linux_amd64 \
  checksums.txt
do
  curl -fsS \
    "$PUBLIC_BASE_URL/downloads/$VERSION/$artifact" \
    -o "$TMP_DIR/$artifact"
done

curl -fsS \
  "$PUBLIC_BASE_URL/release.json" \
  -o "$TMP_DIR/release.json"

cmp -s \
  "$ARTIFACT_DIR/checksums.txt" \
  "$TMP_DIR/checksums.txt" ||
  die "public checksums.txt mismatch"

cmp -s \
  "$ARTIFACT_DIR/release.json" \
  "$TMP_DIR/release.json" ||
  die "public release.json mismatch"

(
  cd "$TMP_DIR"
  sha256sum -c checksums.txt
)

# --------------------------------------------------
# Done
# --------------------------------------------------

printf '\n'
echo "========================================"
echo " kit release deployed"
echo "========================================"
echo "version : $VERSION"
echo "source  : $SOURCE_ID"
echo "date    : $BUILD_DATE"
echo "url     : $PUBLIC_BASE_URL"
echo
