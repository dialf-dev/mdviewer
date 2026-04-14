#!/usr/bin/env bash
# install.sh — build and install mdv into /usr/local/bin
#
# Usage:
#   ./install.sh           # build + install to /usr/local/bin (uses sudo)
#   ./install.sh build     # build only (produces ./mdv)
#   ./install.sh uninstall # remove /usr/local/bin/mdv
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_NAME="mdv"
INSTALL_DIR="/usr/local/bin"
INSTALL_PATH="${INSTALL_DIR}/${BIN_NAME}"

cmd="${1:-install}"

need_sudo() {
  if [[ -w "$INSTALL_DIR" ]]; then
    echo ""
  else
    echo "sudo"
  fi
}

do_build() {
  cd "$SCRIPT_DIR"
  if ! command -v go >/dev/null 2>&1; then
    echo "error: go toolchain not found in PATH" >&2
    exit 1
  fi
  echo ">> go mod tidy"
  go mod tidy
  echo ">> go build -o ${BIN_NAME}"
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "${BIN_NAME}" .
  echo ">> built: ${SCRIPT_DIR}/${BIN_NAME}"
}

do_install() {
  do_build
  local SUDO; SUDO="$(need_sudo)"
  echo ">> installing to ${INSTALL_PATH}"
  $SUDO install -m 0755 "${SCRIPT_DIR}/${BIN_NAME}" "${INSTALL_PATH}"
  echo ">> installed: $(${INSTALL_PATH} --help 2>&1 | head -n1 || echo ok)"
  echo
  echo "done. try:  ${BIN_NAME} path/to/file.md"
}

do_uninstall() {
  local SUDO; SUDO="$(need_sudo)"
  if [[ -e "$INSTALL_PATH" ]]; then
    echo ">> removing ${INSTALL_PATH}"
    $SUDO rm -f "$INSTALL_PATH"
    echo ">> uninstalled"
  else
    echo ">> nothing to remove (${INSTALL_PATH} does not exist)"
  fi
}

case "$cmd" in
  build)     do_build ;;
  install)   do_install ;;
  uninstall) do_uninstall ;;
  *)
    echo "usage: $0 [build|install|uninstall]" >&2
    exit 2
    ;;
esac
