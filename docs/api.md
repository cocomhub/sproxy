<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy HTTP API 参考

本文档列出 sproxy 服务端的全部 HTTP 路由，包括请求/响应格式、必填头、错误码。
适用于通过 `pkg/client` 之外的方式调用 sproxy（如 curl、其他语言客户端、运维脚本）。

## 通用约定

- **响应格式**：除 `/healthz` `/version`、下载流、stat 之外，所有响应都是 JSON：
  ```json
  {
    "success": true,
    "message": "中文描述",
    "file_checksum": "..."
  }
  ```
- **路径校验**：所有 `filename` / `from` / `to` / `dirname` / `subdir` 参数都会经过
  `ValidateFilePath`：拒绝 `..`、绝对路径、空字节、Windows 非法字符 `<>:"|?*`，
  但允许 `/` 作为子目录分隔符（如 `sub/dir/file.txt`）。
- **认证**：当服务端凭据 Ring 非空时（首启自动登记 anonymous 凭据，见 `sclient trust` / `/api/credentials`），除 `/livez`、`/readyz`、`/healthz`、`/version`、`/ui/`、`POST /tunnel` 之外的所有路由都要求 `Authorization: SproxySig v=2 ...`（AccessKey/
  AccessKeySecret + HMAC-SHA256 请求签名；AK/SK 由 `sclient trust ak add` 生成注册，`sclient trust renew` 轮换 SK）。
  详见 CLAUDE.md「认证：SproxySig 请求签名」。
- **隧道**：所有路由（除 `POST /tunnel` 自身）都可以通过 `POST /tunnel` 走 AES-256-GCM
  加密信道访问，sclient 默认就这么做。
- **Gzip 压缩**：服务端自动为 JSON 响应启用 gzip 压缩（当客户端请求头包含
  `Accept-Encoding: gzip` 时）。二进制下载流不做压缩。

## 基础

### GET /livez

存活探针（liveness）：纯进程存活检查，不访问任何外部依赖，进程活着即返回 `OK`（text/plain，200）。无认证。

### GET /readyz

就绪探针（readiness）：检查 per-tenant UploadStore 健康状态，任一 store 停止即返回 503；全部健康返回 `OK`（text/plain，200）。无认证。

### GET /healthz

健康检查（兼容别名，语义等同 `/readyz`）。返回 `OK`（text/plain，200）；任一 UploadStore 停止返回 503。无认证。

### GET /version

返回构建信息。无认证。

```
Version: v0.2.0
BuildAt: 2026-06-01T12:00:00Z
```

### GET /

301 重定向到 `/ui/`。

### GET /ui/

嵌入式 Web UI。静态文件来源 `web/static/`。

## 文件

### POST /upload

上传单文件（multipart）。

| 项 | 内容 |
|---|---|
| Content-Type | `multipart/form-data` |
| 表单字段 | `file`（文件二进制） |
| 必填请求头 | `X-File-Checksum`（源文件 SHA-256，64 位 hex） |
| 可选请求头 | `X-File-MTime`（UnixNano，保留客户端修改时间） |
| 大小限制 | 普通上传请求体固定上限 1 GiB（`internal/size.UploadBodyLimit`，不可配置；超过 413） |

响应（成功 200）：
```json
{"success": true, "message": "文件上传成功, size: 12345", "file_checksum": "abc..."}
```

幂等：如果目标已存在且 checksum 一致，返回 200；如果存在且 checksum 不一致，返回 409。

| 状态码 | 含义 |
|---|---|
| 200 | 上传成功或文件已存在 checksum 一致 |
| 400 | 文件名无效 / 缺少 X-File-Checksum / SHA-256 校验失败 |
| 401 | 未授权（凭据 Ring 非空时签名缺失/非法/过期/重放） |
| 409 | 文件已存在但 checksum 不一致 |
| 413 | 请求体超过 1 GiB 硬限制 |
| 500 | 服务端写文件失败 |

### GET /download?filename=...

下载单文件，支持标准 `Range` header。

| 项 | 内容 |
|---|---|
| 查询参数 | `filename` |
| 响应头 | `X-File-Checksum`、`X-File-MTime`、`Accept-Ranges: bytes`、`Content-Disposition` |
| Range 支持 | 是（返回 206 + `Content-Range`） |

