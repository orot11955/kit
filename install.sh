#!/bin/sh

set -eu

BASE_URL="https://kit.2juho.com"
INSTALL_DIR="${HOME:?HOME is not set}/.local/bin"
INSTALL_PATH="${INSTALL_DIR}/kit"
PATH_MARKER_BEGIN="# >>> kit PATH >>>"
PATH_MARKER_END="# <<< kit PATH <<<"

fail() {
  printf 'kit install: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "'$1' 명령이 필요합니다. 설치한 뒤 다시 실행하세요."
}

require_command curl

umask 077

if command -v sha256sum >/dev/null 2>&1; then
  SHA_COMMAND="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA_COMMAND="shasum -a 256"
else
  fail "checksum 검증을 위해 'sha256sum' 또는 'shasum'이 필요합니다."
fi

os_name=$(uname -s 2>/dev/null || fail "운영체제를 확인할 수 없습니다.")
arch_name=$(uname -m 2>/dev/null || fail "CPU architecture를 확인할 수 없습니다.")

case "${os_name}:${arch_name}" in
  Darwin:arm64)
    artifact="kit_darwin_arm64"
    ;;
  Linux:x86_64|Linux:amd64)
    [ -r /etc/os-release ] || fail "Linux에서는 Ubuntu 24.04 amd64만 지원합니다."
    linux_id=$(sed -n 's/^ID=//p' /etc/os-release | head -n 1 | tr -d '"')
    linux_version=$(sed -n 's/^VERSION_ID=//p' /etc/os-release | head -n 1 | tr -d '"')
    [ "$linux_id" = "ubuntu" ] && [ "$linux_version" = "24.04" ] || \
      fail "현재 Linux는 지원하지 않습니다. Ubuntu 24.04 amd64만 지원합니다."
    artifact="kit_linux_amd64"
    ;;
  Darwin:x86_64)
    fail "Intel Mac은 지원하지 않습니다. Apple Silicon Mac만 지원합니다."
    ;;
  *)
    fail "지원하지 않는 환경입니다: ${os_name}/${arch_name}. 지원 대상은 darwin/arm64와 Ubuntu 24.04 linux/amd64입니다."
    ;;
esac

version=$(curl --proto '=https' --proto-redir '=https' --max-redirs 0 -fsSL "${BASE_URL}/version.txt") || \
  fail "stable version 정보를 받지 못했습니다."
version=$(printf '%s' "$version" | tr -d '\r\n')
printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || \
  fail "version.txt 형식이 올바르지 않습니다: ${version}"

[ ! -L "$INSTALL_DIR" ] || fail "설치 directory가 symbolic link이면 사용할 수 없습니다: ${INSTALL_DIR}"
mkdir -p "$INSTALL_DIR" || fail "설치 directory를 만들 수 없습니다: ${INSTALL_DIR}"
[ -d "$INSTALL_DIR" ] || fail "설치 경로가 directory가 아닙니다: ${INSTALL_DIR}"
if [ "$os_name" = "Darwin" ]; then
  install_dir_uid=$(stat -f '%u' "$INSTALL_DIR") || fail "설치 directory 소유자를 확인할 수 없습니다."
else
  install_dir_uid=$(stat -c '%u' "$INSTALL_DIR") || fail "설치 directory 소유자를 확인할 수 없습니다."
fi
[ "$install_dir_uid" = "$(id -u)" ] || fail "설치 directory가 현재 사용자 소유가 아닙니다: ${INSTALL_DIR}"
chmod 700 "$INSTALL_DIR" || fail "설치 directory 권한을 보호할 수 없습니다: ${INSTALL_DIR}"
binary_tmp=$(mktemp "${INSTALL_DIR}/.kit.download.XXXXXX") || fail "임시 파일을 만들 수 없습니다."
checksum_tmp=$(mktemp "${INSTALL_DIR}/.kit.checksums.XXXXXX") || fail "임시 파일을 만들 수 없습니다."

