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
| `label` | string | ❌ | 自定义管理标签，可用于后续查询分组 |

**响应** `200`：返回完整的容器记录，字段与 `instances.json` 一致：
```json
{
  "id": "user-a1b2c3d4",
  "cpu": 1,
  "mem": 512,
  "disk": 10,
  "servicePort": 8080,
  "sshExtPort": 22001,
  "serviceExtPort": 50001,
  "userID": "u_a1b2c3d4e5f6",
  "password": "Abc123Xyz",
  "nodeID": "nd_abc123",
  "region": "nrt",
  "publicIP": "203.0.113.5",
  "publicIPv6": "2001:db8::1",
  "created": "2026-06-24T09:00:00Z",
  "state": "running",
  "health": "",
  "terminalUrl": "https://lxc-api.clever-clouds.com/terminal/user-a1b2c3d4",
  "label": "",
  "userData": ""
}
```

> 容器创建后 `state` 为 `"running"`（创建成功即已启动），`health` 为空字符串（将由后台健康检测填充）。

### `GET /api/containers` — 列出我的容器

只返回当前用户创建的容器。支持 `?label=` 按标签筛选。

**请求头**：`Authorization: Bearer <user-token>`

**响应** `200`：容器列表，每个元素为完整的容器记录。

### `GET /api/containers/{id}` — 获取容器详情

只能查看自己的容器。

**请求头**：`Authorization: Bearer <user-token>`

**响应** `200`：返回完整的容器记录。对于 `state=running` 的容器，GET 请求会实时查询 LXD 确认实际状态，确保返回的数据反映当前真实情况：
```json
{
  "id": "user-a1b2c3d4",
  "cpu": 1,
  "mem": 512,
  "disk": 10,
  "servicePort": 8080,
  "sshExtPort": 22001,
  "serviceExtPort": 50001,
  "userID": "u_a1b2c3d4e5f6",
  "password": "Abc123Xyz",
  "nodeID": "nd_abc123",
  "region": "nrt",
  "publicIP": "203.0.113.5",
  "publicIPv6": "2001:db8::1",
  "created": "2026-06-24T09:00:00Z",
  "state": "running",
  "health": "",
  "terminalUrl": "https://lxc-api.clever-clouds.com/terminal/user-a1b2c3d4",
  "label": "",
  "userData": ""
}
```

### `POST /api/containers/{id}/start` — 启动容器

操作成功后立即将容器 `state` 设为 `"running"`，`health` 清空（等待下次健康检测）。

**请求头**：`Authorization: Bearer <user-token>`

**响应** `200`：返回完整的容器记录（`state` 已更新为 `"running"`）。

### `POST /api/containers/{id}/stop` — 停止容器

操作成功后立即将容器 `state` 设为 `"stopped"`，`health` 清空。

**请求头**：`Authorization: Bearer <user-token>`

**响应** `200`：返回完整的容器记录（`state` 已更新为 `"stopped"`）。

### `POST /api/containers/{id}/restart` — 重启容器

先停止再启动。成功后 `state` 为 `"running"`，`health` 清空。

**请求头**：`Authorization: Bearer <user-token>`

**响应** `200`：返回完整的容器记录（`state` 已更新为 `"running"`）。

### `DELETE /api/containers/{id}` — 删除容器

停止并删除容器，清理端口转发。

**请求头**：`Authorization: Bearer <user-token>`

---

## 管理接口（Admin Token 认证）

### 节点管理

#### `POST /api/nodes` — 添加节点

异步接口：立即返回后，后台通过 SSH 初始化 LXD。
前端轮询 `GET /api/nodes` 查看状态变化：`creating` → `active` 或 `degraded`。

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
| `sshHost` | string | ✅ | LXD 宿主机 IP，同 IP 只能注册一个节点 |
| `sshPassword` | string | ✅ | root 密码 |
| `sshPort` | int | ❌ | SSH 端口（默认 22） |
| `poolSize` | string | ❌ | btrfs 存储池大小（GiB），默认 10 |
| `maxContainers` | int | ❌ | 最大容器数限制，0 = 不限制 |