| 状态码 | 含义 |
|---|---|
| 200 | 全量下载 |
| 206 | Partial Content（带 Range 请求时） |
| 400 | filename 无效 |
| 401 | 未授权 |
| 404 | 文件不存在 |
| 416 | Range 越界 |

### POST /delete?filename=...

删除文件。

| 项 | 内容 |
|---|---|
| 必填请求头 | `X-File-Checksum`（防误删） |

| 状态码 | 含义 |
|---|---|
| 200 | 删除成功 |
| 400 | filename 无效 / 缺少 checksum / checksum 不匹配 |
| 404 | 文件不存在 |

### POST /rename?from=&to=

重命名 / 移动文件。`from` 必须存在、`to` 必须不存在；服务端会自动 `mkdir -p`
中间目录。**对称要求 `X-File-Checksum`（源文件当前 SHA-256）**，防误覆盖。

| 状态码 | 含义 |
|---|---|
| 200 | 重命名成功（或源与目标相同的 no-op） |
| 400 | 路径无效 / 缺少 checksum / checksum 不匹配 |
| 404 | 源文件不存在 |
| 409 | 目标已存在 |

### POST /api/batch/delete

批量删除文件（continue-on-error 模式）。请求体 JSON：

```json
{
  "files": [
    {"filename": "file1.txt", "checksum": "abc..."},
    {"filename": "sub/file2.txt", "checksum": "def..."}
  ]
}
```

| 项 | 内容 |
|---|---|
| Content-Type | `application/json` |
| 每个条目必须含 | `filename`（路径）、`checksum`（当前 SHA-256 hex） |

响应 `200`：
```json
[
  {"filename": "file1.txt", "success": true, "message": "删除成功"},
  {"filename": "sub/file2.txt", "success": false, "message": "checksum 不匹配"}
]
```

- 按数组顺序逐个执行，单个失败不影响后续条目
- 每个条目的校验逻辑与单文件 `POST /delete` 一致

### POST /api/batch/rename

批量重命名 / 移动文件（continue-on-error 模式）。请求体 JSON：

```json
{
  "operations": [
    {"from": "old1.txt", "to": "new1.txt", "checksum": "abc..."},
    {"from": "old2.txt", "to": "sub/new2.txt", "checksum": "def..."}
  ]
}
```

| 项 | 内容 |
|---|---|
| Content-Type | `application/json` |
| 每个操作必须含 | `from`（源路径）、`to`（目标路径）、`checksum`（源文件当前 SHA-256 hex） |

响应 `200`：
```json
[
  {"from": "old1.txt", "to": "new1.txt", "success": true, "message": "重命名成功"},
  {"from": "old2.txt", "to": "sub/new2.txt", "success": false, "message": "源文件不存在"}
]
```

- 按数组顺序逐个执行，单个失败不影响后续操作
- 每个操作的校验逻辑与单文件 `POST /rename` 一致

### HEAD /api/files/stat?filename=...

查询远端单个文件元信息。**不返回 body**，全部信息在响应头：

| 响应头 | 内容 |
|---|---|
| `X-File-Size` | 字节数 |
| `X-File-MTime` | UnixNano |
| `X-File-Checksum` | SHA-256 hex（目录不返回） |
| `X-File-IsDir` | `true` 仅当目标为目录 |

### GET /api/files?subdir=...

列出指定目录下的文件与子目录。`subdir` 缺省时列出根目录。

支持分页参数 `offset` 和 `limit`，以及排序参数 `sort` 和 `order`。向后兼容（旧客户端不传分页参数时行为和原来一致）。

| 查询参数 | 默认值 | 描述 |
|---|---|---|
| `subdir` | `""`（根目录） | 列出指定子目录 |
| `offset` | `0` | 跳过前 N 个条目 |
| `limit` | `0`（不限制） | 最多返回 N 个条目 |
| `sort` | `"name"` | 排序字段：`name`、`size`、`time` |
| `order` | `"asc"` | 排序方向：`asc`、`desc` |

