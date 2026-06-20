#!/usr/bin/env bash
set -euo pipefail

# ============================================================
# clever-vpn-lxc: Container lifecycle management
#
# Usage:
#   lxc-ctl.sh create <user-id> <plan> <token>    Create container
#   lxc-ctl.sh destroy <user-id>                  Destroy container
#   lxc-ctl.sh resize <user-id> <plan>            Change container plan
#   lxc-ctl.sh list                               List all containers
#   lxc-ctl.sh status <user-id>                   Show container status
#   lxc-ctl.sh add-port <user-id> <ext-port> <int-port>
#   lxc-ctl.sh del-port <user-id> <ext-port>
#
# Plans:
#   free:  512MB / 1 CPU
#   basic: 1GB   / 1 CPU
#   pro:   2GB   / 2 CPU
# ============================================================

BASE_IMAGE="${BASE_IMAGE:-clever-vpn-base}"
NETWORK="${NETWORK:-vpnbr0}"
PREFIX="${PREFIX:-user}"

# ==================== 套餐规格 ====================
declare -A PLAN_CPU
declare -A PLAN_MEM

PLAN_CPU[free]=1
PLAN_CPU[basic]=1
PLAN_CPU[pro]=2

PLAN_MEM[free]=512
PLAN_MEM[basic]=1024
PLAN_MEM[pro]=2048

container_name() { echo "${PREFIX}-$1"; }

usage() {
    echo "Usage: $0 <command> [args...]"
    echo ""
    echo "Commands:"
    echo "  create <user-id> <plan> <token>   Create new container"
    echo "  destroy <user-id>                 Destroy container"
    echo "  resize <user-id> <plan>           Change container plan"
    echo "  list                              List all ${PREFIX}-* containers"
    echo "  status <user-id>                  Show container status"
    echo "  add-port <user-id> <ext> <int>    Add DNAT port forward"
    echo "  del-port <user-id> <ext>          Remove DNAT port forward"
    exit 1
}

# ==================== create ====================
cmd_create() {
    local uid=$1 plan=$2 token=$3
    local name cpu mem
    name=$(container_name "$uid")
    cpu=${PLAN_CPU[$plan]:-1}
    mem=${PLAN_MEM[$plan]:-512}

    if lxc info "$name" &>/dev/null; then
        echo "Container '$name' already exists."
        exit 1
    fi

    echo "Creating container: $name (plan=$plan, cpu=$cpu, mem=${mem}MB)..."
    lxc init "$BASE_IMAGE" "$name" --network "$NETWORK"
    lxc config set "$name" limits.cpu="$cpu" limits.memory="${mem}MB"
    lxc start "$name"
    sleep 3

    local vip
    vip=$(lxc info "$name" 2>/dev/null | awk '/eth0.*inet /{print $3}')
    echo "Container IP: $vip"

    echo "Running VPN server install..."
    lxc exec "$name" -- bash -c "curl -fsSL https://raw.githubusercontent.com/clever-vpn/clever-vpn-server/main/install.sh | bash -s -- 'v2.1.0' '$token'"
    
    echo "Container '$name' created successfully."
}

# ==================== destroy ====================
cmd_destroy() {
    local name
    name=$(container_name "$1")

    echo "Destroying container: $name..."
    lxc stop "$name" --force 2>/dev/null || true
    lxc delete "$name" 2>/dev/null || true
    echo "Container '$name' destroyed."
}

# ==================== resize ====================
cmd_resize() {
    local uid=$1 plan=$2
    local name cpu mem
    name=$(container_name "$uid")
    cpu=${PLAN_CPU[$plan]:-1}
    mem=${PLAN_MEM[$plan]:-512}

    echo "Resizing container '$name' to plan=$plan (cpu=$cpu, mem=${mem}MB)..."
    lxc config set "$name" limits.cpu="$cpu" limits.memory="${mem}MB"
    echo "Container '$name' resized."
}

# ==================== list ====================
cmd_list() {
    echo "VPN containers:"
    lxc list --format csv -c n,s,4,m,D "$PREFIX-"
}

# ==================== status ====================
cmd_status() {
    local name
    name=$(container_name "$1")
    lxc info "$name"
}

# ==================== port forward ====================
cmd_add_port() {
    local uid=$1 ext=$2 int=$3
    local name vip
    name=$(container_name "$uid")
    vip=$(lxc info "$name" 2>/dev/null | awk '/eth0.*inet /{print $3}')

    if [[ -z "$vip" ]]; then
        echo "Cannot get container IP. Is it running?"
        exit 1
    fi

    echo "Adding DNAT: $ext -> $vip:$int"
    iptables -t nat -A PREROUTING -p tcp --dport "$ext" -j DNAT --to "${vip}:${int}"
    iptables -t nat -A PREROUTING -p udp --dport "$ext" -j DNAT --to "${vip}:${int}"
    iptables-save > /etc/iptables/rules.v4
    echo "Port forward added."
}

cmd_del_port() {
    local uid=$1 ext=$2
    local name vip
    name=$(container_name "$uid")
    vip=$(lxc info "$name" 2>/dev/null | awk '/eth0.*inet /{print $3}')

    echo "Removing DNAT: $ext"
    iptables -t nat -D PREROUTING -p tcp --dport "$ext" -j DNAT --to "${vip}:"*  2>/dev/null || true
    iptables -t nat -D PREROUTING -p udp --dport "$ext" -j DNAT --to "${vip}:"* 2>/dev/null || true
    iptables-save > /etc/iptables/rules.v4
    echo "Port forward removed."
}

# ==================== dispatch ====================
[[ $# -lt 1 ]] && usage

case "$1" in
    create)  cmd_create "${@:2}" ;;
    destroy) cmd_destroy "${@:2}" ;;
    resize)  cmd_resize "${@:2}" ;;
    list)    cmd_list ;;
    status)  cmd_status "${@:2}" ;;
    add-port) cmd_add_port "${@:2}" ;;
    del-port) cmd_del_port "${@:2}" ;;
    *)       usage ;;
esac