cleanup() {
  rm -f "$binary_tmp" "$checksum_tmp"
}
trap cleanup EXIT HUP INT TERM

download_base="${BASE_URL}/downloads/${version}"
curl --proto '=https' --proto-redir '=https' --max-redirs 0 -fsSL "${download_base}/${artifact}" -o "$binary_tmp" || \
  fail "${artifact} 다운로드에 실패했습니다."
curl --proto '=https' --proto-redir '=https' --max-redirs 0 -fsSL "${download_base}/checksums.txt" -o "$checksum_tmp" || \
  fail "checksums.txt 다운로드에 실패했습니다."

expected_checksum=$(awk -v file="$artifact" '
  ($2 == file || $2 == "*" file) { value=$1; count++ }
  END { if (count == 1) print value }
' "$checksum_tmp")
[ -n "$expected_checksum" ] || fail "checksums.txt에서 ${artifact}의 유일한 checksum을 찾지 못했습니다."
printf '%s\n' "$expected_checksum" | grep -Eq '^[0-9a-fA-F]{64}$' || fail "checksum 형식이 올바르지 않습니다."

if [ "$SHA_COMMAND" = "sha256sum" ]; then
  actual_checksum=$(sha256sum "$binary_tmp" | awk '{print $1}')
else
  actual_checksum=$(shasum -a 256 "$binary_tmp" | awk '{print $1}')
fi
[ "$actual_checksum" = "$expected_checksum" ] || fail "checksum이 일치하지 않아 설치를 중단했습니다."

chmod 755 "$binary_tmp"
downloaded_version=$("$binary_tmp" version 2>/dev/null | sed -n '1p') || \
  fail "다운로드한 binary의 실행 검증에 실패했습니다."
[ "$downloaded_version" = "kit ${version}" ] || \
  fail "다운로드한 binary version이 version.txt와 일치하지 않습니다."

if [ -e "$INSTALL_PATH" ]; then
  backup_path="${INSTALL_PATH}.backup.$(date -u +%Y%m%dT%H%M%SZ).$$"
  cp -p "$INSTALL_PATH" "$backup_path" || fail "기존 kit backup에 실패했습니다."
  printf '기존 kit backup: %s\n' "$backup_path"
fi

mv -f "$binary_tmp" "$INSTALL_PATH" || fail "kit 설치에 실패했습니다."
binary_tmp=""
"$INSTALL_PATH" version || fail "설치된 kit 실행 검증에 실패했습니다."

case ":${PATH:-}:" in
  *":${INSTALL_DIR}:"*)
    path_changed=0
    ;;
  *)
    path_changed=1
    case "${SHELL:-}" in
      */zsh) rc_file="${HOME}/.zshrc" ;;
      *) rc_file="${HOME}/.profile" ;;
    esac

    if [ ! -f "$rc_file" ] || ! grep -Fq "$PATH_MARKER_BEGIN" "$rc_file"; then
      if [ -f "$rc_file" ]; then
        rc_backup="${rc_file}.backup.$(date -u +%Y%m%dT%H%M%SZ).$$"
        cp -p "$rc_file" "$rc_backup" || fail "${rc_file} backup에 실패했습니다."
        printf 'shell 설정 backup: %s\n' "$rc_backup"
      fi
      {
        printf '\n%s\n' "$PATH_MARKER_BEGIN"
        printf '%s\n' 'export PATH="$HOME/.local/bin:$PATH"'
        printf '%s\n' "$PATH_MARKER_END"
      } >>"$rc_file" || fail "${rc_file}에 PATH를 추가하지 못했습니다."
      printf 'PATH 설정 추가: %s\n' "$rc_file"
    fi
    ;;
esac

printf '\nkit %s 설치 완료: %s\n' "$version" "$INSTALL_PATH"
if [ "$path_changed" -eq 1 ]; then
  printf '새 terminal부터 kit 명령을 바로 사용할 수 있습니다. 현재 shell에서는 다음 경로를 사용하세요:\n  %s\n' "$INSTALL_PATH"
fi
