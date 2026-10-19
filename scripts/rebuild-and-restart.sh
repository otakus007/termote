#!/usr/bin/env bash
# ==============================================================================
# Script: rebuild-and-restart.sh
# Purpose: Rebuild PWA, embed assets into server, compile termote-dev, and
#          restart the user systemd service.
# ==============================================================================
set -euo pipefail

# Colors
C_RESET="\033[0m"
C_GREEN="\033[1;32m"
C_BLUE="\033[1;34m"
C_YELLOW="\033[1;33m"
C_RED="\033[1;31m"

info()  { echo -e "${C_BLUE}[INFO]${C_RESET} $*"; }
step()  { echo -e "${C_GREEN}[STEP]${C_RESET} $*"; }
warn()  { echo -e "${C_YELLOW}[WARN]${C_RESET} $*"; }
error() { echo -e "${C_RED}[ERROR]${C_RESET} $*" >&2; exit 1; }

# Locate project root
SRC="${BASH_SOURCE[0]}"
while [[ -L "$SRC" ]]; do
  DIR="$(cd -P "$(dirname "$SRC")" && pwd)"
  SRC="$(readlink "$SRC")"
  [[ "$SRC" != /* ]] && SRC="$DIR/$SRC"
done
SCRIPT_DIR="$(cd -P "$(dirname "$SRC")" && pwd)"
PROJECT_DIR="$(cd -P "$SCRIPT_DIR/.." && pwd)"

PWA_DIR="$PROJECT_DIR/pwa"
SERVER_DIR="$PROJECT_DIR/server"
WEBUI_DIST="$SERVER_DIR/webui/dist"
BIN_DEV="$SERVER_DIR/termote-dev"
SERVICE_NAME="termote.service"

echo -e "${C_BLUE}======================================================${C_RESET}"
echo -e "${C_BLUE}    Termote Rebuild & Restart Service Tool            ${C_RESET}"
echo -e "${C_BLUE}======================================================${C_RESET}"
info "Project Directory: $PROJECT_DIR"

# 1. Check prerequisites
for cmd in pnpm go systemctl; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    error "Lệnh '$cmd' không tồn tại trên hệ thống. Vui lòng cài đặt trước."
  fi
done

# 2. Build PWA
step "1/4: Đang build PWA (frontend)..."
(
  cd "$PROJECT_DIR"
  pnpm --filter termote build
)

# 3. Copy dist to webui embedded folder
step "2/4: Đang đồng bộ giao diện PWA vào webui/dist..."
mkdir -p "$WEBUI_DIST"
find "$WEBUI_DIST" -mindepth 1 ! -name .gitkeep -delete 2>/dev/null || true
cp -R "$PWA_DIR/dist/." "$WEBUI_DIST/"

# 4. Compile Go binary
step "3/4: Đang compile binary server (termote-dev)..."
(
  cd "$SERVER_DIR"
  CGO_ENABLED=0 go build -ldflags="-s -w" -o termote-dev .
)
info "Binary đã được tạo tại: $BIN_DEV ($(du -h "$BIN_DEV" | cut -f1))"

# 5. Restart systemd user service
step "4/4: Đang khởi động lại $SERVICE_NAME..."
systemctl --user restart "$SERVICE_NAME"

# 6. Verify service status
sleep 1
if systemctl --user is-active --quiet "$SERVICE_NAME"; then
  echo -e "${C_GREEN}✓ Service $SERVICE_NAME đã được khởi động lại thành công và đang hoạt động (active)!${C_RESET}"
  systemctl --user status "$SERVICE_NAME" --no-pager -n 5
else
  echo -e "${C_RED}✗ Service không hoạt động sau khi restart. Vui lòng kiểm tra log:${C_RESET}"
  journalctl --user-unit="$SERVICE_NAME" -n 20 --no-pager
  exit 1
fi
