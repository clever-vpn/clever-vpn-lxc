# Clever VPN - LXC Controller

## One-Click Install (Ubuntu 22.04+)

```bash
bash -c "$(curl -L https://raw.githubusercontent.com/clever-vpn/clever-vpn-lxc/main/scripts/install.sh)"
```

This will:
1. Install Go
2. Clone this private repo
3. Build the Go API
4. Install LXD, create base image, start systemd service

## API Reference

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/containers` | Create container |
| `GET` | `/api/containers` | List all user containers |
| `GET` | `/api/containers/{name}` | Get container details |
| `PUT` | `/api/containers/{name}/resize` | Change container plan |
| `DELETE` | `/api/containers/{name}` | Destroy container |
| `GET` | `/api/health` | Health check |

### POST /api/containers

```json
{
  "plan": "basic",
  "userId": 101,
  "password": "optional-initial-password",
  "sshKey": "ssh-ed25519 AAAA... user@example",
  "installScriptUrl": "https://raw.githubusercontent.com/clever-vpn/clever-vpn-server/main/install.sh",
  "version": "v2.1.4",
  "token": "eyJ..."
}
```

```json
{
  "status": "creating",
  "name": "user-a1b2c3d4-...",
  "password": "generated-or-requested-password",
  "ports": {
    "ssh": 20101,
    "vpn": 10101
  }
}
```

## Provisioning Model

`clever-vpn-base` is the reusable base image. It should contain only shared packages and common runtime dependencies.

Per-user values are injected at instance creation time through `cloud-init`, including:
1. Root password
2. User SSH public key
3. Install script URL
4. Application version and token
5. Instance metadata written to `/etc/clever-vpn/bootstrap.env`

During first boot, the controller runs the install script from cloud-init. For the Clever VPN server installer, that is the automation-friendly equivalent of:

```bash
curl -fsSL https://raw.githubusercontent.com/clever-vpn/clever-vpn-server/main/install.sh | bash -s -- v2.1.4 <token>
```

This is equivalent to the common interactive form based on `bash -c "$(curl -L ...)" @ <version> <token>`, but is easier to quote safely inside automation.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LXD_SOCKET` | `/var/snap/lxd/common/lxd/unix.socket` | LXD Unix socket path |
| `LXC_BASE_IMAGE` | `clever-vpn-base` | Base image alias |
| `LXC_NETWORK` | `vpnbr0` | Container network bridge |
| `LXC_NAME_PREFIX` | `user-` | Container name prefix |
| `PORT` | `8080` | HTTP listen port |
| `VPN_INSTALL_SCRIPT_URL` | `https://raw.githubusercontent.com/clever-vpn/clever-vpn-server/main/install.sh` | Default install script URL when `installScriptUrl` is omitted |

## Container Plans

| Plan | CPU | Memory |
|------|-----|--------|
| free | 1 | 512 MB |
| basic | 1 | 1 GB |
| pro | 2 | 2 GB |

## Architecture

```
┌─ Host (Ubuntu 22.04+) ──────────────────────────────────┐
│                                                           │
│  clever-vpn-lxc (Go, :8080)                              │
│  ├─ POST /api/containers  →  official LXD client + cloud-init │
│  ├─ GET  /api/containers  →  official LXD client         │
│  └─ ...                                                  │
│                                                           │
│  LXD (API via Unix socket)                               │
│  ├─ clever-vpn-base (image)                              │
│  ├─ user-101: 10.0.1.10                                  │
│  ├─ user-102: 10.0.1.11                                  │
│  └─ ...                                                  │
│                                                           │
│  cloud-init: password/key/install metadata               │
│  iptables DNAT: 公网:ext → user-{id}:{port}              │
└───────────────────────────────────────────────────────────┘
```

## Host Requirements

The host must be configured for LXC containers to run eBPF programs and WireGuard:

| Requirement | Config | Auto-set by |
|-------------|--------|-------------|
| WireGuard kernel module | `/etc/modules-load.d/clever-vpn.conf` | `setup-lxc-host.sh` |
| Unprivileged BPF | `kernel.unprivileged_bpf_disabled=0` in `/etc/sysctl.d/99-bpf.conf` | `setup-lxc-host.sh` |

### Container Security Settings

Every container is created with these configs automatically:

| Config | Purpose |
|--------|---------|
| `security.nesting=true` | Allow eBPF syscalls in the container |
| `security.privileged=true` | Required for BTF loading (`BPF_BTF_GET_FD_BY_ID` needs `CAP_SYS_ADMIN`) |
| `limits.kernel.memlock=unlimited` | Allow eBPF programs to lock required memory |

Without these, `clever-vpn-server` will fail with memlock / BTF errors.

## Port Pools

Two non-overlapping port pools are used for NAT forwarding:

| Pool | Range | Protocol | Reason |
|------|-------|----------|--------|
| SSH | `22000-22999` | TCP only | TCP is not restricted; 22xxx is intuitive |
| Service | `50000-54999` | TCP+UDP | >30000 avoids UDP blocking on some ISPs |

Ports are allocated from pools at container creation, persisted to `/var/lib/clever-vpn-lxc/instances.json`, and remain bound to the container until deletion.

## LXC 基本管理命令

```bash
# === 容器生命周期 ===
lxc list                                    # 列出所有容器
lxc info <name>                             # 容器详情（IP、CPU、内存）
lxc start <name>                            # 启动
lxc stop <name>                             # 停止
lxc restart <name>                          # 重启
lxc delete <name>                           # 删除（需先停止）

# === 创建容器 ===
lxc init <image> <name>                     # 从镜像创建（不启动）
lxc launch <image> <name>                   # 创建并启动
lxc launch images:debian/12/cloud test      # 从官方镜像创建

# === 资源配置 ===
lxc config set <name> limits.cpu 2          # 2 CPU
lxc config set <name> limits.memory 1024MB  # 1GB 内存
lxc config show <name>                      # 查看配置

# === 网络 ===
lxc network list                            # 列出网络
lxc network show vpnbr0                     # 网络详情
lxc config device add <name> eth0 nic nictype=bridged parent=vpnbr0

# === 镜像管理 ===
lxc image list                              # 本地镜像
lxc image list images: debian               # 远程 Debian 镜像
lxc publish <container> --alias <name>      # 容器发布为镜像
lxc image delete <alias>                    # 删除镜像

# === 进入容器 ===
lxc exec <name> -- bash                     # 交互 shell
lxc exec <name> -- systemctl status         # 执行命令

# === 文件操作 ===
lxc file pull <name>/path /local/path       # 从容器拉文件
lxc file push /local/path <name>/path       # 推文件到容器

# === 快照与恢复 ===
lxc snapshot <name> <snap-name>             # 创建快照
lxc restore <name> <snap-name>              # 从快照恢复
lxc info <name>                             # 查看快照列表

# === 端口转发（宿主机上执行）===
VIP=$(lxc info <name> | awk '/eth0.*inet /{print $3}')
iptables -t nat -A PREROUTING -p tcp --dport 10001 -j DNAT --to $VIP:443
iptables -t nat -A PREROUTING -p udp --dport 10001 -j DNAT --to $VIP:443
iptables -t nat -A OUTPUT -p tcp --dport 10001 -j DNAT --to $VIP:443
iptables -t nat -A OUTPUT -p udp --dport 10001 -j DNAT --to $VIP:443
iptables-save > /etc/iptables/rules.v4
```

## GitHub Actions

Pushes `clever-vpn-base` Docker image to `ghcr.io/clever-vpn/clever-vpn-lxc`.
