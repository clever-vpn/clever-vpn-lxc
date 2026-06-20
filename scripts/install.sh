#!/usr/bin/env bash
set -euo pipefail

# ============================================================
# clever-vpn-lxc: One-click installer
#
# Usage:
#   bash -c "$(curl -L https://.../install.sh)" -t "ghp_xxx"
#
# This script:
#   1. Installs Go via snap
#   2. Clones the private clever-vpn-lxc repo using the token
#   3. Builds the Go API
#   4. Runs setup-lxc-host.sh
# ============================================================

INSTALL_DIR="/opt/clever-vpn-lxc"
REPO="https://github.com/clever-vpn/clever-vpn-lxc.git"

# ==================== Parse args ====================
TOKEN=""
while getopts "t:" opt; do
    case $opt in
        t) TOKEN="$OPTARG" ;;
        *) ;;
    esac
done

if [[ -z "$TOKEN" ]]; then
    echo "Usage: bash install.sh -t <GITHUB_TOKEN>"
    echo ""
    echo "  GITHUB_TOKEN: GitHub personal access token with repo access"
    echo "  Create one at: https://github.com/settings/tokens"
    echo ""
    echo "  Example:"
    echo "    bash -c \"\$(curl -L https://.../install.sh)\" - -t ghp_xxx"
    exit 1
fi

echo "============================================"
echo "  Clever VPN - LXC Controller Installer"
echo "============================================"
echo ""

# ==================== Checks ====================
if [[ $EUID -ne 0 ]]; then
    echo "ERROR: Must run as root. Use: sudo bash install.sh -t <token>"
    exit 1
fi

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
    echo "[2/4] Cloning private repo..."
    AUTH_REPO="https://${TOKEN}@github.com/clever-vpn/clever-vpn-lxc.git"
    git clone "$AUTH_REPO" "$INSTALL_DIR"
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
echo "  Create a container:"
echo '    curl -X POST http://localhost:8080/api/containers \'
echo '      -H "Content-Type: application/json" \'
echo '      -d '"'"'{"name":"user-101","userId":101,"plan":"basic","version":"v2.1.0","token":"eyJ...","sshKey":"ssh-rsa ..."}'"'"''
echo ""
