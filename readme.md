# Clever VPN - LXC Manager

单二进制 LXC 容器管理服务。通过 REST API 管理多节点 LXD 集群，支持自动 TLS (Let's Encrypt)、SSH 远程供给节点、Token 认证。

## 快速开始

```bash
# 1. 下载二进制（从 GitHub Releases）
curl -LO https://github.com/clever-vpn/clever-vpn-lxc/releases/latest/download/lxc-manager-amd64-latest.gz
gunzip lxc-manager-amd64-latest.gz
chmod +x lxc-manager-amd64-latest
mv lxc-manager-amd64-latest /usr/local/bin/lxc-manager

# 2. 生成客户端证书（用于连接 LXD 节点）
lxc-manager cert gen

# 3. 管理员登录获取 token
curl -X POST https://lxc-api.clever-clouds.com/api/admin/login \
  -H "Content-Type: application/json" \
  -d '{"password": "your-admin-password"}'

# 响应: {"adminToken": "cva_xxx..."}

# 4. 安装并启动（带自动 TLS）
lxc-manager install --domain your-domain.com
```

## CLI 命令

| 命令 | 说明 |
|------|------|
| `lxc-manager serve [--domain DOMAIN] [--port PORT]` | 启动 HTTP API 服务 |
| `lxc-manager install [--domain DOMAIN]` | 安装为 systemd 服务 |
| `lxc-manager uninstall` | 卸载 systemd 服务 |
| `lxc-manager cert gen` | 生成 client.crt + client.key |
| `lxc-manager admin create <name>` | 创建管理员 token（cva_ 前缀） |
| `lxc-manager add-node <name> <host> <region>` | SSH 供给新节点到指定区域 |
| `lxc-manager remove-node <id或name>` | 移除节点 |
| `lxc-manager list-nodes` | 列出所有节点 |
| `lxc-manager add-user <name>` | 创建用户（返回 userID + token） |
| `lxc-manager remove-user <id或name>` | 删除用户（销毁其所有容器） |
| `lxc-manager reset-user-token <id或name>` | 重置用户 token |
| `lxc-manager rename-user <id或name> <新名称>` | 修改用户名称 |
| `lxc-manager list-users` | 列出用户（ID / 名称 / 容器数） |
| `lxc-manager version` | 显示版本号 |
| `lxc-manager update [--tag v1.0.0]` | 从 GitHub Releases 自更新 |
| `lxc-manager backup` | 手动备份数据到 R2/S3 |
| `lxc-manager restore` | 从 R2/S3 恢复数据 |

## REST API

### 端点

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| `GET` | `/_version` | 无 | 版本号 |
| `GET` | `/api/health` | 无 | 健康检查 |
| `POST` | `/api/admin/login` | 无 | 管理员登录（密码 → token） |
| `GET` | `/api/regions` | 无 | 可用区域列表 |
| `POST` | `/api/nodes` | admin | 添加节点 |
| `GET` | `/api/nodes` | admin | 列出节点 |
| `PUT` | `/api/nodes/:id` | admin | 更新节点配置（密码/端口/池大小/容量） |
| `POST` | `/api/nodes/:id/migrate` | admin | 迁移节点到新机器 |
| `POST` | `/api/nodes/:id/rebuild` | admin | 重建节点（重新初始化并恢复容器） |
| `GET` | `/api/nodes/:id/containers` | admin | 节点上所有容器 |
| `DELETE` | `/api/nodes/:id` | admin | 删除节点（容器数必须为 0） |
| `POST` | `/api/admin/containers/:name/migrate` | admin | 迁移容器到指定节点 |
| `POST` | `/api/users` | admin | 创建用户（返回 userID + name + token） |
| `GET` | `/api/users` | admin | 列出用户（id / name / containers） |
| `DELETE` | `/api/users/:id` | admin | 删除用户（销毁所有容器） |
| `PUT` | `/api/users/:id/token` | admin | 重置用户 token |
| `PUT` | `/api/users/:id/name` | admin | 修改用户名称 |
| `POST` | `/api/containers` | user | 创建容器（仅自己的） |
| `GET` | `/api/containers` | user | 列出我的容器 |
| `GET` | `/api/containers/:name` | user | 查看容器（仅自己的） |
| `DELETE` | `/api/containers/:name` | user | 删除容器（仅自己的） |
| `PUT` | `/api/containers/:name/resize` | user | 调整规格（仅自己的） |

### 认证传递

所有需要认证的接口统一使用 HTTP 头：

```
Authorization: Bearer <token>
```

- **Admin token** (`cva_` 前缀)：通过 `POST /api/admin/login` 用密码换取
- **User token** (`cvl_` 前缀)：管理员通过 `POST /api/users` 创建

### 创建容器

```json
POST /api/containers
Authorization: Bearer cvl_xxxxxxxx
{
  "cpu": 1,
  "mem": 512,
  "disk": 10,
  "servicePort": 443,
  "region": "tokyo",
  "userData": "#cloud-config\n..."
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `cpu` | int | ❌ | CPU 核数，默认 1 |
| `mem` | int | ❌ | 内存 (MB)，默认 512 |
| `disk` | int | ❌ | 磁盘上限 (GB)，0 或不传 = 不受限 |
| `servicePort` | int | ✅ | 容器内服务端口 (1-65535) |
| `region` | string | ❌ | 区域名，同区域多节点轮询分配 |
| `userData` | string | ❌ | cloud-init 配置，空则自动设密码 |

```json
{
  "status": "creating",
  "name": "user-a1b2c3d4-...",
  "password": "generated-password",
  "cpu": 1,
  "mem": 512,
  "disk": 10,
  "ports": {
    "ssh": 22000,
    "service": 50000
  }
}
```

### 灾难恢复

节点删除时容器自动标记为 `lost`。重新添加同 IP 的节点后，系统自动恢复所有丢失的容器（保留端口和 Internal IP）。

```json
POST /api/nodes
Authorization: Bearer cva_xxxxxxxx
{
  "name":        "node-nrt",
  "region":      "nrt",
  "sshHost":     "45.77.16.224",
  "sshPort":     22,
  "sshPassword": "...",
  "poolSize":    "10"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `nodeName` | string | ✅ | 节点名称 |
| `region` | string | ✅ | 区域 |
| `sshHost` | string | ✅ | SSH 地址（同为公网 IP） |
| `sshPort` | int | ❌ | SSH 端口，默认 22 |
| `sshPassword` | string | ✅ | root 密码 |
| `poolSize` | string | ❌ | btrfs 池大小 (GiB)，默认 10 |
| `maxContainers` | int | ❌ | 最大容器数，0 表示无限制 |

### 调整规格

可单独调整任一参数，0 表示保持不变。

```json
PUT /api/containers/{name}/resize
{
  "cpu": 2,
  "mem": 2048,
  "disk": 20
}
```

```json
{ "status": "resized", "cpu": 2, "mem": 2048, "disk": 20 }
```

### 添加节点

```json
POST /api/nodes
{
  "adminToken": "cva_xxxxxxxx",
  "name": "tokyo",
  "sshHost": "1.2.3.4",
  "sshPassword": "..."
}
```

## 端口分配

| 池 | 范围 | 协议 |
|----|------|------|
| SSH | 22000–22999 | TCP |
| Service | 50000–54999 | TCP+UDP |

## 架构

```
┌─ Manager ────────────────────────────────────────┐
│  lxc-manager serve --domain api.example.com      │
│  :443 (autocert Let's Encrypt)                   │
│  :80  (HTTP→HTTPS redirect + ACME)               │
│  token auth (admin/user)                         │
└──────┬───────────────────────────────────────────┘
       │ HTTPS + client.crt (InsecureSkipVerify)
┌──────▼──────┐  ┌──────────┐  ┌──────────┐
│ Node tokyo  │  │ Node osaka│  │ Node ... │
│ LXD :8443   │  │ LXD :8443 │  │          │
│ vpnbr0      │  │ vpnbr0    │  │          │
│ clever-vpn- │  │ clever-   │  │          │
│ base image  │  │ vpn-base  │  │          │
└─────────────┘  └──────────┘  └──────────┘
```

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `DATA_DIR` | `/var/lib/clever-vpn-lxc` | 数据目录 |
| `LXD_CLIENT_CERT` | `client.crt` | LXD 客户端证书路径 |
| `LXD_CLIENT_KEY` | `client.key` | LXD 客户端私钥路径 |
| `LXD_URL` | `https://127.0.0.1:8443` | 默认 LXD 地址 |
| `LXC_BASE_IMAGE` | `clever-vpn-base` | 基础镜像别名 |
| `LXC_NETWORK` | `vpnbr0` | 容器网桥 |
| `LXC_NAME_PREFIX` | `user-` | 容器名前缀 |

## 数据文件

所有数据存储在 `DATA_DIR`（默认 `/var/lib/clever-vpn-lxc`）：

```
/var/lib/clever-vpn-lxc/
├── users.json         ← 用户记录（含 token）
├── admin-tokens.json  ← 管理员 token
├── admin-tokens.json  ← 管理员 token
├── nodes.json         ← 节点注册表
├── instances.json     ← 容器实例注册表
├── regions.json       ← 区域定义
├── plans.json         ← 套餐定义
└── certs/
    ├── acme_account+key
    └── your-domain.com
```

所有数据文件统一格式：`{ "version": 1, "records": [...] }`

### users.json

```json
{
  "version": 1,
  "records": [
    { "id": "u_a1b2c3d4", "name": "alice", "tokens": ["cvl_abc123..."] },
    { "id": "u_e5f6g7h8", "name": "bob",   "tokens": ["cvl_def456..."] }
  ]
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 不可变唯一标识，创建时自动生成 (前缀 `u_`) |
| `name` | string | 显示名称，可通过 `rename-user` 修改 |
| `tokens` | []string | 该用户的活跃认证 token 列表 |

### nodes.json

```json
{
  "version": 1,
  "records": [
    {
      "id": "nd_abc123",
      "name": "vultr-nrt",
      "region": "nrt",
      "url": "https://192.168.1.10:8443",
      "network": "vpnbr0",
      "sshHost": "192.168.1.10",
      "sshPort": 22,
      "image": "clever-vpn-base",
      "poolSize": "10",
      "status": "active",
      "maxContainers": 0
    }
  ]
}
```

### instances.json

```json
{
  "version": 1,
  "records": [
    {
      "id": "user-3f8a1b2c",
      "cpu": 1, "mem": 512, "disk": 10,
      "servicePort": 443,
      "sshExtPort": 22001, "serviceExtPort": 50001,
      "userID": "u_a1b2c3d4",
      "password": "Ab3Xy9...",
      "nodeID": "nd_abc123",
      "region": "nrt",
      "staticIP": "10.0.1.100",
      "nodePublicIP": "45.77.16.224",
      "userData": "#cloud-config\n...",
      "health": "healthy",
      "created": "2026-06-21T10:30:00Z"
    }
  ]
}
```

| 字段 | 说明 |
|------|------|
| `id` | 容器名称，唯一标识 |
| `userID` | 所属用户 ID |
| `nodeID` | 所在节点 ID（空表示已丢失） |
| `region` | 区域 ID |
| `staticIP` | 容器内网 IP（10.0.1.x），端口转发目标 |
| `nodePublicIP` | 节点公网 IP，用于灾难恢复匹配 |
| `userData` | 创建时的 cloud-init 配置，用于重建容器 |
| `health` | 健康状态：`healthy`/`unhealthy`/`lost`/`stopped`/`creating` |

## 容器安全设置

每个容器自动应用：

| 配置 | 值 | 原因 |
|------|-----|------|
| `security.nesting` | `true` | 允许容器内加载 eBPF |
| `security.privileged` | `true` | BTF 需要 CAP_SYS_ADMIN |
| `limits.kernel.memlock` | `unlimited` | eBPF 内存锁定 |

## 节点要求

- Ubuntu 22.04+
- Kernel ≥ 5.4
- WireGuard 内核模块
- `kernel.unprivileged_bpf_disabled=0`

节点供给脚本已嵌入二进制，SSH 后自动执行。


端口在容器创建时从池中分配，持久化到 `/var/lib/clever-vpn-lxc/instances.json`，容器删除前一直绑定。

## GitHub Actions

### 发布二进制

手动触发 `.github/workflows/release.yml`：

- **输入版本号**：检查 tag 不存在 → 构建 amd64/arm64 → gzip 压缩 → 生成 sha256 → 打 tag → 发布到 GitHub Releases
- **留空自动升版**：取最新 tag 的 patch 位 +1 作为新版本

构建产物（`{name}-{arch}-{tag}.gz` + `.sha256`）供 `lxc-manager update` 命令下载。

构建时通过 ldflags 注入版本号：`-ldflags "-X main.version=v1.2.3"`

### 部署到 Vultr

手动触发 `.github/workflows/deploy.yml`：

1. 选择 `deploy` 或 `destroy`
2. 指定 lxc-manager 版本（默认 `latest`）

通过 Terraform 创建 Vultr VPS，cloud-init 自动安装配置 lxc-manager，Cloudflare DNS 同步 `lxc-api.clever-clouds.com` 记录。

所有密钥通过 Bitwarden Secrets Manager 注入。部署前需要准备以下内容并存入 Bitwarden：

**生成管理员密码 bcrypt 哈希**：
```bash
# Linux / macOS (需要 whois 包)
# Ubuntu/Debian: apt-get install -y whois
# macOS:         brew install whois
mkpasswd -m bcrypt "你的管理员密码"

# 或者用 Python（无需额外安装）
python3 -c '
import bcrypt
print(bcrypt.hashpw(b"your-password", bcrypt.gensalt(rounds=10)).decode())
'
# 如果提示 No module named bcrypt: pip3 install bcrypt

# 或者用 htpasswd（Apache 工具）
# Ubuntu/Debian: apt-get install -y apache2-utils
htpasswd -bnBC 10 "" "你的管理员密码" | tr -d ':\n'
```

输出类似 `$2a$10$xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx`，将这个哈希值存入 Bitwarden，GitHub Actions 部署时会将其注入 VPS 的 `/var/lib/clever-vpn-lxc/.admin-password`。

管理员在浏览器中通过 `POST /api/admin/login` 用明文密码换取 token。

## R2 数据备份

配置文件中启用后，`serve` 自动定时备份。也可以手动执行：

```bash
# 手动备份/恢复
lxc-manager backup --config /etc/lxc-manager/config.json
lxc-manager restore --config /etc/lxc-manager/config.json
```

### 配置文件 `/etc/lxc-manager/config.json`

```json
{
  "domain": "lxc-api.clever-clouds.com",
  "port": "443",
  "lxd_client_cert": "/etc/lxc-manager/client.crt",
  "lxd_client_key": "/etc/lxc-manager/client.key",
  "backup": {
    "enabled": true,
    "interval": "1h",
    "r2_endpoint": "https://<account-id>.r2.cloudflarestorage.com",
    "r2_bucket": "clever-vpn-lxc-backup",
    "r2_access_key_id": "$R2_ACCESS_KEY_ID",
    "r2_secret_access_key": "$R2_SECRET_ACCESS_KEY"
  }
}
```

- CLI 参数 `--domain` `--port` 等覆盖配置文件对应字段
- `$VAR` 语法从环境变量读取（避免密钥明文写入配置文件）
- 备份排除 `certs/` 目录