响应（分页模式下新增 `total`、`offset`、`limit` 顶层字段）：
```json
{
  "files": [...],
  "total": 10,
  "offset": 0,
  "limit": 10
}
```

### GET /api/files/search?q=keyword

递归搜索文件名中包含 `q` 的文件（不区分大小写）。`q` 为空时返回空列表。

| 查询参数 | 说明 |
|---|---|
| `q` | 搜索关键字（不区分大小写） |

响应格式与 `GET /api/files` 相同 — `{"files": [...]}`，但 `name` 为完整相对路径。

| 状态码 | 含义 |
|---|---|
| 200 | 搜索完成（可能为空结果） |
| 401 | 未授权 |
| 500 | 服务端 WalkDir 失败 |

## 分块上传/下载

### POST /upload/init

初始化分块上传会话。请求体 JSON：

```json
{
  "upload_id": "客户端生成的稳定ID（SHA256(filename|size|mtime|checksum)[:32]）",
  "filename": "sub/dir/file.bin",
  "total_size": 12345678,
  "chunk_size": 4194304,
  "total_chunks": 3,
  "file_checksum": "整个文件的 SHA-256 hex",
  "file_mod_time": 1750000000000000000
}
```

响应 200：
```json
{"success": true, "upload_id": "...", "chunk_size": 4194304, "message": "..."}
```

如果 `upload_id` 存在 → 自动续传，`message` 中说明缺失分块数。

**分块计划上界**：`total_chunks` 不得超过 `65536`（服务端按 `total_chunks` 等长分配两块元数据，
上限用于防内存放大）；超过返回 400。由此可得单文件实际上限 ≈ `65536 × (max_chunk_upload_bytes − 4 KiB)`
（默认配置约 **3.999 TiB**）；若客户端把 `chunk_size` 设得很小（如 4 KiB），可上传的单文件
会相应缩小到 256 MiB（协议未定义 `chunk_size` 下界）。默认客户端（sclient / Web UI / SDK）
自适应 4 MiB–64 MiB，单文件 ≤32 GiB 时恒为 ~512 块，不受影响。

> 注意：`chunk_size` 超过服务端上限时会被**裁剪**，此时 `total_chunks` 会被重算并以重算值
> 再校验一次——因此「声明值恰好等于上界」也可能因裁剪后的重算而超出。

### POST /upload/chunk

上传单个分块（multipart）。

| 表单字段 | 内容 |
|---|---|
| `upload_id` | init 时拿到的 ID |
| `chunk_index` | 0-based |
| `chunk_checksum` | **必填**，本块的 SHA-256 hex（64 位） |
| `chunk` | 文件二进制 |

| 状态码 | 含义 |
|---|---|
| 200 success | 接收成功（或幂等） |
| 200 should_retry | SHA-256 校验失败，客户端应重传 |
| 400 | 缺字段 / chunk_checksum 不是 hex |
| 404 | upload_id 不存在或已过期 |
| 409 should_retry | 该会话正在合并（`complete` 的「全文件校验 → rename」窗口，毫秒级）——**瞬态**，客户端应稍后重试同一分块；不重试会被视为永久失败 |
| 410 | 上传已完成 |
| 413 | 单块超过 `max_chunk_upload_bytes` |

### GET /upload/status?upload_id=&filename=

查询上传状态。优先按 upload_id 查；如果 upload_id 找不到，可用 filename 查
（用于跨进程恢复客户端 ID 丢失场景）。

### POST /upload/complete

合并所有分块。请求体 JSON：`{"upload_id": "..."}`。

服务端会按序读取所有 chunk 文件、流式合并、再次计算完整文件 SHA-256 并与
`file_checksum` 对比。校验失败时**不保留**已合并文件。

> **会话回收与重复 complete 的返回值**：`complete` 成功后服务端会清理该会话（进程内的延迟清理；
> 停机时立即执行一次），并在 TTL（默认 24h）后**兜底回收已完成会话**（此前已完成会话永不回收，
> 表与磁盘会随上传单调增长）。因此对同一 `upload_id` 重复 `complete`：会话仍存在时返回 200
> （幂等）；已被回收后返回 **404**（会话不存在）。
> 另：`upload_id` 已被新会话复用（同 `filename|size|mtime|checksum` 重试即同 id）时，绑定旧会话的
> 清理会**整项跳过**，不会误删新会话的会话目录与在途临时文件。

