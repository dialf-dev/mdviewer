#!/usr/bin/env bash
# install.sh — build and install the mdv document viewer service
#
# Usage:
#   ./install.sh           # build + install binary + systemd service (uses sudo)
#   ./install.sh build     # build only (produces ./mdv)
#   ./install.sh uninstall # stop/remove the service and binary
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_NAME="mdv"
INSTALL_DIR="/usr/local/bin"
INSTALL_PATH="${INSTALL_DIR}/${BIN_NAME}"
UNIT_PATH="/etc/systemd/system/mdv.service"
CONFIG_DIR="/etc/mdv"
UPLOAD_DIR="/var/lib/mdv/upload"

cmd="${1:-install}"

need_sudo() {
  if [[ $EUID -eq 0 ]]; then
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
  echo ">> creating ${CONFIG_DIR} and ${UPLOAD_DIR}"
  $SUDO mkdir -p "${CONFIG_DIR}" "${UPLOAD_DIR}"
  echo ">> installing systemd unit: ${UNIT_PATH}"
  $SUDO tee "${UNIT_PATH}" >/dev/null <<EOF
[Unit]
Description=mdv markdown document viewer
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_PATH} serve
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable --now mdv
  echo
  echo "done. the mdv service is running (localhost:9000 by default). try:"
  echo "  mdv status            # check the service"
  echo "  mdv add               # register the current directory"
  echo "  mdv path/to/file.md   # open a file in the viewer"
  echo "  mdv on                # allow external (LAN) access"
}

do_uninstall() {
  local SUDO; SUDO="$(need_sudo)"
  if systemctl list-unit-files mdv.service >/dev/null 2>&1; then
    echo ">> stopping and disabling mdv.service"
    $SUDO systemctl disable --now mdv 2>/dev/null || true
  fi
  if [[ -e "$UNIT_PATH" ]]; then
    echo ">> removing ${UNIT_PATH}"
    $SUDO rm -f "$UNIT_PATH"
    $SUDO systemctl daemon-reload
  fi
  if [[ -e "$INSTALL_PATH" ]]; then
    echo ">> removing ${INSTALL_PATH}"
    $SUDO rm -f "$INSTALL_PATH"
  fi
  echo ">> uninstalled"
  echo "note: ${CONFIG_DIR} and /var/lib/mdv (uploads) were kept; remove manually if desired"
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
