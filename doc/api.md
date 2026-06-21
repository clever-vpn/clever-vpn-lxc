# LXC Manager REST API

Base URL: `https://<host>:<port>` (default port: `8080`, or `443` with autocert)

---

## 公共接口（无需认证）

### `GET /api/health`

健康检查。

**响应** `200`：
```json
{ "status": "ok" }
```

### `GET /_version`

获取服务端版本。

**响应** `200`：
```json
{ "version": "v1.0.0" }
```

### `POST /api/admin/login` — 管理员登录

部署时注入 bcrypt 哈希的管理员密码。登录成功后返回 admin token，用于后续所有管理接口。

**请求体**：
```json
{ "password": "your-admin-password" }
```

**响应** `200`：
```json
{ "adminToken": "cva_abc123..." }
```

**错误** `401`：
```json
{ "error": "invalid password" }
```

> 密码的 bcrypt 哈希在部署时通过 cloud-init 写入 `/var/lib/clever-vpn-lxc/.admin-password`。
> 需要更换密码时，更新该文件并重启服务即可。

---

## 用户接口（User Token 认证）

所有容器操作使用 `token` 字段认证。用户 token 由管理员通过管理接口创建。

### `POST /api/containers` — 创建容器

**请求体**：
```json
{
  "token":       "<user-token>",
  "cpu":         1,
  "mem":         512,
  "disk":        10,
  "servicePort": 8080,
  "userData":    "#cloud-config\n...",
  "region":       "tokyo"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `token` | string | ✅ | 用户 token |
| `cpu` | int | ❌ | CPU 核数，默认 1 |
| `mem` | int | ❌ | 内存限制 (MB)，默认 512 |
| `disk` | int | ❌ | 磁盘上限 (GB)，0 或不传 = 不受限 |
| `servicePort` | int | ✅ | 容器内服务端口 (1-65535) |
| `region` | string | ❌ | 区域名（如 `tokyo`），同区域多节点轮询分配 |
| `userData` | string | ❌ | cloud-init user-data；为空时自动生成密码 |

**响应** `200`：
```json
{
  "status": "creating",
  "name": "user-a1b2c3d4",
  "password": "Abc123Xyz",
  "cpu": 1,
  "mem": 512,
  "disk": 10,
  "nodeID": "nd_abc123",
  "ports": {
    "ssh": 22001,
    "service": 50001
  }
}
```

### `GET /api/containers` — 列出容器

返回所有容器（无 token 过滤，当前实现返回全部）。

**响应** `200`：
```json
[
  {
    "name": "user-a1b2c3d4",
    "status": "Running",
    "ipv4": "10.0.0.100",
    "...": "..."
  }
]
```

### `GET /api/containers/{name}` — 获取容器详情

**响应** `200`：LXD 容器信息对象

### `PUT /api/containers/{name}/resize` — 调整容器规格

可单独调整 CPU、内存或磁盘，传 0 表示保持不变。

**请求体**：
```json
{ "cpu": 2, "mem": 2048, "disk": 20 }
```

**响应** `200`：
```json
{ "status": "resized", "cpu": 2, "mem": 2048, "disk": 20 }
```

### `DELETE /api/containers/{name}` — 删除容器

停止并删除容器，清理端口转发和注册信息。

**响应** `200`：
```json
{ "status": "deleted" }
```

---

## 管理接口（Admin Token 认证）

所有管理操作需要 `adminToken` 认证。先通过 `POST /api/admin/login` 用密码换取 token，
再携带 token 调用后续接口。对于 `GET`/`DELETE` 请求，通过 query 参数传递；对于 `POST`/`PUT` 请求，通过请求体传递。CLI 也可用 `lxc-manager admin create <name>` 创建 token。

### `POST /api/nodes` — 添加节点

**请求体**：
```json
{
  "adminToken":  "<admin-token>",
  "name":        "tokyo-1",
  "region":      "tokyo",
  "sshHost":     "192.168.1.10",
  "sshPort":     22,
  "sshPassword": "password"
}
```

| 字段 | 说明 |
|------|------|
| `name` | 人类可读名称，必须唯一 |
| `region` | 地理位置（如 `tokyo`、`ewr`），可多个节点共享 |
| `sshHost` | LXD 宿主机 IP |
| `sshPort` | SSH 端口（默认 22） |
| `sshPassword` | root 密码 |

**响应** `200`：
```json
{
  "status": "ready",
  "id":     "nd_abc123",
  "name":   "tokyo-1",
  "region": "tokyo",
  "url":    "https://192.168.1.10:8443"
}
```

### `GET /api/nodes?adminToken=<token>` — 列出所有节点

**响应** `200`：
```json
[
  {
    "id":      "nd_abc123",
    "name":    "tokyo-1",
    "region":  "tokyo",
    "url":     "https://192.168.1.10:8443",
    "network": "vpnbr0",
    "sshHost": "192.168.1.10",
    "sshPort": 22,
    "image":   "clever-vpn-base"
  }
]
```

### `GET /api/nodes/{id}/containers?adminToken=<token>` — 查询节点上所有容器

**响应** `200`：
```json
[
  {
    "name":   "user-abc123",
    "userID": "u_xyz",
    "plan":   { "cpu": 1, "mem": 512, "disk": 10 },
    "ports":  { "ssh": 22001, "service": 50001 }
  }
]
```

### `DELETE /api/nodes/{id}?adminToken=<token>` — 删除节点

**响应** `200`：
```json
{ "status": "removed", "nodeID": "nd_abc123" }
```

### `POST /api/users` — 创建用户

创建用户时自动生成不可变的 `userID` 和认证 `token`。用户名称可后续修改。

**请求体**：
```json
{
  "adminToken": "<admin-token>",
  "name":       "alice"
}
```

**响应** `200`：
```json
{
  "userID": "u_a1b2c3d4e5f6",
  "name":   "alice",
  "token":  "cvl_abc123..."
}
```

### `GET /api/users?adminToken=<token>` — 列出所有用户

**响应** `200`：
```json
[
  {
    "id":         "u_a1b2c3d4e5f6",
    "name":       "alice",
    "containers": 3
  }
]
```

### `DELETE /api/users/{userID}?adminToken=<token>` — 删除用户

删除用户将同时销毁其名下所有容器，清理所有 token。支持传入 userID 或 name。

**响应** `200`：
```json
{
  "status": "deleted",
  "userID": "u_a1b2c3d4e5f6"
}
```

### `PUT /api/users/{userID}/token` — 重置用户 Token

用户忘记或丢失 token 时，管理员可为其重新生成。旧 token 立即失效。支持传入 userID 或 name。

**请求体**：
```json
{
  "adminToken": "<admin-token>"
}
```

**响应** `200`：
```json
{
  "userID": "u_a1b2c3d4e5f6",
  "name":   "alice",
  "token":  "cvl_newtoken..."
}
```

### `PUT /api/users/{userID}/name` — 修改用户名称

仅修改显示名称，userID 保持不变。支持传入 userID 或 name 定位用户。

**请求体**：
```json
{
  "adminToken": "<admin-token>",
  "name":       "bob"
}
```

**响应** `200`：
```json
{
  "userID": "u_a1b2c3d4e5f6",
  "name":   "bob"
}
```

---

## 认证方式总结

| 接口 | 认证方式 | 参数位置 |
|------|----------|----------|
| 用户容器接口 | User Token | 请求体 `token` 字段 |
| 管理接口 (GET/DELETE) | Admin Token | Query 参数 `?adminToken=...` |
| 管理接口 (POST) | Admin Token | 请求体 `adminToken` 字段 |
| 公共接口 (`/health`, `/_version`, `/admin/login`) | 无（login 用密码） | — |

## 容器生命周期

```
POST /api/containers  →  创建 LXD 容器 → 分配端口 → 配置 DNAT → 启动 → 返回连接信息
DELETE /api/containers/{name} → 停止容器 → 清理 DNAT → 删除容器 → 清理注册
PUT /api/containers/{name}/resize → 在线调整 CPU/内存
```