### GET /download/chunk?filename=&offset=&length=

自定义分块下载端点。响应头包含 `Content-Range`、`X-Chunk-Checksum`。

> 推荐：标准 `GET /download` + `Range: bytes=` 与本端点等价，且更易穿越 CDN。
> 本端点保留以维持向后兼容、支持 SHA-256 单块校验场景。

## 目录

### POST /mkdir?dirname=...

创建子目录（递归，类似 `mkdir -p`）。

### POST /rmdir?dirname=&force=true

删除目录。`force=true` 时递归删除内容；否则仅允许空目录。
同时清理 checksum store 中相同前缀的所有记录。

## 文件分享（share）

### POST /api/share

创建分享链接。请求体：`{filename, password?, expire_in?}`。

- `filename`：要分享的文件相对路径；`password` 可选访问密码；`expire_in` 可选过期时长（Go duration 字符串）。
- 响应：`{token, filename, password?, expire_at?, ...}`（分享令牌）。
- **持久化**：分享链接落 `<默认卷根>/anonymous/meta/share/<token>.json`（原子写，重启恢复未过期链接；
  一次性/计数/撤销/过期同步删文件；纯内存形态 = 未装配持久化目录时）。

### GET /s/{token}

通过分享 token 访问文件（无需认证）。`password` 分享需在 query 携带 `?password=`。

### GET /api/shares

列出当前 owner 的所有分享链接。

### DELETE /api/shares/{token}

撤销分享（幂等）。

## 文件版本管理（versioning，需配置 `versioning.enabled: true`）

### GET /api/versions?filename=...

列出指定文件的历史版本（`{versions: [{id, size, mod_time, checksum?}]}`）。

### POST /api/versions/restore?filename=&version=...

恢复指定版本为当前文件。

### DELETE /api/versions?filename=&version=...

删除指定版本。

## 云端下载（cloud）

### POST /api/cloud/download

创建云端下载任务。请求体：`{url, filename?}`。

- `url`：外部 HTTP/HTTPS URL（SSRF 防护默认开启：解析到私网/内网/回环地址拒绝，
  `cloud_download_allow_private: true` 关闭）；`filename` 可选保存文件名（默认从 URL 提取）。
- 响应：任务对象（见下）。提交后服务端**异步下载**（客户端断连不中断），下载落
  `<storage_root>/<tenant>/cloud/<taskID>/<filename>`。

### POST /api/cloud/download/batch

批量创建下载任务。请求体：`{urls: [...]}`（或 `{entries: [{url, filename?}]}`）。部分失败不中断，逐项返回结果。

### GET /api/cloud/tasks

列出当前 owner 的下载任务（`{tasks: [...]}`；owner 为空 = 管理员可见全部）。

### GET /api/cloud/tasks/{id}

查询单个任务。任务对象字段：

```json
{
  "id": "...", "owner": "...", "url": "...", "filename": "...",
  "status": "pending|downloading|completed|failed|cancelled",
  "total_size": -1, "downloaded": 0, "checksum": "",
  "etag": "...", "file_mtime": 0, "error": "",
  "created_at": "...", "updated_at": "...", "expires_at": "..."
}
```

### POST /api/cloud/tasks/{id}/cancel

取消下载任务（幂等）。

### POST /api/cloud/tasks/{id}/resume

恢复/重新启动任务（支持续传）。

### POST /api/cloud/tasks/{id}/archive

将任务已下载文件打包为 tar.gz（`cloud_archive_max_bytes` 限制原始大小总和）。

### POST /api/cloud/archive

批量归档（请求体含任务 ID 列表）。

### DELETE /api/cloud/tasks/{id}

删除任务（含云端文件）。

### POST /api/cloud/groups

创建任务组。请求体：`{name, urls: [...]}`（组内每项独立任务）。

### GET /api/cloud/groups

列出任务组。

### GET /api/cloud/groups/{id}

查询组详情（含子任务状态聚合）。

### POST /api/cloud/groups/{id}/cancel / /resume / /archive

取消 / 恢复 / 归档整个组。