**响应** `200`：
```json
{
  "status": "creating",
  "id": "nd_abc123",
  "name": "vultr-nrt",
  "region": "nrt"
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
    "maxContainers": 5,
    "ipv4": "192.168.1.10",
    "ipv6": "2001:db8::1"
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
| `status` | string | `creating` / `active` / `rebuilding` / `degraded` / `offline` |
| `statusReason` | string | 状态原因（非正常状态时） |
| `maxContainers` | int | 最大容器数，0 = 不限制 |
| `ipv4` | string | 自动检测的公网 IPv4（provision/rebuild 时检测） |
| `ipv6` | string | 自动检测的公网 IPv6（provision/rebuild 时检测） |

#### `PUT /api/nodes/{id}` — 更新节点配置

**用途**：修改已有节点的连接参数和容量设置。**不会**移动或重建容器。
不可修改 `name`、`region`、`sshHost`（关联业务逻辑）。
`status` 由系统自动管理，不可手动设置。

**请求头**：`Authorization: Bearer <admin-token>`

**请求体**（所有字段可选）：
```json
{
  "maxContainers": 20,
  "sshPassword": "newPassword",
  "sshPort": 2222,
  "poolSize": "15"
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `maxContainers` | int | 最大容器数，0 = 不限制 |
| `sshPassword` | string | SSH 密码（重装系统后更新） |
| `sshPort` | int | SSH 端口 |
| `poolSize` | string | btrfs 存储池大小（GiB） |

**响应** `200`：
```json
{ "nodeID": "nd_abc123", "status": "updated" }
```

#### `POST /api/nodes/{id}/migrate` — 迁移节点到新机器

**用途**：将源节点上的所有容器**迁移到一台新机器**上，所有容器获得新的外部端口和外部 IP。
迁移过程异步执行，API 立即返回。源节点容器逐个在新机器上重建，成功后删除旧容器。
失败容器保留在源节点，管理员可手动重试。

> **与 `PUT /api/nodes/{id}` 的区别**：update 仅修改配置参数（密码/端口/容量），不移动容器。
> migrate 是在新机器上 provision LXD 并将所有容器搬过去，属于物理机器级别的切换。

**请求头**：`Authorization: Bearer <admin-token>`

**请求体**：
```json
{
  "sshHost": "192.168.1.100",
  "sshPassword": "newMachinePassword",
  "sshPort": 22,
  "poolSize": "15"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `sshHost` | string | ✅ | 新机器的 SSH 地址 |
| `sshPassword` | string | ✅ | 新机器的 root 密码 |
| `sshPort` | int | ❌ | SSH 端口，默认 22 |
| `poolSize` | string | ❌ | btrfs 存储池大小（GiB），默认从配置读取 |

**响应** `200`（立即返回，实际迁移在后台执行）：
```json
{
  "status": "migrating",
  "newNodeID": "nd_xxx",
  "name": "tokyo-1-migrate",
  "region": "jp-tokyo"
}
```

**迁移流程**：
```
1. 注册目标节点（status: "migrating"）
2. SSH provision 目标机器（安装 LXD、初始化网络、信任证书）
3. 逐个迁移容器：
   a. 在目标节点创建同规格容器（相同 userData、CPU、内存、磁盘）
   b. 分配新的外部端口（SSH + Service）和静态 IP
   c. /etc/clever-vpn/bootstrap.env 自动写入新节点的 SSH_HOST/PUBLIC_IPV4/PUBLIC_IPV6
   d. 配置 DNAT 端口转发
   e. 删除源节点旧容器
   f. 更新 InstanceRecord: nodeID → 新节点
4. 目标节点 status → "active"
5. 全部成功：删除源节点；部分失败：保留源节点，目标节点 status → "degraded"
```

**迁移后容器变化**：

| 属性 | 是否变化 | 说明 |
|------|---------|------|
| 容器名称 | ❌ 不变 | `user-xxxx` 保持一致 |
| CPU/内存/磁盘 | ❌ 不变 | 规格完全相同 |
| userData | ❌ 不变 | 原始 cloud-config 保留 |
| root 密码 | ❌ 不变 | 使用 `instances.json` 中保存的密码重建 |
| 外部 SSH 端口 | ✅ 重新分配 | `sshExtPort` 变化 |
| 外部 Service 端口 | ✅ 重新分配 | `serviceExtPort` 变化 |
| 静态 IP | ✅ 重新分配 | 新节点的 IP 池中分配 |
| `bootstrap.env` | ✅ 更新 | `INSTANCE_SSH_HOST` / `IPV4` / `IPV6` 指向新节点 |

#### `POST /api/nodes/{id}/rebuild` — 重建节点

清空节点上所有容器和 DNAT 规则，重新初始化 LXD，然后从 `instances.json` 全量恢复容器。
节点状态变化：`rebuilding` → `active`（全部成功）或 `degraded`（部分失败）。

**请求头**：`Authorization: Bearer <admin-token>`

**响应** `200`：返回完整的节点记录，`status` 为 `"rebuilding"`：
```json
{
  "nodeID": "nd_abc123",
  "name": "node-local",
  "status": "rebuilding",
  "region": "nrt",
  "sshHost": "1.2.3.4",
  "sshPort": 2222,
  "poolSize": "15",
  "maxContainers": 10,
  "statusReason": "administrator requested rebuild"
}
```

#### `POST /api/nodes/{id}/refresh` — 立即刷新节点状态

触发即时健康检查，通过 LXD API 验证节点连通性，完成后返回最新的节点记录。

**请求头**：`Authorization: Bearer <admin-token>`

**响应** `200`：
```json
{
  "nodeID": "nd_abc123",
  "name": "node-local",
  "status": "active",
  "region": "nrt",
  "sshHost": "1.2.3.4",
  "sshPort": 2222,
  "poolSize": "15",
  "maxContainers": 10,
  "statusReason": ""
}
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

支持 `?userID=` 和 `?label=` 筛选。

**请求头**：`Authorization: Bearer <admin-token>`

#### `DELETE /api/admin/containers/{id}`

#### `POST /api/admin/containers/{id}/start`

#### `POST /api/admin/containers/{id}/stop`

#### `POST /api/admin/containers/{id}/restart`

#### `POST /api/admin/containers/{id}/refresh` — 立即刷新容器状态

触发即时状态检查：先检查所属节点连通性，再检查容器运行状态，完成后返回最新的容器记录。

**请求头**：`Authorization: Bearer <admin-token>`

**响应** `200`：返回完整的容器记录（格式同上）。

---

## Web 终端

浏览器直接登录容器命令行。

**URL**：`https://<domain>/terminal/<container-id>`

容器 API 响应中包含 `terminalUrl` 字段，打开后输入 root 密码即可连接。

---

## 容器状态

容器状态由两个独立字段表达：`state`（操作生命周期）和 `health`（运行时健康）。两者由不同的代码路径写入，互不污染。

### `state` — 操作生命周期状态

**写入者**：API handler（start/stop/restart/create）和自动恢复流程。

**语义**：容器当前处于哪个生命周期阶段。操作成功后**立即**更新，可信任。

| 值 | 含义 | 触发者 |
|------|------|--------|
| `running` | 容器在运行 | 用户 start / 创建完成 / 恢复完成 |
| `stopped` | 容器已停止 | 用户 stop |
| `creating` | 正在创建中 | 系统创建流程 |
| `recovering` | 自动恢复中 | 健康检测发现容器丢失 |
| `failed` | 恢复失败，需管理员介入 | 自动恢复失败 |

> **可信性保证**：start/stop 命令在 LXD 操作成功后才更新 `state`。API 消费者无需怀疑 `state` 的准确性。

### `health` — 运行时健康状态

**写入者**：后台健康检查器（每 60 秒）和 `GET /api/containers/{id}` 的实时查询。

**语义**：仅在 `state=running` 时有意义，表示容器的实际运行质量。

| 值 | 含义 |
|------|------|
| `""` (空字符串) | 健康，或 state 非 running（不适用） |
| `unhealthy` | 运行中但 exec 连续 3 次失败，或节点异常 |
| `lost` | 容器或节点不可达 |

### 两者的关系

```
state         health        含义
────────────────────────────────────────
running       ""            ✅ 正常运行
running       "unhealthy"   ⚠️ 运行中但异常
running       "lost"        🔴 节点失联
stopped       ""            ⏸️ 用户主动停止
creating      ""            🔄 创建中
recovering    ""            🔄 自动恢复中
failed        ""            ❌ 恢复失败
```

**关键规则**：
- `state` 由 API 操作设定，健康检测**永远不改** `state`
- `health` 由健康检测设定，API handler（start/stop）**清空** `health`
- `state` 变更时自动清空 `health`（重新开始观测）
- 前端展示优先看 `state`，`state=running` 时再看 `health`

### 检测机制

后台每 **60 秒** 自动检查所有 `state=running` 容器和节点的健康状态。管理员可通过 `POST /api/admin/containers/{id}/refresh` 和 `POST /api/nodes/{id}/refresh` 手动触发即时检查。

用户调用 `GET /api/containers/{id}` 时，对于 `state=running` 的容器会实时查询 LXD 确认实际运行状态，确保返回数据反映当前真实情况。

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
DELETE /api/containers/{id} → 停止容器 → 清理 DNAT → 删除容器 → 清理注册
```

---

## 运维操作

### 节点重启

节点宿主机重启后，iptables DNAT 规则会丢失，容器端口转发失效。容器本身仍在 LXD 中运行，但外部无法访问。需要执行重建恢复 DNAT：

```
POST /api/nodes/{id}/rebuild
```

重建过程：
1. SSH 到节点执行 `node-setup.sh`（idempotent，安全可重复执行）
2. 重新初始化 LXD、网络、防火墙
3. 重建所有容器并恢复 DNAT 规则
4. 节点状态：`rebuilding` → `active`

> 容器密码不会改变（使用 `instances.json` 中保存的原始密码重建）。

### 节点重装系统

重装后 root 密码通常会改变。操作步骤：

1. **更新节点密码**：
   ```
   PUT /api/nodes/{id}
   { "sshPassword": "新密码" }
   ```

2. **重建节点**：
   ```
   POST /api/nodes/{id}/rebuild
   ```

> 必须先更新密码再重建，否则 SSH 连接失败会导致重建失败。
