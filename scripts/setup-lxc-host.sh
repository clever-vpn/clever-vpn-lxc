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
    if lxc network show "$CONTAINER_NETWORK" &>/dev/null; then
        log_info "Network '$CONTAINER_NETWORK' already exists"
        # Ensure DNS is configured on existing network.
        # LXD 6.x does not support dns.nameservers directly; use raw.dnsmasq instead.
        if ! lxc network show "$CONTAINER_NETWORK" | grep -q 'raw.dnsmasq'; then
            lxc network set "$CONTAINER_NETWORK" raw.dnsmasq 'server=8.8.8.8'
        fi
        return 0
    fi

    log_info "Creating container network '$CONTAINER_NETWORK' ($CONTAINER_SUBNET)..."
    lxc network create "$CONTAINER_NETWORK" \
        ipv4.address="$CONTAINER_SUBNET" \
        ipv4.nat=true \
        raw.dnsmasq='server=8.8.8.8'
    log_info "Network created"
}

# ==================== 内核配置 ====================
setup_kernel_config() {
    log_info "Configuring kernel for eBPF support..."

    # WireGuard kernel module
    modprobe wireguard 2>/dev/null || true
    local modules_file="/etc/modules-load.d/clever-vpn.conf"
    if [[ ! -f "$modules_file" ]]; then
        echo "wireguard" > "$modules_file"
        log_info "WireGuard module auto-load enabled: $modules_file"
    fi

    # Allow unprivileged BPF (needed for LXC containers to load eBPF programs)
    local bpf_conf="/etc/sysctl.d/99-bpf.conf"
    if [[ ! -f "$bpf_conf" ]]; then
        echo "kernel.unprivileged_bpf_disabled=0" > "$bpf_conf"
        sysctl -w kernel.unprivileged_bpf_disabled=0
        log_info "Unprivileged BPF enabled: $bpf_conf"
    fi
}

# ==================== UFW 配置 ====================
setup_ufw() {
    if ! command -v ufw &>/dev/null; then
        return 0
    fi
    if ! ufw status 2>/dev/null | grep -q 'Status: active'; then
        return 0
    fi

    # LXD containers need DHCP/DNS from the bridge; UFW must allow traffic on bridge interfaces.
    # See: https://canonical.com/lxd/docs/latest/howto/network_bridge_firewalld/
    for br in "$CONTAINER_NETWORK" lxdbr0; do
        if lxc network show "$br" &>/dev/null; then
            ufw allow in on "$br" 2>/dev/null || true
            ufw route allow in on "$br" 2>/dev/null || true
            ufw route allow out on "$br" 2>/dev/null || true
        fi
    done
    log_info "UFW rules added for LXD bridges"
}

# ==================== 基础镜像构建 ====================
build_base_image() {
    if lxc image list 2>/dev/null | grep -q "$BASE_IMAGE_ALIAS"; then
        log_info "Base image '$BASE_IMAGE_ALIAS' already exists. Use --rebuild to recreate."
        return 0
    fi

    # Use the cloud-enabled image so per-instance credentials and install parameters can be
    # injected at first boot via LXD cloud-init configuration.
    local IMAGE_SRC="${LXC_IMAGE_SRC:-images:debian/12/cloud}"
    log_info "Building base image from $IMAGE_SRC ..."

    # Clean up any leftover builder from previous failed runs
    lxc delete "$BASE_CONTAINER_NAME" --force 2>/dev/null || true

    # 1. Launch temp container
    lxc launch "$IMAGE_SRC" "$BASE_CONTAINER_NAME" --network "$CONTAINER_NETWORK"
    log_info "Waiting for container to boot..."
    sleep 5
    lxc exec "$BASE_CONTAINER_NAME" -- cloud-init status --wait >/dev/null 2>&1 || true

    # Verify network connectivity
    if ! lxc exec "$BASE_CONTAINER_NAME" -- ping -c 1 -W 10 8.8.8.8 &>/dev/null; then
        log_error "Container cannot reach the internet. Check UFW/firewall rules."
        log_error "Try: ufw allow in on $CONTAINER_NETWORK && ufw route allow in on $CONTAINER_NETWORK"
        lxc delete "$BASE_CONTAINER_NAME" --force 2>/dev/null || true
        return 1
    fi
    log_info "Network connectivity verified"

    # 2. Install dependencies
    log_info "Installing VPN server dependencies..."
    lxc exec "$BASE_CONTAINER_NAME" -- bash -c '
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y --no-install-recommends \
            cloud-init \
            wireguard-tools \
            nftables \
            curl \
            ca-certificates \
            openssh-server
        apt-get clean
        rm -rf /var/lib/apt/lists/*

        # Configure SSH: allow both key and password auth
        sed -ri "s/^#?PermitRootLogin.*/PermitRootLogin yes/" /etc/ssh/sshd_config
        sed -ri "s/^#?PasswordAuthentication.*/PasswordAuthentication yes/" /etc/ssh/sshd_config
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
    local service_file="/etc/systemd/system/clever-vpn-lxc.service"
    if systemctl is-active --quiet clever-vpn-lxc 2>/dev/null; then
        log_info "Go API service already running"
        return 0
    fi

    log_info "Installing Go API service..."
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    
    # Copy service file from repo to systemd
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
    setup_ufw
    setup_kernel_config
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