### DELETE /api/cloud/groups/{id}

删除组（含云端文件）。

## 存档（archive 压缩/解压缩）

### POST /api/archive

创建存档任务（压缩/解压缩）。请求体含 `{sources: [...], target: "...", action: "compress|decompress"}` 等。

### GET /api/archive-dir

获取可存档目录列表。

## Hub 中继管理（需配置 `hub.enabled: true` + `RouteTable`）

### GET /api/hub/nodes

列出已注册节点（含 `id`、`virtual_ip`、`services`、`tags` 等；SproxySig 签名数据源）。

### GET /api/hub/services

列出所有节点宣告的 mesh 服务（`[{name, node, addr}]`）。

### GET /api/hub/stats

Hub 统计。

### GET /api/stats

服务端统计信息（文件数/大小/存储用量等；隧道内层裸注册，隧道加密即认证）。

### GET /api/hub/federation/nodes

联邦节点表：本 hub 路由表 + 联邦候选（跨 hub 2 级发现）。

### DELETE /api/hub/nodes/{id}

移除节点（幂等）。

### POST /api/relay/stream

流中继：升级为到目标叶子的双向字节流（`{target, type: "tcp", addr}`），
叶子按 dial 帧出站拨号（出口拨号策略把关）。跨 hub 联邦未命中时链式转发。

## 凭据管理（credentials）

凭据 Ring 由服务端持有（首启自动登记 anonymous 凭据；登记/轮换走 `sclient trust` / 本组端点）。

### POST /api/credentials/register

凭据注册（公开端点，限频；供 `sclient trust register` TOTP 注册）。

### POST /api/credentials/nonce

一次性 nonce（限频）。

### POST /api/credentials/login

登录（限频；TOTP）。

### GET /api/credentials

列出 AccessKey（AK）。

### POST /api/credentials

新增 AK 条目。

### DELETE /api/credentials/{ak}

删除 AK 条目（幂等）。

### POST /api/credentials/{ak}/renew

轮换 AK 条目。

### GET /api/credentials/{ak}/sk

列出某 AK 下的 SK 条目（含 skey-id）。

### DELETE /api/credentials/{ak}/sk/{skID}

删除 SK 条目。

### POST /api/credentials/{ak}/sk/{skID}/expire

使 SK 条目过期。

## 隧道

### POST /tunnel

AES-256-GCM 加密的转发请求。请求体为帧协议：

```
[4B big-endian metaLen][encrypted metadata][stream chunks...]
```

`metaLen` 上限 1 MiB（`MaxMetadataBytes`），超过返回 400 并立即关闭。

详见 [tunnel.md](./tunnel.md)。

## 跨节点（mesh）

### GET /api/mesh/status

只读运维视图：回答「本机跨节点面/角色起了没、pin 了几个」。**不含任何秘密**（指纹为公开标识，不返回 SK/隧道密钥/信令密钥）。

```json
{
  "remote_read":  {"enabled": true, "addr": "127.0.0.1:19000", "pinned": 2},
  "remote_write": {"enabled": true, "addr": "127.0.0.1:19001", "pinned": 1},
  "node": {"running": true, "node_id": "node-b", "hub_url": "wss://hub.example.com/ws",
           "webrtc": true, "services": ["volread", "volwrite"]},
  "hub_url": "https://hub.example.com:18083",
  "signaling_enabled": true
}
```

- `pinned`：读面 = 全部 `mesh_readers` 指纹数；写面 = **仅 scope 授予写**（`write|rw`）的指纹数；
- `addr` 优先取 **listener 实际监听地址**（配置写 `:0` 时只有 listener 知道真实端口），未启动时回落配置值；
- `node.running = false` 表示**配置启用但角色未启动**（端口占用/凭据缺失等）——便于直接定位；
- 未启用的面/角色**不出现**（`omitempty`）。
- **Web UI**：Hub 面板（Hub tab）以状态卡形式展示本视图，且与 Hub 节点表**各自独立容错**
  （隧道模式下 `/api/hub/*` 404 不影响本卡显示）。

### `GET /api/mesh/acl`（跨节点授权只读视图，W3）

