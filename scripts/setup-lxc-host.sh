#!/usr/bin/env bash
set -euo pipefail

# ============================================================
# clever-vpn-lxc: One-click LXD host setup
#
# Usage: sudo bash setup-lxc-host.sh
#
# This script:
#   1. Installs & configures LXD
#   2. Creates a base Debian 12 image with VPN dependencies
#   3. Sets up network bridge for containers
# ============================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

# ==================== 环境检查 ====================
check_root() {
    if [[ $EUID -ne 0 ]]; then
        log_error "This script must be run as root. Use: sudo bash setup-lxc-host.sh"
        exit 1
    fi
}

check_ubuntu() {
    if [[ ! -f /etc/os-release ]]; then
        log_error "Cannot detect OS. Ubuntu 22.04+ required."
        exit 1
    fi
    source /etc/os-release
    if [[ "$ID" != "ubuntu" ]]; then
        log_error "This script requires Ubuntu. Detected: $ID"
        exit 1
    fi
    log_info "Detected: $NAME $VERSION_ID"
}

check_kernel() {
    local major minor
    major=$(uname -r | cut -d. -f1)
    minor=$(uname -r | cut -d. -f2)
    if [[ $major -lt 5 || ($major -eq 5 && $minor -lt 4) ]]; then
        log_error "Kernel >= 5.4 required for eBPF support. Current: $(uname -r)"
        exit 1
    fi
    log_info "Kernel: $(uname -r) ✓"
}

# ==================== LXD 安装 ====================
install_lxd() {
    if command -v lxd &>/dev/null; then
        log_info "LXD already installed: $(lxd --version)"
        return 0
    fi

    log_info "Installing LXD via snap..."
    snap install lxd
    log_info "LXD installed: $(lxd --version)"
}

init_lxd() {
    if lxc network list 2>/dev/null | grep -q lxdbr0; then
        log_info "LXD already initialized"
        return 0
    fi

    log_info "Initializing LXD with defaults..."
    lxd init --auto
    log_info "LXD initialized"
}

# ==================== 默认配置 ====================
CONTAINER_NETWORK="${CONTAINER_NETWORK:-vpnbr0}"
CONTAINER_SUBNET="${CONTAINER_SUBNET:-10.0.1.1/24}"
BASE_IMAGE_ALIAS="${BASE_IMAGE_ALIAS:-clever-vpn-base}"
BASE_CONTAINER_NAME="${BASE_CONTAINER_NAME:-vpn-base-builder}"

# ==================== 网络配置 ====================
setup_network() {
    if lxc network list 2>/dev/null | grep -q "$CONTAINER_NETWORK"; then
        log_info "Network '$CONTAINER_NETWORK' already exists"
        # Ensure DNS is configured on existing network
        lxc network set "$CONTAINER_NETWORK" dns.mode dynamic 2>/dev/null || true
        return 0
    fi

    log_info "Creating container network '$CONTAINER_NETWORK' ($CONTAINER_SUBNET)..."
    lxc network create "$CONTAINER_NETWORK" \
        ipv4.address="$CONTAINER_SUBNET" \
        ipv4.nat=true \
        dns.mode=dynamic
    log_info "Network created"
}

