# Clever VPN - LXC Controller

## Quick Start

```bash
# 1. One-time: Setup LXD host (Ubuntu 22.04+)
sudo bash scripts/setup-lxc-host.sh

# 2. Create a VPN server container
sudo bash scripts/lxc-ctl.sh create 101 basic "eyJh..."

# 3. Forward public port to container
sudo bash scripts/lxc-ctl.sh add-port 101 10001 443

# 4. Check status
sudo bash scripts/lxc-ctl.sh list
```

## Container Plans

| Plan | Memory | CPU |
|------|--------|-----|
| free | 512 MB | 1 |
| basic | 1 GB | 1 |
| pro | 2 GB | 2 |

## Architecture

```
┌─ Host (Ubuntu) ─────────────────────────────────────┐
│                                                       │
│  LXD                                                  │
│  ├─ clever-vpn-base (image, built from Debian 12)     │
│  ├─ user-101: 10.0.1.10 (free)                       │
│  ├─ user-102: 10.0.1.11 (basic)                      │
│  └─ ...                                               │
│                                                       │
│  iptables DNAT: 公网端口 → 容器内网 IP:端口             │
└───────────────────────────────────────────────────────┘
```

## GitHub Actions

Pushes `clever-vpn-base` Docker image to `ghcr.io/clever-vpn/clever-vpn-lxc`.