返回**调用者自己 owner** 的跨节点授权（服务端卷 ACL 的 `mesh_readers` 条目）。

```json
{
  "owner": "alice",
  "entries": [
    {"volume": "main",  "node": "node-a", "fingerprint": "sha256:3f2a…", "scope": "rw"},
    {"volume": "share", "node": "node-c", "fingerprint": "sha256:0123…", "scope": "read"}
  ]
}
```

**可见性口径：仅 owner 自身** —— 只返回 `mesh_readers.owner == 调用者 owner` 的条目；别人的授权既
不出现在列表里，**也不通过计数泄露**。owner 口径取自「已认证 actor」，**不接受任何查询参数覆盖**
（`?owner=bob` 无效）。无认证部署下 actor 为空 ⇒ 归入 `anonymous`（该部署只有一个隐式 owner）。

- 无授权时 `entries` 为空数组（`[]`，非 `null`）；未装配配置时同样 200 + 空数组；
- 指纹是**公开标识**（对端节点身份，非秘密），响应不含任何密钥；
- 同 `GET /api/mesh/status` 一样同时注册主 mux（受认证保护）与隧道内 `localMux`；
- 挂载面：主 mux 走 `authMiddleware`（**不挂** `fileRoute` 的角色门禁——owner 过滤本身就是边界，
  且该端点不触碰文件系统）。
- **CLI**：`sclient mesh acl`（见 `docs/cli.md`）。

### 跨节点指标（`GET /metrics`，W4）

跨节点能力默认关闭，出问题时「谁在被拒、直连到底成不成」需要能长期观测（而非翻日志）。以下三族
指标均为**带标签**计数器，标签基数由配置界定（节点/服务/原因都是小集合）：

```
# 成功建链的实际载体（webrtc=打洞直连 / relay=hub 中继）
sproxy_mesh_dial_total{carrier="webrtc",node="node-a",service="volread"} 12

# 「先尝试打洞、失败后改用中继」的次数（直连成功率的真实分母）
sproxy_mesh_dial_fallback_total{node="node-a",service="volread"} 3

# 写面**授权**拒绝（不含配额超限/校验和不符这类「已授权但业务失败」）
sproxy_remote_write_denied_total{node="node-a",reason="scope_denied"} 1
```

- `carrier` ∈ `webrtc` | `relay`（**只记成功**；失败且未回落没有可用链路，记成任何一种载体都是错的）；
- `reason` ∈ `unauthenticated` | `volume_missing` | `volume_unknown` | `not_pinned` | `scope_denied` |
  `not_assembled`（**稳定取值**，改动即破坏既有面板/告警）；`node` 来自**指纹反查**的绑定节点名
  （不是请求参数），反查不到时为空；
- 标签值按 Prometheus 文本格式转义（`\`、`"`、换行）；无样本时仍输出 `HELP`/`TYPE`
  （让「一直没数据」与「指标不存在」在面板上可区分）；
- 与任务快照的 `carriers` **同源**：任务快照回答「这次任务走了什么」，指标回答「长期直连成功率」。

## 用户卷（per-owner 用户自有卷）

用户自有卷是每个 sproxy 用户独立管理的网盘盘（仅外部类型：`baidupcs` 等已注册 backend）。
存储位置：`<storage_root>/<owner>/meta/volume/<name>.json`（原子写，重启扫描恢复）。
寻址：同步任务 `remote.volume` 填用户卷名，任务 owner 必须匹配卷 owner（跨用户 404 防枚举）。

### `POST /api/volumes/user`

创建用户卷。请求体（JSON）：

```json
{"name": "my-disk-1", "type": "baidupcs", "capacity": 0, "extra": {"bduss": "...", "baidu_root": "/disk1"}}
```

```json
{"name": "my-webdav", "type": "webdav", "capacity": 0, "extra": {"url": "https://nextcloud.example.com/remote.php/webdav", "username": "alice", "password": "..."}}
```

- `type` 须服务端已注册 backend（未注册 → 400）；`extra` 为类型特有配置（baidupcs 支持
  `bduss`/`baidu_root`/`binary_path`/`local_root`；webdav 支持 `url`（必填 http(s) 根）、
  `username`+`password`（Basic）或 `token`（Bearer，优先））；`capacity` 独立卷容量（0 = 不限制，不计 owner 配额）