# ==================== 基础镜像构建 ====================
build_base_image() {
    if lxc image list 2>/dev/null | grep -q "$BASE_IMAGE_ALIAS"; then
        log_info "Base image '$BASE_IMAGE_ALIAS' already exists. Use --rebuild to recreate."
        return 0
    fi

    log_info "Building base image from Debian 12 cloud..."
    
    # Clean up any leftover builder from previous failed runs
    lxc delete "$BASE_CONTAINER_NAME" --force 2>/dev/null || true
    
    # 1. Launch temp container
    lxc launch images:debian/12/cloud "$BASE_CONTAINER_NAME" --network "$CONTAINER_NETWORK"
    log_info "Waiting for container to boot..."
    sleep 5

    # 2. Install dependencies
    log_info "Installing VPN server dependencies..."
    lxc exec "$BASE_CONTAINER_NAME" -- bash -c '
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y --no-install-recommends \
            wireguard-tools \
            nftables \
            curl \
            ca-certificates \
            openssh-server
        apt-get clean
        rm -rf /var/lib/apt/lists/*
        
        # Configure SSH: allow both key and password auth
        sed -i "s/#PermitRootLogin prohibit-password/PermitRootLogin yes/" /etc/ssh/sshd_config
        sed -i "s/#PasswordAuthentication yes/PasswordAuthentication yes/" /etc/ssh/sshd_config
        systemctl enable ssh
    '

    # 3. Enable IP forwarding permanently
    lxc exec "$BASE_CONTAINER_NAME" -- bash -c '
        echo "net.ipv4.ip_forward=1" >> /etc/sysctl.conf
        echo "net.ipv6.conf.all.forwarding=1" >> /etc/sysctl.conf
    '

    # 4. Stop and publish
    log_info "Publishing base image as '$BASE_IMAGE_ALIAS'..."
    lxc stop "$BASE_CONTAINER_NAME"
    lxc publish "$BASE_CONTAINER_NAME" --alias "$BASE_IMAGE_ALIAS"
    lxc delete "$BASE_CONTAINER_NAME"
    
    log_info "Base image '$BASE_IMAGE_ALIAS' created successfully!"
}

# ==================== iptables 持久化 ====================
setup_iptables_persist() {
    if dpkg -l | grep -q iptables-persistent; then
        log_info "iptables-persistent already installed"
        return 0
    fi

    log_info "Installing iptables-persistent for port forward persistence..."
    echo iptables-persistent iptables-persistent/autosave_v4 boolean true | debconf-set-selections
    echo iptables-persistent iptables-persistent/autosave_v6 boolean true | debconf-set-selections
    apt-get install -y iptables-persistent
}

# ==================== Go API 服务部署 ====================
install_go_api() {
    local service_file="/opt/clever-vpn-lxc/clever-vpn-lxc.service"
    if systemctl is-active --quiet clever-vpn-lxc 2>/dev/null; then
        log_info "Go API service already running"
        return 0
    fi

    log_info "Installing Go API service..."
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    
    # Copy service file from repo
    if [[ -f "$SCRIPT_DIR/clever-vpn-lxc.service" ]]; then
        cp "$SCRIPT_DIR/clever-vpn-lxc.service" "$service_file"
    else
        log_error "Service file not found at $SCRIPT_DIR/clever-vpn-lxc.service"
        return 1
    fi

    # Enable and start
    systemctl daemon-reload
    systemctl enable clever-vpn-lxc
    systemctl start clever-vpn-lxc
    log_info "Go API service started on :8080"
}

# ==================== 主流程 ====================
main() {
    echo ""
    echo "============================================"
    echo "  Clever VPN - LXC Host Setup"
    echo "============================================"
    echo ""

    if [[ "${1:-}" == "--rebuild" ]]; then
        log_warn "Rebuilding base image..."
        lxc image delete "$BASE_IMAGE_ALIAS" 2>/dev/null || true
    fi

    check_root
    check_ubuntu
    check_kernel
    install_lxd
    init_lxd
    setup_network
    build_base_image
    setup_iptables_persist
    install_go_api

    echo ""
    echo "============================================"
    echo "  Setup Complete!"
    echo "============================================"
    echo ""
    echo "  Network:     $CONTAINER_NETWORK ($CONTAINER_SUBNET)"
    echo "  Base image:  $BASE_IMAGE_ALIAS"
    echo ""
    echo "  Next steps:"
    echo "    lxc init $BASE_IMAGE_ALIAS user-<id> -n $CONTAINER_NETWORK"
    echo "    lxc config set user-<id> limits.cpu=1 limits.memory=512MB"
    echo "    lxc start user-<id>"
    echo "    lxc exec user-<id> -- curl -L https://...install.sh | bash -s v2.1.0 token"
    echo ""
}

main "$@"
