#!/usr/bin/env bash
set -euo pipefail

# ============================================================
# clever-vpn-lxc: One-click installer
#
# Usage:
#   bash -c "$(curl -L https://raw.githubusercontent.com/clever-vpn/clever-vpn-lxc/main/scripts/install.sh)"
#
# This script:
#   1. Installs Go via snap
#   2. Clones the clever-vpn-lxc repo
#   3. Builds the Go API
#   4. Runs setup-lxc-host.sh
# ============================================================

INSTALL_DIR="/opt/clever-vpn-lxc"
REPO="https://github.com/clever-vpn/clever-vpn-lxc.git"

if [[ $EUID -ne 0 ]]; then
    echo "ERROR: Must run as root. Use: sudo bash install.sh"
    exit 1
fi

echo "============================================"
echo "  Clever VPN - LXC Controller Installer"
echo "============================================"
echo ""

# ==================== Install Go ====================
if command -v go &>/dev/null; then
    echo "[1/4] Go already installed: $(go version)"
else
    echo "[1/4] Installing Go..."
    snap install go --classic
    echo "Go installed: $(go version)"
fi

# ==================== Clone repo ====================
if [[ -d "$INSTALL_DIR/.git" ]]; then
    echo "[2/4] Repo already exists, pulling latest..."
    cd "$INSTALL_DIR"
    git pull origin main
else
    echo "[2/4] Cloning repo..."
    git clone "$REPO" "$INSTALL_DIR"
    cd "$INSTALL_DIR"
fi

# ==================== Build ====================
echo "[3/4] Building Go API..."
go build -o clever-vpn-lxc ./cmd/server

# ==================== Setup ====================
echo "[4/4] Running setup (LXD + base image + service)..."
bash scripts/setup-lxc-host.sh

echo ""
echo "============================================"
echo "  Installation Complete!"
echo "============================================"
echo ""
echo "  API running on: http://localhost:8080"
echo ""