- 认证：SproxySig / api_keys（owner 从请求派生）；重名 → 错误
- 响应：`{"success": true}`

### `GET /api/volumes/user`

列出当前 owner 的用户自有卷（按认证过滤，只返回自己的）。

```json
{"volumes": [{"name": "my-disk-1", "type": "baidupcs", "capacity": 0, "extra": {...}}]}
```

### `DELETE /api/volumes/user?name=<name>`

删除用户卷。

- owner 校验：跨 owner 404（防枚举）；被**活跃同步任务引用**（pending/syncing/retrying）→ 409
- 响应：`{"success": true}`

### Web UI

卷面板（`/ui/` → 卷 tab）内置「我的用户卷」区：创建表单（卷名 / 类型下拉 / 容量 / extra JSON）
+ 列表（卷名/类型/容量 + 删除按钮）。创建/删除即调上述 API，删除前确认，409 时提示先取消同步任务。
类型下拉经 `GET /api/backends` 动态填充（未来任何新 backend 自动出现，无需改前端）。

## 文件同步（sync /api/sync/tasks）

服务端文件同步任务 API（需配置 `sync.max_concurrent` 或 `sync_remotes`，否则 400 `sync not configured`）。

### POST /api/sync/tasks

创建并启动同步任务。请求体 JSON（对齐 `syncmgr.CreateRequest`）：

```json
{"direction": "push", "remote": "r1", "src": "", "dst": "", "recursive": true,
 "conflict_policy": "skip", "delete_policy": "skip", "sync_empty_dirs": false, "follow_symlinks": false}
```

- `direction`：`push`（本地→远程）/ `pull`（远程→本地）/ `both`（双向：一次任务内先 push 再 pull，两端一致）
- `remote`：`sync_remotes` 配置的远程节点名（必填；未配置/缺凭据 → 400 fail-closed）
- `conflict_policy`：`skip`（默认）| `overwrite` | `lww` | `conflict_rename`
- `delete_policy`：`skip`（默认，源删除不传播，零回归）| `propagate`（源端删除经一次任务反映到目标，幂等）
- `src`/`dst`：FS 根相对路径（默认 `""` = 整个根）；`include`/`exclude`：glob 过滤器
- `owner` 由请求认证派生（客户端不可伪造）；201（新建）/ 200（去重复用活跃任务）
- 响应：SyncTask JSON（含 `id`/`direction`/`status`/`files_total`/`files_done`/`bytes_total`/`bytes_done`/`files_deleted`/`results` 等）

### GET /api/sync/tasks

列出当前 owner 的同步任务元信息（`{success, tasks: [SyncTaskMeta]}`；含 `files_deleted` 删除传播计数）。

### GET /api/sync/tasks/{id}

查询单个任务详情（含逐文件 `results`）。跨 owner 404。

### POST /api/sync/tasks/{id}/cancel

取消进行中任务（pending/syncing/retrying）。跨 owner 404。

### DELETE /api/sync/tasks/{id}

删除任务（终态清理）。跨 owner 404。

## 卷后端类型（GET /api/backends）

返回服务端已注册的卷后端类型（动态，随 `RegisterBackend` 注册变化）。

```json
{"backends": ["baidupcs", "webdav", "s3"]}
```

- 供 Web UI 类型下拉 / sclient 提示已注册类型
- 新增外部后端 = 新 backend 包 `RegisterBackend(type, factory)` 注册，前端自动感知
  - `s3`：`endpoint`/`bucket`/`access_key`/`secret_key`（必填），`region`/`use_ssl`/`local_root`（可选）

## 错误码附录

| HTTP | 业务原因（示例） |
|---|---|
| 400 | 路径无效、缺必填头、checksum 不匹配、chunk_checksum 不是 hex |
| 401 | SproxySig 校验未通过（凭据 Ring 非空时） |
| 404 | 文件不存在、upload_id 不存在 |
| 409 | 文件 / 目录已存在 |
| 410 | 上传会话已完成 |
| 413 | 请求体超过限制 |
| 416 | Range 越界 |
| 500 | 服务端写文件、目录失败 |
