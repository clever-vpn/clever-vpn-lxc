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
    local pool_size="${STORAGE_POOL_SIZE:-10}"

    log_info "Initializing LXD with btrfs (${pool_size}GiB loop)..."

    # Always clean up before re-creating
    if lxc storage show default &>/dev/null 2>&1; then
        log_info "Removing existing storage pool 'default'..."
        lxc profile device remove default root 2>/dev/null || true
        lxc storage delete default 2>/dev/null || true
    fi
    if lxc network show lxdbr0 &>/dev/null 2>&1; then
        log_info "Removing existing lxdbr0 network..."
        lxc network delete lxdbr0 2>/dev/null || true
    fi
    rm -f /var/snap/lxd/common/lxd/disks/default.img

    # Reset LXD database if previous init left stale state
    if lxd init --auto --storage-backend=btrfs --storage-create-loop="${pool_size}" 2>&1 | grep -q "already been configured"; then
        log_info "Resetting stale LXD configuration..."
        systemctl stop snap.lxd.daemon 2>/dev/null || true
        rm -f /var/snap/lxd/common/lxd/database/local.db
        systemctl start snap.lxd.daemon 2>/dev/null || true
        sleep 2
        lxd init --auto --storage-backend=btrfs --storage-create-loop="${pool_size}"
    fi
    log_info "LXD initialized with btrfs ${pool_size}GiB"
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
        if ! lxc network get "$CONTAINER_NETWORK" raw.dnsmasq &>/dev/null; then
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

# Allow Manager to reach LXD API from remote.
setup_ufw_allow_lxd_api() {
    if ! command -v ufw &>/dev/null; then
        return 0
    fi
    if ! ufw status 2>/dev/null | grep -q 'Status: active'; then
        return 0
    fi
    if ufw status verbose 2>/dev/null | grep -q '8443/tcp'; then
        return 0
    fi
    ufw allow 8443/tcp comment 'LXD HTTPS API'
    log_info "UFW: allowed LXD API port 8443/tcp"
}

# Allow container port ranges for external access (SSH and service ports).
setup_ufw_container_ports() {
    if ! command -v ufw &>/dev/null; then
        return 0
    fi
    if ! ufw status 2>/dev/null | grep -q 'Status: active'; then
        return 0
    fi
    ufw allow 22000:22999/tcp comment 'Container SSH ports' 2>/dev/null || true
    ufw allow 22000:22999/udp comment 'Container SSH ports' 2>/dev/null || true
    ufw allow 50000:54999/tcp comment 'Container service ports' 2>/dev/null || true
    ufw allow 50000:54999/udp comment 'Container service ports' 2>/dev/null || true
    log_info "UFW: allowed container port ranges"
}

# ==================== 基础镜像构建 ====================
build_base_image() {
    if lxc image show "$BASE_IMAGE_ALIAS" &>/dev/null; then
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

        # Custom shell prompt: root@clever-vpn:/path#
        echo 'export PS1="\\[\\e[1;32m\\]root@clever-vpn\\[\\e[0m\\]:\\w# "' >> /root/.bashrc
    '

    # 3. Enable IP forwarding permanently
    lxc exec "$BASE_CONTAINER_NAME" -- bash -c '
        echo "net.ipv4.ip_forward=1" >> /etc/sysctl.conf
        echo "net.ipv6.conf.all.forwarding=1" >> /etc/sysctl.conf
    '

    # 4. Stop and publish
    log_info "Publishing base image as '$BASE_IMAGE_ALIAS'..."
    lxc stop "$BASE_CONTAINER_NAME"
    lxc publish "$BASE_CONTAINER_NAME" --alias "$BASE_IMAGE_ALIAS" 2>/dev/null || {
        log_info "Image alias already exists, removing old builder"
        lxc delete "$BASE_CONTAINER_NAME" --force
        return 0
    }
    lxc delete "$BASE_CONTAINER_NAME"

    log_info "Base image '$BASE_IMAGE_ALIAS' created successfully!"
}

# ==================== iptables 持久化 ====================
setup_iptables_persist() {
    if dpkg -s iptables-persistent &>/dev/null; then
        log_info "iptables-persistent already installed"
        return 0
    fi

    log_info "Installing iptables-persistent for port forward persistence..."
    echo iptables-persistent iptables-persistent/autosave_v4 boolean true | debconf-set-selections
    echo iptables-persistent iptables-persistent/autosave_v6 boolean true | debconf-set-selections
    apt-get install -y iptables-persistent
}

# ==================== 环境清理 ====================
# Ensure a clean starting point regardless of previous state.
cleanup_containers() {
    log_info "Removing all existing LXD containers..."
    lxc list --format csv -c n 2>/dev/null | while read c; do
        [[ -n "$c" ]] && lxc delete "$c" --force 2>/dev/null || true
    done
}

cleanup_iptables() {
    log_info "Flushing all DNAT rules..."
    iptables -t nat -F PREROUTING 2>/dev/null || true
    iptables -t nat -F POSTROUTING 2>/dev/null || true
    iptables -t nat -F OUTPUT 2>/dev/null || true
}

# ==================== LXD HTTPS / Manager trust ====================
MANAGER_CERT="${MANAGER_CERT:-}"

setup_lxd_remote() {
    if [[ -z "$MANAGER_CERT" ]]; then
        log_info "No --manager-cert provided, skipping LXD HTTPS setup"
        return 0
    fi

    log_info "Enabling LXD HTTPS API on :8443..."
    lxc config set core.https_address :8443 2>/dev/null || true

    log_info "Adding Manager certificate to trust store..."
    lxc config trust add "$MANAGER_CERT" --type=client --restricted=false 2>/dev/null || true
    log_info "Manager certificate trust ensured"
}

# ==================== 主流程 ====================
main() {
    echo ""
    echo "============================================"
    echo "  Clever VPN - LXC Host Setup"
    echo "============================================"
    echo ""

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --rebuild)
                log_warn "Rebuilding base image..."
                lxc image delete "$BASE_IMAGE_ALIAS" 2>/dev/null || true
                ;;
            --manager-cert)
                MANAGER_CERT="$2"
                shift
                ;;
            --manager-cert=*)
                MANAGER_CERT="${1#*=}"
                ;;
        esac
        shift
    done

    check_root
    check_ubuntu
    check_kernel
    install_lxd
    cleanup_containers
    cleanup_iptables
    init_lxd
    setup_network
    setup_ufw
    setup_kernel_config
    setup_lxd_remote
    setup_ufw_allow_lxd_api
    setup_ufw_container_ports
    build_base_image
    setup_iptables_persist

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
