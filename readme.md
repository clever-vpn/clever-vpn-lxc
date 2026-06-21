# Clever VPN - LXC Manager

单二进制 LXC 容器管理服务。通过 REST API 管理多节点 LXD 集群，支持自动 TLS (Let's Encrypt)、SSH 远程供给节点、Token 认证。

## 快速开始

```bash
# 1. 下载二进制（从 GitHub Releases）
curl -LO https://github.com/clever-vpn/clever-vpn-lxc/releases/latest/download/lxc-manager-linux-amd64
chmod +x lxc-manager-linux-amd64
mv lxc-manager-linux-amd64 /usr/local/bin/lxc-manager

# 2. 生成客户端证书（用于连接 LXD 节点）
lxc-manager cert gen

# 3. 创建管理员 token
lxc-manager admin create superadmin

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
| `lxc-manager add-node <name> <host>` | SSH 供给新节点 |
| `lxc-manager remove-node <name>` | 移除节点 |
| `lxc-manager list-nodes` | 列出所有节点 |
| `lxc-manager add-user <name>` | 创建用户 token（cvl_ 前缀） |
| `lxc-manager remove-user <name>` | 删除用户 |
| `lxc-manager list-users` | 列出用户及其容器数 |
| `lxc-manager --version` | 显示版本号 |

## REST API

### 端点

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| `GET` | `/_version` | 无 | 版本号 |
| `GET` | `/api/health` | 无 | 健康检查 |
| `POST` | `/api/nodes` | admin | 添加节点 |
| `GET` | `/api/nodes` | admin | 列出节点 |
| `DELETE` | `/api/nodes/:name` | admin | 删除节点 |
| `POST` | `/api/users` | admin | 创建用户 |
| `GET` | `/api/users` | admin | 列出用户 |
| `DELETE` | `/api/users/:name` | admin | 删除用户 |
| `POST` | `/api/containers` | user | 创建容器 |
| `GET` | `/api/containers` | user | 列出容器 |
| `GET` | `/api/containers/:name` | user | 查看容器 |
| `DELETE` | `/api/containers/:name` | user | 删除容器 |
| `PUT` | `/api/containers/:name/resize` | user | 调整规格 |

### 认证传递

- **Admin token**: POST/PUT 在 body 中传 `"adminToken":"cva_..."`，GET/DELETE 在 query 中传 `?adminToken=cva_...`
- **User token**: POST body 中传 `"token":"cvl_..."`

### 创建容器

```json
POST /api/containers
{
  "token": "cvl_xxxxxxxx",
  "plan": "free",
  "servicePort": 443,
  "node": "tokyo",
  "userData": "#cloud-config\n..."
}
```

```json
{
  "status": "creating",
  "name": "user-a1b2c3d4-...",
  "password": "generated-password",
  "ports": {
    "ssh": 22000,
    "service": 50000
  }
}
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

## 容器规格

| Plan | CPU | Memory |
|------|-----|--------|
| free | 1 | 512 MB |
| basic | 1 | 1 GB |
| pro | 2 | 2 GB |

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

```
/var/lib/clever-vpn-lxc/
├── tokens.json        ← 用户 token
├── admin-tokens.json  ← 管理员 token
├── nodes.json         ← 节点注册表
├── instances.json     ← 容器实例注册表
└── certs/
    ├── acme_account+key
    └── your-domain.com
```

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

- **输入版本号**：检查 tag 不存在 → 构建 amd64/arm64 → 打 tag → 发布到 GitHub Releases
- **留空自动升降**：取最新 tag 的 patch 位 +1 作为新版本

构建时通过 ldflags 注入版本号：`-ldflags "-X main.version=v1.2.3"`

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

