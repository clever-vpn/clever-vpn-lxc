# LXC Manager REST API

Base URL: `https://<host>:<port>` (default port: `443` with certmagic DNS-01)

所有需要认证的接口在 HTTP 头中传递：`Authorization: Bearer <token>`

---

## 认证方式

| 接口组 | Token 前缀 | 获取方式 |
|--------|-----------|---------|
| 管理接口 | `cva_xxx` | `POST /api/admin/login` |
| 用户接口 | `cvl_xxx` | 管理员通过 `POST /api/users` 创建 |
| 公共接口 | 无需认证 | — |

---

## 实体 ID 规范

所有实体间关联统一使用 ID：

| 关联 | 字段 | 示例值 |
|------|------|--------|
| 容器 → 用户 | `userID` | `u_a1b2c3d4e5f6` |
| 容器 → 节点 | `nodeID` | `nd_abc123` |
| 容器 → 区域 | `region` | `nrt` |
| 套餐 → 区域 | `locations` | `["nrt", "ewr"]` |
| 节点 → 区域 | `region` | `nrt` |

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
{ "version": "v1.2.10" }
```

### `GET /api/regions` — 列出所有区域

从 `regions.json` 数据文件读取，由管理员通过 API 管理。

**响应** `200`：
```json
[
  { "id": "nrt", "city": "Tokyo",  "country": "JP", "continent": "Asia" },
  { "id": "ewr", "city": "Newark", "country": "US", "continent": "North America" }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 区域唯一标识，容器创建时传 `region` 参数 |
| `city` | string | 城市名称 |
| `country` | string | [ISO 3166-1 alpha-2](https://en.wikipedia.org/wiki/ISO_3166-1_alpha-2) 国家代码 |
| `continent` | string | 所属大洲 |

### `GET /api/plans` — 列出套餐

从 `plans.json` 数据文件读取。支持 `?region=` 按区域筛选。

**响应** `200`：
```json
[
  {
    "id": "lxc-1c-512mb",
    "name": "1 vCPU / 512 MB / 10 GB",
    "vcpuCount": 1,
    "ram": 512,
    "disk": 10,
    "bandwidth": 512,
    "monthlyCost": 300,
    "locations": ["nrt", "ewr"],
    "type": "lxc"
  }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 套餐标识符 |
| `name` | string | 显示名称 |
| `vcpuCount` | int | CPU 核数 |
| `ram` | int | 内存 (MB) |
| `disk` | int | 磁盘 (GB) |
| `bandwidth` | int | 月流量配额 (GB) |
| `monthlyCost` | int | 月费（美分） |
| `locations` | []string | 可用区域 ID 列表 |
| `type` | string | 套餐类型标识符 |

### `POST /api/admin/login` — 管理员登录

**请求体**：
```json
{ "password": "your-admin-password" }
```

**响应** `200`：
```json
{ "adminToken": "cva_xxxxxxxx" }
```

**错误** `401`：
```json
{ "error": "invalid password" }
```

---

## 区域管理（Admin Token 认证）

### `POST /api/regions` — 创建区域

**请求头**：`Authorization: Bearer <admin-token>`

**请求体**：
```json
{ "id": "nrt", "city": "Tokyo", "country": "JP", "continent": "Asia" }
```

### `PUT /api/regions/{id}` — 修改区域

**请求头**：`Authorization: Bearer <admin-token>`

### `DELETE /api/regions/{id}` — 删除区域

**请求头**：`Authorization: Bearer <admin-token>`

---

## 套餐管理（Admin Token 认证）

### `POST /api/plans` — 创建套餐

**请求头**：`Authorization: Bearer <admin-token>`

**请求体**：
```json
{
  "id": "lxc-1c-512mb",
  "name": "1 vCPU / 512 MB / 10 GB",
  "vcpuCount": 1,
  "ram": 512,
  "disk": 10,
  "bandwidth": 512,
  "monthlyCost": 300,
  "locations": ["nrt", "ewr"],
  "type": "lxc"
}
```

### `PUT /api/plans/{id}` — 修改套餐

**请求头**：`Authorization: Bearer <admin-token>`

### `DELETE /api/plans/{id}` — 删除套餐

**请求头**：`Authorization: Bearer <admin-token>`

---

## 用户接口（User Token 认证）

所有容器操作使用 `Authorization: Bearer cvl_xxx` 认证，每个用户只能操作自己的容器。

### `POST /api/containers` — 创建容器

**请求头**：`Authorization: Bearer <user-token>`

**请求体**：
```json
{
  "cpu":         1,
  "mem":         512,
  "disk":        10,
  "servicePort": 8080,
  "region":      "nrt",
  "planId":      "lxc-1c-512mb",
  "userData":    "#cloud-config\n..."
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `cpu` | int | ❌ | CPU 核数，默认 1 |
| `mem` | int | ❌ | 内存限制 (MB)，默认 512 |
| `disk` | int | ❌ | 磁盘上限 (GB)，0 或不传 = 不受限 |
| `servicePort` | int | ✅ | 容器内服务端口 (1-65535) |
| `region` | string | ❌ | 区域 ID（如 `nrt`），同区域多节点轮询分配。不传则随机选节点 |
| `planId` | string | ❌ | 套餐 ID，自动填充 cpu/mem/disk（与显式指定可共存） |
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
  "publicIP": "203.0.113.5",
  "ports": { "ssh": 22001, "service": 50001 }
}
```

### `GET /api/containers` — 列出我的容器

只返回当前用户创建的容器。

**请求头**：`Authorization: Bearer <user-token>`

**响应** `200`：容器列表，包含 `terminalUrl`、`health`、`region`、`nodeID` 和 `publicIP` 字段。

### `GET /api/containers/{name}` — 获取容器详情

只能查看自己的容器。

**请求头**：`Authorization: Bearer <user-token>`

**响应** `200`：
```json
{
  "name": "user-a1b2c3d4",
  "status": "Running",
  "terminalUrl": "https://lxc-api.clever-clouds.com/terminal/user-a1b2c3d4",
  "health": "healthy",
  "region": "nrt",
  "nodeID": "nd_abc123",
  "publicIP": "203.0.113.5",
  "...": "..."
}
```

### `POST /api/containers/{name}/start` — 启动容器

**请求头**：`Authorization: Bearer <user-token>`

### `POST /api/containers/{name}/stop` — 停止容器

**请求头**：`Authorization: Bearer <user-token>`

### `POST /api/containers/{name}/restart` — 重启容器

**请求头**：`Authorization: Bearer <user-token>`

### `PUT /api/containers/{name}/resize` — 调整容器规格

可单独调整 CPU、内存或磁盘，传 0 表示保持不变。

**请求头**：`Authorization: Bearer <user-token>`

**请求体**：
```json
{ "cpu": 2, "mem": 2048, "disk": 20 }
```

**响应** `200`：
```json
{ "status": "resized", "cpu": 2, "mem": 2048, "disk": 20 }
```

### `DELETE /api/containers/{name}` — 删除容器

停止并删除容器，清理端口转发。

**请求头**：`Authorization: Bearer <user-token>`

---

## 管理接口（Admin Token 认证）

### 节点管理

#### `POST /api/nodes` — 添加节点

添加节点时自动执行孤儿清理：删除节点上不属于注册表的容器。
同时恢复同 IP 的历史容器（灾难恢复）。

**请求头**：`Authorization: Bearer <admin-token>`
```json
{
  "name":          "vultr-nrt",
  "region":        "nrt",
  "sshHost":       "192.168.1.10",
  "sshPort":       22,
  "sshPassword":   "password",
  "poolSize":      "10",
  "maxContainers": 5
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | ✅ | 人类可读名称，必须唯一 |
| `region` | string | ✅ | 区域 ID（如 `nrt`） |
| `sshHost` | string | ✅ | LXD 宿主机 IP |
| `sshPassword` | string | ✅ | root 密码 |
| `sshPort` | int | ❌ | SSH 端口（默认 22） |
| `poolSize` | string | ❌ | btrfs 存储池大小（GiB），默认 10 |
| `maxContainers` | int | ❌ | 最大容器数限制，0 = 不限制 |

**响应** `200`：
```json
{
  "status": "ready",
  "id": "nd_abc123",
  "name": "vultr-nrt",
  "region": "nrt",
  "url": "https://192.168.1.10:8443"
}
```

#### `GET /api/nodes` — 列出所有节点

**请求头**：`Authorization: Bearer <admin-token>`

**响应** `200`：
```json
[
  {
    "id": "nd_abc123",
    "name": "vultr-nrt",
    "region": "nrt",
    "url": "https://192.168.1.10:8443",
    "network": "vpnbr0",
    "sshHost": "192.168.1.10",
    "sshPort": 22,
    "sshPassword": "password",
    "image": "clever-vpn-base",
    "poolSize": "10",
    "status": "active",
    "statusReason": "",
    "maxContainers": 5
  }
]
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 节点唯一标识 |
| `name` | string | 节点名称 |
| `region` | string | 所属区域 |
| `url` | string | LXD API 地址 |
| `network` | string | 容器网络桥名称 |
| `sshHost` | string | SSH 主机地址 |
| `sshPort` | int | SSH 端口 |
| `image` | string | 基础镜像别名 |
| `poolSize` | string | btrfs 存储池大小 |
| `status` | string | `active` / `degraded` / `offline` / `rebuilding` |
| `statusReason` | string | 状态原因（非正常状态时） |
| `maxContainers` | int | 最大容器数，0 = 不限制 |

#### `PUT /api/nodes/{id}` — 更新节点配置

**请求头**：`Authorization: Bearer <admin-token>`

**请求体**（所有字段可选）：
```json
{
  "status": "active",
  "maxContainers": 10
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `status` | string | 手动设置状态：`active` / `degraded` / `offline` |
| `maxContainers` | int | 最大容器数，0 = 不限制 |

**响应** `200`：
```json
{ "nodeID": "nd_abc123", "status": "updated" }
```

#### `POST /api/nodes/{id}/rebuild` — 重建节点

重新初始化节点 LXD 配置，恢复该节点上的所有容器。

**请求头**：`Authorization: Bearer <admin-token>`

**响应** `200`：
```json
{ "status": "rebuilding", "nodeID": "nd_abc123" }
```

#### `GET /api/nodes/{id}/containers` — 节点上所有容器

**请求头**：`Authorization: Bearer <admin-token>`

#### `DELETE /api/nodes/{id}` — 删除节点

删除节点时清空关联容器的 `nodeID`，标记为 `lost`。容器记录保留，由用户自行清理。

**请求头**：`Authorization: Bearer <admin-token>`

### 用户管理

#### `POST /api/users` — 创建用户

创建时自动生成不可变的 `userID` 和认证 `token`。

**请求头**：`Authorization: Bearer <admin-token>`

**请求体**：
```json
{ "name": "alice" }
```

**响应** `200`：
```json
{ "userID": "u_a1b2c3d4e5f6", "name": "alice", "token": "cvl_abc123..." }
```

#### `GET /api/users` — 列出所有用户

**请求头**：`Authorization: Bearer <admin-token>`

#### `DELETE /api/users/{userID}` — 删除用户

删除用户将同时销毁其名下所有容器。支持传入 `userID` 或 `name`。

**请求头**：`Authorization: Bearer <admin-token>`

#### `PUT /api/users/{userID}/token` — 重置用户 Token

**请求头**：`Authorization: Bearer <admin-token>`

#### `PUT /api/users/{userID}/name` — 修改用户名称

**请求头**：`Authorization: Bearer <admin-token>`

**请求体**：
```json
{ "name": "bob" }
```

### 管理员容器操作

管理员可以操作任意用户的容器，不受 userID 隔离限制。

#### `POST /api/admin/containers` — 创建容器（管理员）

**请求头**：`Authorization: Bearer <admin-token>`

```json
{
  "userID":      "u_a1b2c3d4e5f6",
  "cpu":         1,
  "mem":         512,
  "disk":        10,
  "servicePort": 8080,
  "region":      "nrt",
  "planId":      "lxc-1c-512mb",
  "userData":    "#cloud-config\n..."
}
```

> `userID` 必填，其余字段同用户 `POST /api/containers`（含 `planId`）。

#### `GET /api/admin/containers` — 列出所有容器

支持 `?userID=` 筛选。

**请求头**：`Authorization: Bearer <admin-token>`

#### `DELETE /api/admin/containers/{name}`

#### `POST /api/admin/containers/{name}/start`

#### `POST /api/admin/containers/{name}/stop`

#### `POST /api/admin/containers/{name}/restart`

---

## Web 终端

浏览器直接登录容器命令行。

**URL**：`https://<domain>/terminal/<container-name>`

容器 API 响应中包含 `terminalUrl` 字段，打开后输入 root 密码即可连接。

---

## 容器健康状态

容器列表和详情接口返回 `health` 字段：

| 值 | 说明 |
|------|------|
| `healthy` | 运行中，LXD exec 响应正常 |
| `unhealthy` | 连续 3 次 exec 检查失败 |
| `lost` | 节点已删除或容器在 LXD 中不存在 |
| `stopped` | 容器未运行 |

---

## 数据文件

所有数据以 `{ "version": 1, "records": [...] }` 格式存储：

| 文件 | 内容 |
|------|------|
| `users.json` | 用户记录 |
| `nodes.json` | 节点记录 |
| `instances.json` | 容器实例记录 |
| `regions.json` | 区域定义 |
| `plans.json` | 套餐定义 |

---

## 容器生命周期

```
POST /api/containers  →  创建 LXD 容器 → 分配端口 → 配置 DNAT → 启动 → 返回连接信息
DELETE /api/containers/{name} → 停止容器 → 清理 DNAT → 删除容器 → 清理注册
PUT /api/containers/{name}/resize → 在线调整 CPU/内存
```
