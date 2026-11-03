<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy HTTP API 参考

本文档列出 sproxy 服务端的全部 HTTP 路由，包括请求/响应格式、必填头、错误码。
适用于通过 `pkg/client` 之外的方式调用 sproxy（如 curl、其他语言客户端、运维脚本）。

> **新增/修改路由必须同步本节文档**：每条路由需有 `### METHOD /path` 标题（查询串可附在路径后，
> 不参与校验），实现依据为 `pkg/server` 等处的实际 handler。门禁 **R55**
> （`internal/archcheck/docs_api_gate_test.go`）会比对本文档路径集合与 `route_classes.tsv`：
> 未文档化必须登记驱动清单，文档里出现过期路径即红。

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
| 大小限制 | 普通上传请求体上限（`max_upload_bytes`，默认 1 GiB；超过 413） |

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
| 413 | 请求体超过 `max_upload_bytes` 上限 |
| 500 | 服务端写文件失败 |

### GET /download?filename=...

下载单文件，支持标准 `Range` header。

| 项 | 内容 |
|---|---|
| 查询参数 | `filename`；可选 `ciphertext=1` |
| 响应头 | `X-File-Checksum`、`X-File-MTime`、`Accept-Ranges: bytes`、`Content-Disposition` |
| Range 支持 | 是（返回 206 + `Content-Range`） |

**`ciphertext=1`**：at-rest 加密卷（`volumes[].extra.encrypt`）默认返回**明文**（加密只在
存储层，用户访问恒见明文）；带此参数时返回**存储原样密文**（不解密），`Range`/`Content-Length`
按密文坐标——供备份/迁移/带外校验。非加密卷该参数无副作用（原始字节 == 明文）。
密文响应不附 `X-File-Checksum`（台账描述明文，不适用于密文）。

> 适用范围：`ciphertext=1` 仅对**卷根 at-rest 加密**（`storage.Root` 加密态）生效。
> secretdata 保密卷 / 302 直链（B 态外部卷）分支不支持该参数（仍返回解密/直链内容）。

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
| 409 | 外部卷：sidecar 记录的内容校验和与 `X-File-Checksum` 不一致（文件已变更，请刷新后重试） |

- **作用卷（`?volume=`）**：未指定 → 先在 owner 视图内的**外部卷**上按 rel 查找（删到即返回），
  未命中再走本地卷（跨卷定位）；显式指定外部卷 → 只在该卷上删。
- **配额释放按「用户可见逻辑长度」**（非磁盘字节）：at-rest 加密卷上磁盘是密文（容器头 +
  每分块 tag/padding 膨胀），而写入预留/软删保留的都是逻辑长度 ⇒ 清空回收站/过期清理（下文的
  `EmptyTrash`/`CleanupTrash`）必须用 `Root.LogicalSize` 释放，否则 owner 配额漂移（密文 > 逻辑
  时多释放 → 配额虚低、超限写入通过）。
- **外部卷为「硬删」**（评审 F1）：不经回收站（`?soft=true` 对本地卷生效）、不做本地 quarantine
  （远端后端无原子 rename）。`X-File-Checksum` 与 sidecar **记录**的整文件 SHA-256 交叉比对
  （不重算——远端整文件回读不可接受）；sidecar 缺失则跳过该判据。删除会同时删 sidecar 并释放卷级容量。
  此前外部卷对象**无任何用户可见删除入口**（卷写满后一切写入恒 507），本接口是唯一回收路径。
- **外部卷 `?soft=true` 为 fail-closed（400）**：回收站（trash 桶）落在**本地租户根**，外部卷没有对应
  实现 ⇒ 若静默走硬删，用户**显式**请求的可恢复删除会变成**不可恢复删除**（静默降级 + 数据丢失）⇒
  明确拒绝并提示改用硬删（不带 `soft=true`），**不触碰数据**。外部卷回收站为独立排期项。

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

- 按数组顺序逐个执行，单个失败不影响后续操作（**部分成功**：响应逐条反映每个操作的真实结果，
  不因某条失败回滚已成功条目）
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

**性能（roadmap P0）**：列表走**增量文件索引**（首次使用全量构建 + 写路径增量维护，
与搜索同源），大目录亚秒级；语义与实时扫描逐字一致——目录条目 `IsDir=true`（name 为
basename）、文件条目带 `size/mtime/checksum/volume`、`subdir` 只列直接子项、在途临时文件
不可见、`?volume=` 只列该卷文件（目录条目跨卷聚合仍全列）。索引缓存语义：不经领域写
路径的旁路写（如版本恢复）需使索引失效后才可见（`InvalidateIndex`，装配层自动调用）。

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

### GET /api/files/search?q=keyword[&tag=label]

递归搜索文件名中包含 `q` 的文件（不区分大小写）。`q` 为空时返回空列表。
`tag` 非空时**按标签精确过滤**（roadmap 11.10-④）：只返回含该标签的文件，与 `q` AND
组合；`q` 为空 + `tag` 非空 = 按标签过滤全部文件（不退化 400）。

| 查询参数 | 说明 |
|---|---|
| `q` | 搜索关键字（不区分大小写）；与 `tag` 都为空时 400 |
| `tag` | 标签精确过滤（可组合）；非法标签（非 `[A-Za-z0-9_\p{L}-]{1,32}`）不命中 |
| `offset` / `limit` | **可选分页**（2026-10-29）：不传 = 返回全部（兼容既有契约）；显式传则按 List 语义（`limit` 缺省 1000、上限 10000），`total` 恒为全部命中数 |

响应格式与 `GET /api/files` 相同 — `{"files": [...]}`，但 `name` 为完整相对路径。

**覆盖范围**（2026-10-30 起为**全卷**）：本地卷走内存增量索引；**外部卷**（S3/WebDAV/baidupcs/
secretdata 等）经**卷状态缓存**（`volumestate`，TTL=0 + 写穿 + `volume refresh`）递归列举后聚合
——首次遍历付出远端成本，之后命中缓存。上限：深度 ≤ 16、单请求条目 ≤ 50000、超时 10s（超限截断
并记录 Warn）；单卷列举失败被隔离（其它卷结果照常返回）。`tag` 是本地卷元数据，`tag` 非空时
不遍历外部卷（结果只含本地卷）。
索引失效/重建见 `POST /api/volumes/refresh`（同步全量失效搜索索引 + 卷状态缓存）。

| 状态码 | 含义 |
|---|---|
| 200 | 搜索完成（可能为空结果） |
| 401 | 未授权 |
| 500 | 服务端 WalkDir 失败 |

### GET /api/search/semantic?q=&topk=

语义检索端点（向量索引未装配 → `404`，零回归）。

- 认证：主 mux `reader`（SproxySig）/ 隧道内层 `tunnel`
- query 参数：
  - `q`（必填）：查询文本（trim 后为空 → `400`）
  - `topk`：返回条数（默认 10，上限 50；非法值回落默认）
- 响应：`{"results": [{"file": {…FileInfo…}, "score": 0.83}], "mode": "semantic"}`
  - `mode=semantic`：向量相似度命中；比对范围限于请求者 owner 的向量索引
  - `mode=keyword`：**回退**（装配层未把 `q` 向量化，或检索失败）——按文件名关键词搜索返回，
    `score` 恒 0；不返回 5xx（fail-safe）
- 错误：`400` `q` 为空 / 检索失败；`404` 语义搜索未配置（`vector` 未装配）

### PUT /api/files/meta?filename=

元数据补录（已上传文件的 `extra` 旁路更新：读–校验–合并–写 sidecar，不重算内容哈希）。

- 认证：主 mux `user`（写子组）/ 隧道内层 `tunnel`
- query 参数：`filename`（必填，经 `ValidateFilePath`）
- 请求体（≤ 64 KiB）：
  ```json
  {"extra": {"code": "EXAMPLE-001", "maker": "STUDIO X"}, "mode": "merge"}
  ```
  - `mode`：`merge`（缺省，新增/覆盖同名键，不删旧键）/ `replace`（整表替换）/ `delete`（删指定键）
  - `extra` 的键值经 `meta.TrimExtra` 清洗 + `meta.ValidateExtra` 校验；`merge`/`replace` 时不得为空，
    `delete` 至少一个键
- 响应：`{"success": true, "message": "meta extra 已更新"}`
- 错误：`400` filename 为空 / 请求体非法 / `mode` 非法 / `extra` 为空或校验失败；
  `503` 元数据能力不可用（`ErrMetaUnavailable`）；`409` 其余域层失败（无 sidecar / 并发覆盖 / 卷不支持 /
  文件不存在或路径非法——handler 把域错误统一映射为状态冲突，按 message 区分原因，可重试）
- 副作用：审计 `file_meta_update`（成功）

### GET /api/files/meta?filename=

读取文件的自定义元数据 `extra` 与原始文件时间。

- 认证：主 mux `reader`（只读子组）/ 隧道内层 `tunnel`
- query 参数：`filename`（必填）
- 响应：`{"extra": {"code": "EXAMPLE-001"}, "mtime": "2026-11-03T12:00:00.000000000Z",
  "ctime": "…"}`
  - 文件存在但无 extra → `extra` 为空对象 `{}`；`mtime`/`ctime`（RFC3339Nano UTC）为附加字段，
    无时间信息时省略
- 错误：`400` filename 为空；`404` 文件不存在（`files.HTTPError` 状态直传）；`500` 读取失败

### POST /api/tags

为文件打标签（roadmap 11.10-④）。标签规则：`[A-Za-z0-9_\p{L}-]{1,32}`（1-32
字符），每文件 ≤20 个，自动去重排序。打标/搜索不影响既有语义（索引快照带 tags 字段，
重启不丢；索引失效重建从 meta/tags store 重新合并）。

| 查询参数 | 说明 |
|---|---|
| `filename` | 打标文件名（单文件形态） |
| `tags` | 逗号分隔标签列表（如 `a,b,c`） |

JSON body 批量形态（body 存在且 `files` 非空时优先）：
```json
{"files": ["a.txt", "dir/b.txt"], "tags": ["work", "重要"]}
```

| 状态码 | 含义 |
|---|---|
| 200 | 打标成功（`{"success":true,"message":"标签已更新"}`） |
| 400 | 标签非法/超量/缺参（整批拒绝，不部分成功） |
| 404 | 文件不存在（整批拒绝） |
| 500 | 标签存储写失败（索引不更新，下次构建以 store 为准） |

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
| 200 success | 接收成功（或幂等）。**持久化契约**：返回 200 时该块的校验值已落盘（每块一个 `chunk-<i>.sha256` sidecar），进程被 kill -9 后重启仍判为已接收（内容校验通过即可续传，无需重传） |
| 200 should_retry | SHA-256 校验失败，客户端应重传 |
| 400 | 缺字段 / chunk_checksum 不是 hex |
| 404 | upload_id 不存在或已过期 |
| 409 should_retry | 该会话正在合并（`complete` 的「全文件校验 → rename」窗口，毫秒级）——**瞬态**，客户端应稍后重试同一分块；不重试会被视为永久失败 |
| 410 | 上传已完成 |
| 413 | 单块超过 `max_chunk_upload_bytes` |

### GET /upload/status?upload_id=&filename=

查询上传状态。优先按 upload_id 查；如果 upload_id 找不到，可用 filename 查
（用于跨进程恢复客户端 ID 丢失场景）。

### GET /upload/sessions

列出**当前 owner 未完成**的上传会话（`complete` 成功后的已完会话不列出）。

- 认证：主 mux `user`（SproxySig）/ 隧道内层 `tunnel`
- 响应：
  ```json
  {"success": true, "sessions": [{"upload_id": "…", "filename": "dir/a.bin", "total_size": 12345678,
    "received_count": 2, "total_chunks": 3, "file_checksum": "…", "file_mod_time": 1750000000000000000,
    "status": "uploading"}]}
  ```
  - `status` 恒为 `uploading`（已完会话被过滤）；无会话或上传存储未装配 → `{"success": true, "sessions": []}`
  - 会话按租户隔离（per-tenant store 天然只含本 owner 会话）

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

支持与 `GET /download` 同名的 `ciphertext=1`：at-rest 加密卷按**密文坐标**
（`offset`/`length`/`size` 均为密文字节）返回存储原样字节；缺省返回明文。

> 推荐：标准 `GET /download` + `Range: bytes=` 与本端点等价，且更易穿越 CDN。
> 本端点保留以维持向后兼容、支持 SHA-256 单块校验场景。

## 目录

### POST /mkdir?dirname=...

创建子目录（递归，类似 `mkdir -p`）。

### POST /rmdir?dirname=&force=true

删除目录。`force=true` 时递归删除内容；否则仅允许空目录。
同时清理 checksum store 中相同前缀的所有记录。

### GET /api/du?path=&volume=

按目录递归统计（目录数/文件数/字节）。

- 认证：主 mux `reader`（SproxySig）/ 隧道内层 `tunnel`
- query 参数：
  - `path`：目录相对路径（缺省 = 当前卷 user 桶根；前导 `/` 被忽略，`/` 等同缺省）
  - `volume`：卷名（可选；指定后只在该卷定位，未命中/无权限 → 404，不泄卷存在性）
- 响应：
  ```json
  {"success": true, "data": {"path": "user/dir", "dirs": 2, "files": 7, "size": 1048576}}
  ```
- 错误：`400` 路径非法（含非法 filename）/ 租户不可用；`404` 路径或卷不存在（不泄卷存在性）；
  `500` 遍历失败（错误体 `{"success": false, "message": "遍历目录失败"}`——**fail-closed**，
  不把部分结果当完整结果）
- 外部卷（无本地 Root）走同一可信链的外部 FS 递归统计，跳过 `.__` 元数据与
  `checksums.json`/`LAYOUT_VERSION`；响应 `data.path` 归一为 `/` 开头的用户可见路径。

## 回收站（trash）

软删（`POST /delete?filename=…&soft=true`）把文件移入**文件所在卷**的 `trash` 桶，可在 TTL 内恢复。
多卷语义：各卷独立管理 + 列表汇总展示（`volume` 字段标识来源）。

- 认证：主 mux `auth`（SproxySig）/ 隧道内层 `tunnel`
- 外部卷软删 **fail-closed**（无回收站实现 → 拒绝）；硬删（不带 `soft`）不受影响

### GET /api/trash?volume=

列出回收站条目。`volume` 缺省 = 汇总 owner 可见的**全部本地卷**；指定则只列该卷。

```json
{"entries": [{"trash_rel": "trash/user/dir/a.txt.__deleted__", "name": "dir/a.txt", "volume": "main"}]}
```

- `trash_rel`：回收站内路径（恢复时作为 `file` 参数）；`name`：解码后的原相对路径；
  `volume`：条目所在卷
- 只列主文件条目（目录与服务端 meta sidecar 条目不列）；无条目时 `entries` 为 `null`
- 错误：卷不存在/无权限 → 该状态码（错误体 `{"entries": [], "message": "…"}`）；其它失败 `500`

### POST /api/trash/restore?file=&volume=

恢复条目回 `user/` 原路径。

- `file`（必填）：`GET /api/trash` 返回的 `trash_rel`
- `volume`（可选）：缺省 = **跨卷定位**（条目只存在于软删时所在卷）
- 响应：`{"success": true, "message": "已恢复"}`
- 错误：`400` file 为空；条目不存在/目标已存在等 → `files.HTTPError` 状态直传；其它 `500`
  `{"success": false, "message": "恢复失败"}`（并记服务端 error 日志）

### POST /api/trash/empty?volume=&all=1

清空回收站。

- `volume=<卷>`：只清该卷；`all=1`：清全部卷；**二者皆无 = 只清默认卷**（破坏性范围不静默扩大）
- 响应：`{"success": true, "message": "回收站已清空"}`（按形态文案分别为「全部卷回收站已清空」/
  「该卷回收站已清空」）
- 错误：卷无权限/不存在 → 状态直传；其它 `500` `{"success": false, "message": "清空失败"}`
- 周期 GC：`trash.gc_interval > 0` 时按 `trash.ttl`（默认 7 天）过期清理**各卷**（`all=true`）

## 文件变更事件流（events，SSE）

### GET /api/events?owner=<owner>

SSE（Server-Sent Events）文件变更事件流：订阅 upload/delete/rename/mkdir/rmdir/version/share 事件。
Web UI 文件列表实时刷新；sclient/脚本可用 `Last-Event-ID` 头重连回放。

- 响应：`Content-Type: text/event-stream`；每事件 `id:<cursor>\ndata:<json>\n\n`，
  data JSON = `{cursor, action, owner, rel, size?}`（rel 相对 user 桶路径）。
- 动作覆盖：`upload`/`rename`/`delete`/`mkdir`/`rmdir`（文件写路径）、`version`
  （版本恢复/删除，restore 时 `size`=恢复后文件大小，delete 时 `size`=0）、
  `share`（分享链接创建，`size`=分享文件大小；载荷**不含 token/password**）。
- `owner` 参数可选：缺省取请求认证 actor（隧道模式需显式传）。
- `Last-Event-ID` 头：从指定游标之后回放（游标单调递增，缓冲容量 1000 条/owner，
  落后过多滚出缓冲则回放失败——客户端应全量刷新兜底）。
- 认证：主 mux 经 `authMiddleware`（直连需验签）；隧道内层裸注册（隧道加密即认证）。
- 订阅者慢消费（chan 满）会被断开，客户端重连可回放。

## 文件分享（share）

### POST /api/shares

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

## URL 解析（resolve）

### GET /api/resolve?url=

通用 URL → 资源信息（大小/类型/文件清单）。扩展按 URL **自动识别**（pikpak 分享等由
`downloader.Registry` 的 `URLResolver` 提供）；未识别 URL 回落通用结果（文件名由路径推导、大小未知）。

- 认证：主 mux `auth`（SproxySig）/ 隧道内层 `tunnel`
- query 参数：`url`（必填；trim 后为空 → `400`）
- 响应（`downloader.ResolveInfo`）：
  ```json
  {"url": "https://example.com/s/EXAMPLE-001", "kind": "pikpak_share", "files": [
    {"id": "file-1", "name": "EXAMPLE-001.mp4", "size": 1048576, "type": "video/mp4",
     "hash": "…", "direct_link": "…"}]}
  ```
  - `kind`：解析扩展名，或 `generic`（回落）；单文件时用 `filename` + `size`，多文件（分享）用 `files`
  - `files[].direct_link` / `hash` 可能缺失（提供方不提供或受限）
- 错误：`400` url 为空；`502` 扩展解析失败（分享不存在/上游网络错误，body 含原因）；
  `503` URL 解析接口未装配

## 云端下载（cloud）

### POST /api/cloud/download

创建云端下载任务。请求体：`{url, filename?}`。

- `url`：外部 HTTP/HTTPS URL（SSRF 防护默认开启：解析到私网/内网/回环地址拒绝，
  `cloud_download_allow_private: true` 关闭）；`filename` 可选保存文件名（默认从 URL 提取）。
- 响应：任务对象（见下）。提交后服务端**异步下载**（客户端断连不中断），下载落
  `<storage_root>/<tenant>/cloud/<taskID>/<filename>`。

### PikPak 混合下载：匿名区自动探测 + 进度 WAL

- **匿名区上限自动探测**（`pikpak.hybrid.share_ratio: 0`，默认）：下载前用 Range 请求二分
  探测分享直链的 **416 边界**（`ProbeAnonymousLimit`），`[0, 边界)` 走匿名直链（不耗账号
  配额），其余走账号转存后 FETCH；探测失败回落 50%。显式 `share_ratio > 0` 表示上限（与探测取小）。
- **进度 WAL（`disable_progress_journal: false`，默认启用）**：每个分片完成即写 audit WAL
  （节点本地 `<storage_root>/meta/state/pikpak-journal`，按下载作用域隔离、SyncAlways）；
  进程重启后重放已完成分片（manifest 丢失/被改名也能续传），下载成功后写**提交记录**
  （`MarkDone`）供统一恢复协调器跳过。WAL 源身份（share/file/hash/total）与目标不一致时
  **不合并**旧分片（避免把旧内容当已完成）。

### 转存产物 meta（transfer）

`POST /api/cloud/download` 带 `transfer: {volume, path}` 时，服务端把下载产物转存进卷，
并在目标对象 meta.Extra 写入来源与审计键（客户端 `meta_extra` 同名键覆盖服务端值）：

| 键 | 说明 |
| --- | --- |
| `source` | 固定 `transfer`（`ValidateExtra` 白名单枚举含该值） |
| `source_url` | 源 URL（含 `spx_target` 等定位参数，原始编码） |
| `etag` / `checksum` / `checksum_algorithm` | 服务端唯一标识与本地 SHA-256（带算法标识） |
| `authority_hash` / `authority_hash_algorithm` | 提供方带外权威 hash（如 PikPak GCID） |
| `transferred_at` / `downloaded_at` | 转存时刻 / 下载完成时刻（UTC RFC3339） |
| `x_source_created_at` / `x_source_modified_at` | 源文件创建/修改时间（未知不写） |
| `x_download_started_at` / `x_download_finished_at` | 本次下载起止（UTC） |
| `x_download_duration_ms` | 下载耗时（毫秒） |
| `x_download_anonymous_bytes` / `x_download_account_bytes` | 匿名 / 账号通道下载字节 |
| `x_download_bytes_per_sec` | 平均带宽（字节/秒） |

审计键均为 `x_` 自定义扩展（白名单外），客户端读取原始值；`sclient meta` 默认把时间
转本地时区显示，`--json`/`--raw` 输出原始 UTC。

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
  "created_at": "...", "updated_at": "...", "expires_at": "...",
  "download_stats": {
    "started_at": "...", "finished_at": "...", "duration_ms": 0,
    "anonymous_bytes": 0, "account_bytes": 0, "bytes_per_second": 0,
    "source_created_at": "...", "source_modified_at": "..."
  }
}
```

`download_stats` 是下载器的**实测统计**（下载器未统计时字段缺省，服务端不编造）：
分通道字节（匿名直链 / 账号通道）、耗时、起止时刻、平均带宽，以及源文件
创建/修改时间（来自提供方带外元数据，如 PikPak `created_time`/`modified_time`）。

> 时间语义（2026-10-28 裁定）：转存产物的 `mtime` 取**源文件修改时间**；源时间未知时
> 回落到下载**开始**时刻，绝不使用「下载完成时刻」冒充源时间；目标卷 `ctime` 是首次
> 写入时刻，源创建时间见 `download_stats.source_created_at` / meta `x_source_created_at`。

### POST /api/cloud/tasks/{id}/cancel

取消下载任务（幂等）。

### POST /api/cloud/tasks/{id}/resume

恢复/重新启动任务（支持续传）。

### POST /api/cloud/tasks/{id}/archive

将任务已下载文件打包为 tar.gz（`cloud_archive_max_bytes` 限制原始大小总和）。

请求体可选字段 `target_volume`：非空 = **统一转存**——打包后经可信卷写视图转存到该卷
（**任意存储卷**：本地卷与外部卷皆可——统一卷模型 U3，见 docs/designs/2026-10-28-unified-volume-model.md；
本地卷 target 产物同样走 `<owner>/user/<rel>` 键空间、可经 `/download` 命中），不落租户 archive 桶。
可选 `target_path` 指定目标键（目标卷 **user 桶内**相对路径，可含子目录；空 = 默认 `archive/<archive_name>`，
以 `/` 结尾 = 目录并拼上归档名）。目标键实际为 `<owner>/user/<target_path>`（经 `volume.ResolveUserLocation`
唯一入口，与读路径同键空间；外部卷不走本地文件索引，故不出现在 `/api/files` 列表），响应 `file` 为
**展示用** URL（非归档名；本地卷形态 `local://<卷>/<rel>`，见 U4/R16——注意：本地卷产物
请用 `GET /download?filename=<rel>` 取用，`local://` URL 供标识/trace 展示，不承诺经
`/api/resolve` 解析为可下载资源；外部卷 `scheme://<卷>/<rel>` 语义不变）。空/缺省 = 沿既有
行为落租户 archive 桶。同名目标已存在 → 409（拒绝静默覆盖）。可选 `encrypt`（true = 打包流式加密，密钥来自服务端 `archive.key_file`，
产物命名追加 `.aes`，但以 `target_path`/默认名为准）、`cipher`（默认 aes-256-gcm，**须与 `encrypt`
同用**——单独指定不加密且无效果 → 400，防误信已加密）、`compression`（gzip 默认 / zstd / brotli；
非法值 400）——均仅与 `target_volume` 同用（未指定 target 时设置 `target_path`/`encrypt`/`cipher`/
`compression` → 400，防静默忽略）。CLI 用**卷 URI 路径**（`--target <卷名>:/<路径>`，cd/mv 同规约）
指定，客户端归一为 `target_volume`/`target_path` 后发送（协议层不解析 URI）。

### POST /api/cloud/archive

批量归档（请求体含任务 ID 列表）。支持与单任务相同的 `target_volume` 统一转存语义。

### DELETE /api/cloud/tasks/{id}

删除任务（含云端文件）。

### POST /api/cloud/groups

创建任务组。请求体：`{name, urls: [...]}`（组内每项独立任务）。

### GET /api/cloud/groups

列出任务组。

### GET /api/cloud/groups/{id}

查询组详情（含子任务状态聚合）。

### POST /api/cloud/groups/{id}/cancel

取消整个组（组内未终态子任务一并取消）。

- 认证：主 mux `auth`（SproxySig）/ 隧道内层 `tunnel`；组按 owner 过滤，跨 owner 视为不存在
- 响应：`{"status": "cancelled"}`
- 错误：`404` 组不存在（跨 owner 同此，不区分）；其余失败 `400`（错误体 `{"error": "..."}`）

### POST /api/cloud/groups/{id}/resume

恢复/重新启动整个组：对组内每个子任务调用单任务恢复（续传语义），组内已被单独删除的子任务
跳过（不因此报错），最后刷新组状态。

- 认证：同 `/cancel`
- 请求体（可选，≤1 KiB）：`{"force": true}`——`force` 透传给子任务恢复：`true` = 丢弃已有产物
  （结果文件 + `.partial` + `.partial.etag`）重新下载；`false`/缺省 = 保留 `.partial` 由下载器续传
- 响应：`{"status": "resumed"}`
- 错误：`404` 组不存在；`400` 子任务恢复失败（多个错误合并返回）

### POST /api/cloud/groups/{id}/archive

把组内**已完成**子任务的产物打包为单个 tar.gz（未完成/已删除的子任务跳过并计入
`skipped_tasks`）。

- 认证：同 `/cancel`
- 请求体（≤1 MiB，可选）：`{archive_name?, target_volume?, target_path?, encrypt?, cipher?, compression?}`
  - `archive_name`：归档名（默认 `<groupID>-<unix秒>.tar.gz`；取 basename，缺 `.tar.gz` 自动补齐；
    空/`.`/`..`/超 255 字节 → 400）
  - `target_volume`：非空 = 打包后**统一转存**到该卷（任意存储卷，含本地卷；写视图 + owner 前缀键 +
    本地 staging 配额 + `per-(owner,rel)` 写锁）；空 = 落租户 archive 桶
  - `target_path`：目标卷 **user 桶内**相对路径（空 = `archive/<archive_name>`；以 `/` 结尾 = 目录）
  - `encrypt` / `cipher`（默认 `aes-256-gcm`）/ `compression`（`gzip` 默认 / `zstd` / `brotli`）
    仅与 `target_volume` 同用，单独指定 → 400（防静默忽略）
- 响应：`{"success": true, "file": "<归档名或转存 URL>", "size": N, "checksum": "...",
  "task_count": N, "skipped_count": N, "skipped_tasks": ["<taskID>"]}`
- 错误：`404` 组不存在；`400` 无可归档文件 / 请求体或归档名非法 / 选项误用 /
  超过 `cloud_archive_max_bytes`；`403` 目标卷 ACL 拒绝；`409` 同名归档已存在；`507` 配额/容量不足；
  `500` 打包/写盘失败
- 转存成功后组记录 `archive_file`；组删除时联动清理该产物（要求目标卷仍在服务端装配中；卷已移除时
  删除组会 fail-closed 报错，不静默遗留产物）。

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

### GET /api/hub/federation/services

本 hub 节点宣告的服务（供**联邦对端拉取**，按 mesh 隔离）。

- 认证：主 mux `auth`（SproxySig）；仅当 hub 路由表已装配**且** `hub.federation.enabled: true`
  时注册（否则路由不存在 → `404`）；不注册 localMux（联邦是 hub 间直连 HTTP 同步，不经隧道）
- mesh：取自请求 ctx（认证层注入）；无 mesh 时返回空 mesh 的服务集
- 响应：`[{"node": "node-a", "mesh": "m1", "name": "volread", "addr": "127.0.0.1:19000"}]`
  （`mesh` 为空时省略；无服务时为空数组 `[]`）
- 错误：`404` `hub 未启用`（路由表未装配）；JSON 序列化失败 → `500`

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

## 外部认证登录面（OIDC / LDAP）

外部身份登录（roadmap 11.7-⑥）。仅在 `external_auth.oidc.enabled` / `external_auth.ldap.enabled`
为 `true` 时注册（未启用 = 路由不存在 → `404`，零回归）。

- 认证分类：`public`（主 mux，`ipGate` 白名单——登录前无法做 SproxySig）/ 隧道内层 `tunnel`
- 登录成功后发会话 cookie `sproxy_session`（HttpOnly + SameSite=Lax + `Secure` 由 TLS 开关决定；
  HMAC-SHA256 签名）；后续请求凭 cookie（或 OIDC `Authorization: Bearer <id_token>`）经认证链识别。
  会话密钥 `external_auth.session_secret` 未配置时回落进程内随机密钥——**重启即全员下线**

### GET /auth/oidc/login

发起 OIDC Authorization Code + PKCE 登录。

- 服务端生成 PKCE `code_verifier` + 一次性 `state`（有效期 10 分钟，内存表上限 4096 条），
  然后 `302 Found` 重定向到 issuer 的 `authorization_endpoint`（带 `response_type=code`、
  `client_id`、`redirect_uri`、`scope`、`state`、`code_challenge`+`code_challenge_method=S256`）
- 错误：`503` discovery 不可用（无法获取授权端点）；`500` 随机数/重定向 URL 构造失败

### GET /auth/oidc/callback?code=&state=

OIDC 授权码回调。

- 校验 `state`（**单次使用**，用完即删）→ 用 `code` + `code_verifier` 向 `token_endpoint` 换取
  `id_token` → 验签与 claims（iss/aud/exp）→ 按 `claim_owner`（缺省 `email`，回退 `sub`）映射 owner
  → 发会话 cookie → `302 Found` 到 `/ui/`
- AK 由 `sub` 稳定派生（`oidc-<sha256(sub)[:16hex]>`）
- 错误：`400` 缺 `code`/`state`；`401` state 无效或过期 / token 交换失败 / id_token 校验失败；
  `503` discovery 不可用；`500` 会话签发失败

### POST /auth/ldap/login

LDAP 绑定登录。请求体 JSON：`{"username": "alice", "password": "…"}`。

- 流程：服务账号 bind（`bind_dn` 可空 = 匿名 search）→ 按 `user_filter`（`{{username}}` 占位符，
  LDAP filter 转义）搜用户 DN → 用户 DN bind 验证口令 → 映射 owner（`username_attr` 属性值，回退用户名）
- 成功：`200` + `{"ok": true}`，并下发会话 cookie（AK = `ldap-<sha256(username)[:16hex]>`）
- 错误：`400` 请求体非法 / 缺用户名或口令；`401` bind/search/口令失败（**统一文案**「登录失败」，
  不泄露账号存在性）；`500` 会话签发失败。`ldaps://` 严格校验证书（不支持 InsecureSkipVerify）

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

### GET /api/mesh/acl

**跨节点授权只读视图（W3）**：返回**调用者自己 owner** 的跨节点授权（服务端卷 ACL 的 `mesh_readers` 条目）。

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

### GET /metrics

Prometheus 文本格式运行时指标（`text/plain; version=0.0.4`）。认证由 `MetricsAuth` 把关
（`Authorization: Bearer <api_key>` 或 SproxySig 凭据 AK；未认证 `401`）。

指标族见下文各小节（跨节点 / mux 传输质量 / 卷健康 / 派生状态 / 内存观测）；
无样本时仍输出 `HELP`/`TYPE` 头（让「一直没数据」与「指标不存在」在面板上可区分）。

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

### Mux 传输质量指标（`GET /metrics`）

`/metrics` 另暴露 mux 层**传输质量**计数器（roadmap §6 P1：面板可见各传输质量），
从 MeshRouteTable 所有已注册 mux 实例聚合（跨全部 mesh 汇总）：

```
# 重传成功次数（数据帧首次 Send 失败入队、退避后重发成功——瞬时故障自愈；持续增长提示连接劣化）
sproxy_mux_retransmits_total 3

# 重传队列满（256 帧上限）而关闭的 mux 数（大量帧 Send 失败 = 连接不可用，fail-closed）
sproxy_mux_retransmit_queue_full_total 1

# 重传重试耗尽而关闭的 mux 数（退避预算用尽，连接不可用）
sproxy_mux_retransmit_exhausted_total 2
```

既有相关指标（传输质量家族）：`sproxy_mux_stream_overflow_spills`（接收背压溢出帧数）、
`sproxy_mux_stream_window_violations`（对端超窗口被 abort 的流数）、
`sproxy_mux_readloop_push_*`（接收背压等待耗时，即传输延迟指标）、
`sproxy_mux_datagram_handler_drops`（数据报丢包数）。

### 卷健康指标（`GET /metrics`，roadmap §3 P1）

每卷读写延迟/失败率入 `/metrics`（带 `volume`+`op` 标签）——面板可见每卷健康（失败卷告警数据源）：

```
# 该卷该操作（upload/download）的请求总数
sproxy_volume_io_total{volume="main",op="upload"} 42

# 失败数（失败率 = 失败/总数）
sproxy_volume_io_failures_total{volume="main",op="upload"} 1

# 累计延迟（纳秒，成功+失败都计入）
sproxy_volume_io_latency_nanos_total{volume="main",op="upload"} 20000000
```

- `volume` = 落盘/读取定位命中的卷名（upload 成功响应头 `X-Volume` 同源；卷未装配/旧路径回落时为空串）；
- `op` ∈ `upload` | `download`；`ok`（请求成败）只影响失败计数器，总数与延迟都记（失败也耗时）；
- 标签值按 Prometheus 文本格式转义；无样本时仍输出 `HELP`/`TYPE`。

### 派生状态指标（`GET /metrics`，2026-10-29）

卷状态缓存（list 数据源）与搜索索引（search 数据源）的可观测面——命中率判断缓存是否生效/
被进程外改动打穿，索引规模与重建次数判断内存与陈旧：

```
sproxy_volume_cache_hits_total      # 缓存命中（list 未回源）
sproxy_volume_cache_misses_total    # 未命中（回源到卷 FS）
sproxy_volume_cache_entries         # 内存中缓存条目数（LRU）
sproxy_search_index_owners          # 内存中索引的 owner 数（LRU 上限 128）
sproxy_search_index_entries         # 内存中索引条目总数
sproxy_search_index_rebuilds_total  # 全量重建次数（冷启动/失效/LRU 淘汰后重建）
```

- 未装配缓存/索引时不输出该组（不伪造 0）；
- `rebuilds_total` 持续快速增长 ⇒ 失效过于频繁（检查 refresh/旁路写）或 LRU 过小；
- `misses_total` 持续增长而 `hits_total` 不增 ⇒ 缓存未命中（TTL=0 下多因写穿失效/显式刷新）。

### 内存观测指标（roadmap §6 P2）

`/metrics` 暴露进程级堆分配指标（`runtime.MemStats` 拉取时采样，无标签）：

```
# 当前堆分配字节（HeapAlloc）
sproxy_heap_alloc_bytes 12345678

# 当前堆对象数（HeapObjects）
sproxy_heap_objects 54321

# 已完成的 GC 周期数（NumGC，累计）
sproxy_gc_cycles 42
```

`/debug/pprof/`（heap/goroutine/allocs/block/mutex + cmdline/symbol/trace）受认证保护：
`debug_pprof_enabled: true` 显式开启（默认关 = 404），且必须经 SproxySig/APIKey 认证（未认证 401）。
启用状态启动日志可观测（安全开关铁律）。

### GET /debug/pprof/

Go 标准库 pprof 子树：`heap` / `goroutine` / `allocs` / `block` / `mutex` / `cmdline` /
`symbol` / `trace`。仅在 `debug_pprof_enabled: true` 时挂载（默认关 = `404`，零回归），
且经 `authMiddleware` 认证（SproxySig / APIKey；未认证 `401`）。启用状态在启动日志中可观测
（安全开关铁律）。

## 信令（signal，hub 中转 WebRTC）

Hub 中转 WebRTC 的信令投递面。仅 `hub.enabled` 且路由表装配时注册（否则 `404` `hub 未启用`）。
主 mux `auth` / 隧道内层 `tunnel` 双注册。

**身份绑定（所有信令端点）**：请求头 `X-Node-ID`（声明自身节点）与 `X-Node-Secret`
（B1 下发的 per-node secret，恒定时间比对）必须与路由表登记一致；节点必须与请求 ctx 的 mesh 一致。

- 缺少 `X-Node-ID` → `400`；节点未注册 → `400`；per-node secret 为空或不匹配 → `403`；
  跨 mesh → `403`
- `SignalMsg` 字段：`id`（服务端去重 ID）、`kind`、`from`（服务端从 `X-Node-ID` 派生，**忽略 body 伪造值**）、
  `to`、`sdp`、`cand`（trickle ICE 预留，当前恒空）、`at`（毫秒时间戳，服务端设置）
- 队列容量受限：全局满 / per-sender 配额超 → `429`（调用方需回落重试）；
  `hub.persist_file` 配置时收件箱快照同步落盘

### POST /api/signal/offer

投递一条 `kind=offer` 信令到 `to` 节点收件箱。

- 请求体（≤ 8 KiB）：`{"to": "peer-b", "sdp": "…"}`（`from` 由服务端填充）
- 响应：`202 Accepted`（空体）
- 错误：`400` 解析失败 / 缺少 `to` / 给自己发 / `to` 节点未注册 / `from` 与 `to` 跨 mesh；
  `413` 消息体超 8 KiB；`429` 队列满

### POST /api/signal/answer

同上，投递 `kind=answer`（请求体与错误码同 `offer`）。

### POST /api/signal/candidate

同上，投递 `kind=candidate`（trickle ICE 预留注入点；当前 ICE 全内联 SDP，保留兼容旧对端）。

### GET /api/signal/poll/{peer}?kind=

长轮询取走**自己**收件箱的一条信令（取走即从队列删除；请求者 `X-Node-ID` 必须等于 `peer`，
否则 `403`）。

- query 参数：`kind`（可选，`offer` / `answer` / `candidate`；非空时只取该 kind，其余 kind 保留在队列）；
  空 = 任意 kind
- 响应：`200` + `[{…SignalMsg…}]`（0 或 1 条）；最长等待 `hub.PollTimeout`（25s），超时返回空数组
- 错误：`400` 缺少 `X-Node-ID` / 节点未注册；`403` `peer` 与调用方身份不一致 / secret 不匹配 / 跨 mesh；
  `404` hub 未启用

## 卷列表（GET /api/volumes）

### GET /api/volumes

列出当前 owner 可见卷（系统盘 + 用户卷，ACL 过滤后视图）。每卷含健康状态 `state`（roadmap 3.3 P1
后端健康探针）：

```json
{"volumes": [{"name": "main", "mode": "", "capacity": 0, "usage": 12345, "allowed": true, "state": "healthy"}]}
```

- `state` 取值：`healthy`（探针通过）/ `degraded`（后端不可达）/ `unknown`（后端未实现探针）
- 本地卷恒 `healthy`；外部卷（baidupcs/webdav/s3/sftp）探针缓存 30s（列表触发惰性探测）

### 冷热分层（roadmap §3 P1：卷 `tier` + 自动降级 + 读时回迁）

卷配置 `volumes[].tier` 声明 hot/warm/cold（缺省 hot）。冷热分层行为：

- **自动降级**（`tier_policy.interval > 0` 时启用）：周期扫描 hot 卷 user 桶，把 mtime 超过 `max_age_hot` 且 size 超过 `min_size_hot` 的文件迁移到同 owner 视图内第一个 tier=cold 的本地卷（复用 rebalance 原子迁移核心；迁移可中断续跑——任务不持跨 tick 状态）。迁移成功记审计 `volume_tier_downgrade`（detail 形如 `default→cold1`）。
- **读时回迁**：普通下载（`GET /download`）定位命中 cold 卷文件时，自动迁移回同 owner 视图内第一个 tier=hot 的本地卷（API 无感；成功记审计 `volume_tier_promote`）。无 hot 卷/回迁失败 → 直接从 cold 卷读取（尽力而为，不中断下载）。
- cloud_archive / cloud_task 功能桶不参与分层（非 user 桶对象）。

## 卷状态缓存刷新（POST /api/volumes/refresh）

### POST /api/volumes/refresh

强制刷新卷缓存/索引——**默认永不过期**的缓存下，这是感知「进程外改动」的唯一入口
（用户裁定 2026-10-29）。本进程的写由事件写穿（下一次读立即可见），但本机直写盘 / 同根多实例 /
备份恢复 / 网盘 App 直改不会自动收敛。

```bash
POST /api/volumes/refresh                # 清空**全部卷**状态缓存 + 全部搜索索引
POST /api/volumes/refresh?volume=<卷名>   # 只清该卷状态缓存（本地卷同样适用）+ 全部搜索索引
```

**搜索索引同步失效**（2026-10-29）：索引是 owner 维度、无法按卷精确映射 ⇒ 两种形态都**全量失效**
（清内存 + 删除全部 `<meta>/index/<owner>.json` 快照，含本进程未知的 owner）。否则会出现
「列表已更新、搜索仍返回陈旧结果且无任何入口可纠正」。

- 本地卷：清空该卷状态缓存（本地卷自 2026-10-28 起也走缓存）；搜索索引按上述规则全量失效
- 外部卷：清空状态缓存；后端实现 `IndexRefresher`（secretdata）时**额外**重扫内存索引并原子换入
- 落盘快照同步清除（否则重启会复用旧快照）
- 响应：`{"success": true, "message": "..."}`；未知/未授权卷 → 403（防枚举）；未装配缓存且无索引 → 405
- 审计：`action=volume_refresh`（`object` = 卷名，全部卷形态为 `*`）
- 等价入口：CLI `sclient volume refresh [<卷名>]`、Web UI 工具栏「强制刷新」按钮

## 卷运维（move / rebalance / copy / export / import / backup）

以下端点均为写/读卷操作，owner 恒取自请求认证；`from_volume` / `to_volume` 必须在 owner 卷视图内
（否则 `403`，不泄卷存在性）。响应统一形如 `{"success": bool, "message": "…"}`（rebalance/copy
另有结构化字段）。

### POST /api/volumes/move?from_volume=&to_volume=&filename=

跨卷移动同 owner 同 rel 文件（`move` 保留目标、删除源）。

- 认证：主 mux `user`（写子组）/ 隧道内层 `tunnel`
- query 参数：`from_volume`、`to_volume`、`filename`（均必填；缺参 → `400`；`filename` 经
  `ValidateFilePath`）
- 语义：`from==to` = 同卷 no-op 成功（幂等）；跨卷 = 目标侧双账本预留（owner 配额 + 目标卷容量）
  → 流式复制到临时文件 + fsync + 原子 rename → 删源 → 结算；单文件锁与上传共用（同 rel 并发恰一成功）
  - 本地卷→本地卷走路径式原子核心；**含外部/加密/封装卷**时交域侧统一写内核（封装卷不能作 move 目标）
- 错误：`400` 缺参/路径非法/卷未装配/目标卷不支持；`403` 卷不在 owner 视图；`404` 源不存在；
  `409` 目标卷（或视图内其它卷）已存在同 rel / 同文件并发；`507` 配额或容量不足（含目标 meta 配额探测失败）；
  `500` 复制/结算失败
- 审计：`volume_move`（成功含字节数；失败/拒绝也留痕）

### POST /api/volumes/rebalance?from_volume=&to_volume=&max_bytes=

卷再平衡：把 `from` 卷 user 桶文件按大小降序逐文件迁到 `to` 卷，直到 `max_bytes` 用尽或无可迁文件。

- 认证：主 mux `user` / 隧道内层 `tunnel`
- query 参数：`from_volume`、`to_volume`（必填）；`max_bytes`（可选，0/空 = 不限；非整数/负数 → `400`）
- 语义：逐文件复用 move 的原子单文件核心；**单文件失败跳过继续**（尽力而为）；`from==to` = no-op 成功
- 响应：
  ```json
  {"success": true, "message": "卷再平衡完成: 3 个文件、1048576 字节已迁移",
   "moved": 3, "bytes_moved": 1048576, "remaining": 2097152}
  ```
  （`remaining` = 迁移后 `from` 卷容量池 `Usage`；在途进度由 `/metrics` 的
  `sproxy_rebalance_progress{from_volume,to_volume}` 暴露，任务结束后序列消失）
- 错误：`400` 缺参/`max_bytes` 非法/卷未装配；`403` 卷不在 owner 视图；`500` 列举源卷失败
- 审计：`volume_rebalance`（`object` 为 `from→to`）

### POST /api/volumes/copy?from_volume=&to_volume=&filename=

跨卷复制（保留源）。

- 认证：主 mux `user` / 隧道内层 `tunnel`
- query 参数：`from_volume`、`to_volume`、`filename`（必填）；`to`（可选目标路径，缺省 = `filename`）；
  `force=true`（可选，允许覆盖已存在目标）
- 两种形态：
  - **经典跨卷同 rel**（无 `to`/`force`，两侧均本地卷）：双预留 + 流式 + 原子 rename；目标卷已有同 rel
    且 checksum 一致 → `200` 幂等成功（不重复计费），不一致 → `409`（不静默覆盖）
  - **通用复制**（目标路径不同 / `force` / 任一侧外部卷）：交 `files.Service.CopyFile` 统一写内核
    （外部卷→本地卷重新加密；同卷 + 卷支持 blob 直拷时不重新加解密）
- 响应：`{"success": true, "message": "…", "checksum": "…", "size": 1048576}`（幂等命中另带
  `"idempotent": true`）
- 错误：`400` 缺参/路径非法/卷未装配；`403` 卷不在 owner 视图；`404` 源不存在；`409` 目标已存在；
  `507` 配额/容量不足；`500` 读写失败

### GET /api/volumes/export?volume=&strict=true

卷备份导出：流式 `application/x-tar` 响应，逐文件写 `user/<rel>` 条目，末尾附 `manifest.json`。

- 认证：主 mux `reader`（只读子组）/ 隧道内层 `tunnel`
- query 参数：`volume`（可选；**缺省 = owner 可访问的全部卷合并导出**，显式指定则只导该卷）；
  `strict=true`（可选，导出中源文件被并发修改/删除时中止流，缺省宽松：该条目记 warning 继续）
- 响应头：`Content-Type: application/x-tar`、`Content-Disposition: attachment; filename="volume-<卷>-<unix>.tar"`
- `manifest.json`：`{"volume": "main", "files": [{"path": "user/dir/a.txt", "size": 123,
  "mtime": 1750000000000000000, "checksum": "…", "ledger": "…"}], "warnings": ["…"]}`
  （`checksum` = 导出时实时重算；`ledger` = 台账登记值，供交叉校验）
- 错误：枚举失败 → `500` `{"success": false, "message": "读取卷文件失败"}`（JSON，在写 tar 头之前）；
  非 strict 下单条目失败仅记入 `warnings`

### POST /api/volumes/import?volume=&overwrite=true

卷备份导入：请求体 = `application/x-tar` 流（与 export 产物同构）。

- 认证：主 mux `user` / 隧道内层 `tunnel`
- query 参数：`volume`（可选；缺省 = 默认卷）；`overwrite=true`（可选，允许覆盖同名文件；缺省跳过）
- 语义：先扫描收集 `manifest.json` 与文件条目 → 逐条 `ValidateFilePath`（拒绝穿越）→ 配额 Reserve →
  同名存在且未 `overwrite` → 跳过 → 经统一写路径落盘并登记 checksum 台账 → 有 manifest 时校验 checksum（不符 → 该条失败跳过）
- 响应：
  ```json
  {"success": true, "message": "导入完成", "imported": 2, "skipped": 1, "failed": 0,
   "errors": ["dir/b.txt: checksum 不符"]}
  ```
  （`skipped` = 已存在未覆盖 / 配额不足 / 路径非法 / checksum 不符；单 tar 条目上限 1 GiB）
- 错误：tar 损坏在扫描阶段记入 `errors` 并继续处理已收集条目；响应恒 `200`（部分失败显式化）

### POST /api/backup

卷备份到配置卷（源=本地卷 user 桶，目标=本地卷或外部卷的写面；增量语义由 manifest 比对 size+mtime）。

- 认证：主 mux `auth`（SproxySig / APIKey）/ 隧道内层 `tunnel`
- 请求体（≤ 1 MiB）：
  ```json
  {"target": "backup-vol", "source": "main", "verify": true, "concurrent": 4, "exclude": ["*.tmp"]}
  ```
  - `target`（必填）：目标卷名（ACL 校验；外部卷走其写面）；`source`（可选，空 = 默认卷）
  - `verify`：传输后对目标 `Stat` 校验 size；`concurrent`：并发文件数（≤0 = 引擎默认 4）；
    `exclude`：`path.Match` glob 排除
- 响应（部分失败也 `200`）：
  ```json
  {"files": 10, "bytes": 10485760, "skipped": 2, "failed": 0, "errors": ["dir/a.txt: …"],
   "truncated": false}
  ```
- 错误：`400` 请求体解析失败 / `target` 为空 / 源与目标同卷（自备份） / 源卷根不可用；
  `403` 源或目标卷不在 owner 视图；`502` 远端写面不可达；`500` 备份执行失败
- 审计：`backup`（`failed>0` 或 `truncated` ⇒ `result=error`）

## 用户卷（per-owner 用户自有卷）

用户自有卷是每个 sproxy 用户独立管理的网盘盘（仅外部类型：`baidupcs` 等已注册 backend）。
存储位置：`<storage_root>/<owner>/meta/volume/<name>.json`（原子写，重启扫描恢复）。
寻址：同步任务 `remote.volume` 填用户卷名，任务 owner 必须匹配卷 owner（跨用户 404 防枚举）。

### POST /api/volumes/user

创建用户卷。请求体（JSON）：

```json
{"name": "my-disk-1", "type": "baidupcs", "capacity": 0, "unlimited": false, "extra": {"bduss": "...", "baidu_root": "/disk1"}}
```

```json
{"name": "my-webdav", "type": "webdav", "capacity": 0, "extra": {"url": "https://nextcloud.example.com/remote.php/webdav", "username": "alice", "password": "..."}}
```

- `type` 须服务端已注册 backend（未注册 → 400）；`extra` 为类型特有配置（baidupcs 支持
  `bduss`/`baidu_root`/`binary_path`/`local_root`；webdav 支持 `url`（必填 http(s) 根）、
  `username`+`password`（Basic）或 `token`（Bearer，优先））
- `capacity` 独立卷容量（不计 owner 配额）。**语义（F6）**：
  - `capacity > 0`：显式声明上限（可改大/改小；改小会把已用钳到新上限）；
  - `capacity == 0` 且 `unlimited != true`：**继承同名卷已持久化的上限**。删除用户卷时刻意保留
    计数快照（远端已写入的字节仍在，本系统不删远端内容），若不继承，`delete + recreate` 写
    `capacity:0` 即把上限变「不限」——该绕过已被堵住；
  - `capacity == 0` 且 `"unlimited": true`：**显式不限**（0）。
- 生效上限在**创建期即落盘**（计数快照 + 卷元数据），重启 restore 后保持一致
- 认证：SproxySig / api_keys（owner 从请求派生）；重名 → 错误
- 响应：`{"success": true}`

### GET /api/volumes/user

列出当前 owner 的用户自有卷（按认证过滤，只返回自己的）。

```json
{"volumes": [{"name": "my-disk-1", "type": "baidupcs", "capacity": 0, "extra": {...}}]}
```

### DELETE /api/volumes/user?name=<name>

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

```json
{"direction": "push", "remotes": ["r1", "r2"], "src": "", "dst": ""}
```

- `direction`：`push`（本地→远程）/ `pull`（远程→本地）/ `both`（双向：一次任务内先 push 再 pull，两端一致）
- `remote`：`sync_remotes` 配置的远程节点名（与 `remotes` 二选一；未配置/缺凭据 → 400 fail-closed）
- `remotes`：**多节点扇出**（roadmap 4.3 P2）——一次提交创建多个 remote 子任务，逐节点独立执行/独立失败（单目标失败不阻塞其它）；响应为父任务（聚合视图）
- `conflict_policy`：`skip`（默认）| `overwrite` | `lww` | `conflict_rename`
- `delete_policy`：`skip`（默认，源删除不传播，零回归）| `propagate`（源端删除经一次任务反映到目标，幂等）
- `src`/`dst`：FS 根相对路径（默认 `""` = 整个根）；`include`/`exclude`：glob 过滤器
- `owner` 由请求认证派生（客户端不可伪造）；201（新建）/ 200（去重复用活跃任务）
- 响应：SyncTask JSON（含 `id`/`direction`/`status`/`files_total`/`files_done`/`bytes_total`/`bytes_done`/`files_deleted`/`results` 等）

### GET /api/sync/tasks

列出当前 owner 的同步任务元信息（`{success, tasks: [SyncTaskMeta]}`；含 `files_deleted` 删除传播计数）。

### GET /api/sync/tasks/{id}

查询单个任务详情（含逐文件 `results`）。跨 owner 404。
**扇出父任务**（`remotes` 创建）返回聚合视图 `FanoutSummary`：`{parent_id, direction, total, completed, failed, pending, children:[{remote,status,error}]}`——每 remote 子任务状态一目了然。

### POST /api/sync/tasks/{id}/retry

对任务的失败文件发起单文件重试（roadmap 4.3 P1「失败可重试单个文件」）。请求体 JSON：

```json
{"files": ["dir/a.txt", "b.txt"]}
```

- `files`：要重试的失败文件路径列表；`[]`/缺省 = 重试任务 Results 中全部失败
  （`error`/`verify_failed`）文件
- 语义：服务端构造重试子任务（方向/remote/src/dst 与源任务一致，Include 精确限定失败文件），
  复用同步引擎单文件路径重新执行——**不重跑整个任务**（原任务状态不变）；重试结果回写
  原任务 Results 对应条目
- 幂等：指定了非失败/不存在的文件 → 跳过（不进重试）
- 响应：`{retried: [{path, action, error}], skipped: [path...]}`（`retried` 是重试后的
  逐文件动作：updated/created/verify_failed/error）
- 跨 owner 404（防枚举）

### POST /api/sync/tasks/{id}/cancel

取消进行中任务（pending/syncing/retrying）。跨 owner 404。

### DELETE /api/sync/tasks/{id}

删除任务（终态清理）。跨 owner 404。

## 同步冲突（sync conflicts）

三方合并（merge3）产生的冲突索引（未装配冲突索引 → 响应 `400` `sync conflicts not configured`）。
所有端点：主 mux `auth` / 隧道内层 `tunnel`；`ActorFrom(ctx)` 作为 owner 边界。

### GET /api/sync/conflicts

列出当前 owner 的**未解决**冲突（时间升序）。

- 响应：`{"success": true, "conflicts": [ConflictItem…]}`
- `ConflictItem` 字段：`id`、`path`（同步相对路径）、`hunk_count`、`base_sha` / `ours_sha` / `theirs_sha`、
  `ours` / `theirs`（行快照）、`ts`（毫秒时间戳）；已解决条目不在列表出现

### GET /api/sync/conflicts/{id}

单条冲突详情（包含已 `resolved` 的历史条目）。

- 响应：`ConflictItem`（另带 `resolved` / `resolve_at`）
- 错误：`404` `{"error": "conflict not found"}`；未装配 `400`

### POST /api/sync/conflicts/{id}/resolve

解决冲突并把结果写回冲突文件路径（替换带冲突标记的文件）。

- 请求体：`{"choice": "ours|theirs|manual", "content": "…"}`
  - `ours` / `theirs`：取对应侧快照写回；`manual`：必须显式提供 `content`
- 响应：`{"success": true, "path": "dir/a.txt"}`
- 错误：`400` 请求体非法 / `choice` 不支持 / `manual` 缺 content / 取快照失败；`404` id 不存在；
  `500` 写回文件失败（文案含原因）
- 副作用：条目标记 `resolved`（不再出现在列表）；重建目标文件 sidecar；审计 `sync_conflict_resolve`
  （detail `choice=<choice> id=<id>`）

## secrets 卷管理（/api/secrets）

per-owner 默认 secrets 卷的管理面（存储：`<storage_root>/<owner>/meta/secrets/<name>`，租户首次使用自动派生）。
服务端只做「校验 + 落盘」，**不参与口令/密钥派生**，原始口令永不传输到本层。

- 认证：主 mux 写面 `user` / 读面 `reader`（SproxySig）；隧道内层 `tunnel`
- 失败形态：请求者租户不可用 → 纯文本 `503` `secrets 卷不可用（请求者租户未装配存储根）`
  （非 JSON）；内部 IO 失败 → `500`。审计动作：`secret_create` / `secret_export` / `secret_delete`

### POST /api/secrets

创建（追加/覆盖）一个 secret。请求体（≤ 1 MiB）：

```json
{"name": "main-key", "mode": "random", "value": "", "origin": "random"}
```

- `name`：secret 名（trim 后使用；非法名/路径穿越 → `400`）
- `mode`：`random`（缺省；服务端 `crypto/rand` 生成 32B→hex）| `import` / `passphrase`
  （客户端传 `value`，服务端校验 64 位 hex 后落盘）
- `origin`：来源标注（信息性，不影响落盘内容）
- 响应：`{"name": "main-key", "value": "…", "origin": "random", "message": "ok"}`
  （`value` **仅 random 模式返回**，供创建后立即备份；import/passphrase 不回显）
- 错误：`400` 请求体解析失败 / 未知 mode / 非法名或非法 value；`500` 落盘失败

### GET /api/secrets

列出 owner 默认 secrets 卷内全部 secret 名（排序）。

- 响应：`{"secrets": ["main-key", "other"]}`
- 错误：`500` 列举失败

### GET /api/secrets/{name}

导出 secret 内容（**明文 hex 文本**，供备份/迁移；安全敏感，恒记审计）。

- 响应：`{"name": "main-key", "value": "<hex>", "origin": "export"}`
- 错误：`400` 非法名；`404` 不存在（fail-closed）；`500` 读取失败

### DELETE /api/secrets/{name}

删除 secret（不存在 → `404`，不静默 no-op）。

- 响应：`{"name": "main-key", "deleted": "true"}`
- 错误：`400` 非法名；`404` 不存在；`500` 删除失败

## 卷后端类型（GET /api/backends）

### GET /api/backends

返回服务端已注册的卷后端类型 + 每类型的建卷表单 schema（动态，随 `RegisterBackend` 注册变化）。

```json
{"backends": [{"type": "s3", "category": "linked", "label": "",
  "fields": [{"key": "endpoint", "label": "Endpoint", "type": "text", "required": true}]}]}
```

- `category` ∈ `linked`（外部后端）/ `wrapper`（封装/密钥类，如 `secrets`/`secretdata`/`egress`）/
  `mt-local`（本地）；`fields[].type` ∈ `text` / `volume-select` / `enum` / `bool` / `number`；
  `options` 仅 `enum` 有值；`required=true` 的键必填。未实现 `SchemaProvider` 的后端 `fields` 为空数组
  （前端只填卷名）
- 供 Web UI 类型下拉 / sclient 提示已注册类型
- 新增外部后端 = 新 backend 包 `RegisterBackend(type, factory)` 注册，前端自动感知
  - `s3`：`endpoint`/`bucket`/`access_key`/`secret_key`（必填），`region`/`use_ssl`/`local_root`（可选）；大文件分片上传（roadmap 3.3 P1）：`multipart_threshold`（分片阈值，默认 64MiB）、`multipart_part_size`（分片大小，默认 16MiB，minio 5MiB 下限钳制）、`upload_retries`（失败重试次数，默认 3）
  - `sftp`：`url`（`sftp://user@host[:port][/root-path]`，必填）+ `private_key` 或 `password`（二选一），`root`（远端根，可选）

### POST /api/backends/{type}/presign

预签名直传 URL 签发（服务端持凭据代签）。

> **卷上默认禁（2026-10-18，#44① (b)）**：`method=PUT` 且目标命中**已配置卷**时，服务端
> **默认拒绝（403）** —— PUT 由客户端直传对象存储，服务端**看不到那次写**；`presign/complete`
> 只是事后登记 ⇒ 客户端可 PUT 后不调 complete，在卷内留下**未记账对象**（owner 配额永不收敛）。
> 需在该卷配置显式开启 `extra.allow_presign: true` 才放行（默认禁 + 按卷豁免）。
> `method=GET`（读）不受限；无已配置卷的裸后端不受影响。

服务端持凭据为客户端签发对象存储直传 URL（当前 `s3`），客户端 PUT 后经登记端点确认。

query 参数：`path`（卷内相对路径，必填）、`method`（`PUT`/`GET`，必填）、`expires`（正整数秒，可选）。

- **路径 owner 收敛（硬性）**：`path` 由服务端重绑到**调用者**的键空间——
  裸相对路径（`dir/a.txt`）与 `user/dir/a.txt` 均归一为 `<owner>/user/dir/a.txt`；
  已带 `<owner>/…` 前缀时 owner 必须等于调用者。**越权（他人 owner 前缀 / 非 `user` 桶 /
  `meta` 功能桶）→ 403**。这样对象落点与文件 API 一致（此前落裸 key、文件 API 不可见）。
- 响应 `{"url": "…"}`；未注册类型 → 404；后端不支持预签名 → 405；`method`/`expires` 非法 → 400。

### POST /api/backends/{type}/presign/complete

预签名直传完成后的登记（服务端校验对象存在 + 记账）。

> **登记是义务，不是可选**：complete 经**统一写内核**（`ContentPresent` 模式）完成存在性/size
> 校验（fail-closed）+ **双账本结算**（owner 配额 + 卷容量池）+ 元数据/台账 —— 与普通上传同一套
> 记账原语。客户端须声明 `size` 与 `sha256`（服务端无法在直传后廉价读取对象内容）；缺任一 ⇒ 400。

客户端直传完成后**登记**：服务端按上述同一 owner 收敛路径校验对象存在且大小与声明一致，并经
**统一写内核**（内容已在位模式）完成登记。

query 参数：`path`（必填）、**`size`**（直传对象字节数，非负整数，必填）、**`sha256`**
（整文件 SHA-256，64 位十六进制，必填）。

- **记账（2026-10-14 P0 收口）**：登记走与上传**同一套**双账本原语（owner Scope + 卷容量池
  预留→结算）并写 checksum 台账（本地卷另写完整性 sidecar、更新搜索索引、发布 upload 事件）。
  ⇒ 直传对象与上传对象在容量/配额/元数据上**同口径**，不再有旁路。
- 错误：缺 `size`/`sha256` 或格式非法 → **400**；对象不存在 → **404**；**大小与声明不符 → 409**
  （fail-closed，宁可拒绝也不记错账）；配额/容量不足 → 403/507；越权路径 → 403；未注册类型 → 404。
- **已知边界（明示）**：直传内容不经服务端 ⇒ ① 服务端**不重算** `sha256`（重算需完整下载对象），
  只记录客户端声明值；② 账本按**新对象**记账（`prev=0`）——若同路径此前已经内核写入并记账，
  本次会重复计一次（容量池由周期对账收敛，owner 配额侧偏高）。**覆盖既有文件请走上传 API**
  （同一套记账原语，按差分结算）。设计残余见 `docs/designs/2026-10-10-trusted-volume.md` §8。

## 审计（audit /api/audit）

审计日志查看与导出（敏感运维面，主 mux 需 SproxySig/APIKey 认证；隧道内层裸注册，隧道加密即认证）。

> **访问模型（重要）**：审计读面是**全系统视图**，因此**仅管理员可读**
> （主体自身 `Role == "admin"` 优先，其次按 AK 查角色环；未认证/非管理员一律 `403`）。
> 裁决函数 `auditReadAllowed` 是**唯一**权限点，且**不接受任何请求参数放宽**
> （门禁 R35-e 机械保证：`/api/audit`、`/api/audit/export`、`/api/audit/cold/seg` 同一裁决）。
> 条目已含 `owner`（归属租户）与 `volume`（卷维度）字段；将来开放「用户维度自视图」时必须在
> 同一裁决函数内改为「该 owner 的过滤视图 + 脱敏」。

### GET /api/audit

返回最近审计事件（按时间倒序，最新在前）：

```json
{"events": [{"action": "delete", "actor": "ak-…", "owner": "alice", "mesh": "", "volume": "vault",
             "object_type": "file", "object": "file.txt", "result": "success", "detail": "…",
             "request_id": "…", "source_ip": "10.0.0.2", "ts": "2026-09-01T12:00:00Z"}], "total": 1}
```

query 参数：
- `limit`：非负整数（默认 100，>500 clamp 到 500；非法回落默认）
- `action` / `actor` / `mesh` / **`volume`**：过滤（`action` 支持前缀匹配，其余精确相等；空值不过滤）
- `since`：RFC3339 时间，仅返回该时刻之后（`TS.After`）的事件；解析失败 400

`volume` 语义：审计条目的**卷维度**——显式值 > 请求 `?volume=` 绑定 > 单卷部署补默认卷名；
多卷部署中自动放置且未绑定时为**空**（不猜默认卷，避免记错归属）。

### GET /api/audit/export

导出审计事件为 JSON 数组（按 `ts` 升序，时间正序；供日志 collector 顺序回放）。

query 参数：`action` / `actor` / `volume`（同上）、`after_ts`（RFC3339，仅导出该时刻之后；解析失败 400）。

- 未启用审计时返回空数组 200
- 审计默认落盘（`<默认卷根>/audit/audit.log`）：导出全量历史（含重启前事件）；冷段（归档）合并进结果
- 读面方向：`newest-first + limit` 走**冷段尾读**（只读最新段尾部），升序导出走按时间扫描

### GET /api/audit/cold/seg

按区间读取**已归档冷段**（取证式随机读：管理员按需取任意区间，不必整段下载）。

query 参数：
- `path`：冷段键（**必须来自服务端冷段清单白名单**；不在白名单 ⇒ `404`，防目录穿越/探测）
- `offset`：起始字节偏移（默认 `0`；非法 400；超出大小则夹紧为空区间）
- `length`：读取长度（默认 `64KiB`，上限 `4MiB`；超出自动夹紧）

响应：`200` + `application/x-ndjson` 原始字节，带 `X-Audit-Segment-Size` 与
`Content-Range: bytes <off>-<end>-1/<size>`（便于客户端续读）。归档未启用 ⇒ `404`。

## 计量报告（usage）

### GET /api/usage/report?owner=&from=&to=&format=

per-owner 周期用量（upload/download 字节）导出。数据源为日桶累计的 `usageStore`；
`usage.enabled=false`（未装配 store）→ `400` `usage 未启用`。

- 认证：主 mux `auth` / 隧道内层 `tunnel`；权限矩阵：
  - `owner` 非空：管理员可查任意 owner，普通主体只能查自己（否则 `403`）
  - `owner` 空（全部）：**仅管理员**可查（否则 `403`）
- query 参数：
  - `owner`（可选；空 = 全部，仅 admin）
  - `from` / `to`（可选，`YYYY-MM-DD`；缺省 `from=2000-01-01`、`to=今天`；非法或 `from>to` → `400`）
  - `format`：`json`（缺省）/ `csv`（其它值 → `400`）
- 响应（JSON）：
  ```json
  {"from": "2026-11-01", "to": "2026-11-03",
   "reports": [{"owner": "alice", "from": "2026-11-01", "to": "2026-11-03", "days": 3,
                "kinds": {"upload_bytes": 1048576, "download_bytes": 2048}}]}
  ```
  - `owner` 不存在/无数据 → `200` + `reports: []`（报告语义不 404）
- CSV 响应：`text/csv; charset=utf-8` + `Content-Disposition: attachment; filename="usage-report.csv"`
  （RFC 4180 转义；表头 `owner,from,to,days,kind,value`，每 kind 一行）
- 错误：`400` `usage 未启用` / from-to 非法 / format 非法；`403` 越权查询

## AI 洞察与配额（ai）

AI 端点默认关闭（`notify.ai_insight.enabled` / `notify.ai_quota.enabled` /
`notify.ai_privacy.enabled`），未启用时返回 400 并给出明确文案，不静默降级。

**认证差异（重要）**：

- `POST /api/ai/summarize` / `POST /api/ai/tag`：**仅隧道内层**（localMux `tunnel`），主 mux
  **未注册**——必须经 `POST /tunnel` 访问（owner 由隧道身份注入）。
- `GET /api/ai/quota` / `GET /api/ai/privacy` / `POST /api/ai/privacy/purge`：主 mux `public`
  裸注册（无 SproxySig；handler 内按请求身份**自查询/自删除**，看不到他人数据）+ 隧道内层 `tunnel`。

### POST /api/ai/summarize?filename=&volume=

读取文件首 4KiB 抽样后调用 LLM 生成摘要；结果按 `(owner, 卷:路径, mtime)` 缓存（mtime 未变即命中）。

- query 参数：`filename`（必填，经 `ValidateFilePath`）；`volume`（可选，外部/加密卷定位）
- 响应：`{"summary": "..."}`（最多 2000 个 rune，超出截断）
- 错误：`400` AI 洞察未启用 / filename 必填 / 路径非法 / **非文本文件**（抽样为空）；
  `404` 文件不存在；`429` AI 配额超限（`notify.ai_quota.enabled`，审计 `result=quota_exceeded`）；
  `502` AI 网关失败

### POST /api/ai/tag?filename=&volume=

同上（同一抽样/缓存/配额门），响应：`{"tags": ["..."]}`——按逗号/分号/换行切分并去重，
每个标签截断到 20 个 rune，最多 10 个。

### GET /api/ai/quota?owner=

返回 AI 调用配额用量（per-owner **进程内内存**计数：重启清零；按本地日跨天惰性重置）。

- query 参数：`owner`（可选）——仅在请求身份非空时生效；与身份不同且身份不是 `admin` → `403`
- 响应：
  ```json
  {"owner": "alice", "usage": {"day": "2026-11-03", "calls": 3, "tokens": 1200,
    "limit": {"DailyCalls": 100, "DailyTokens": 100000}}}
  ```
  （`limit` 内部字段无 JSON tag ⇒ 键名为 `DailyCalls` / `DailyTokens`；0 = 不限）
- 错误：`400` AI 配额未启用；`403` 无权查看他人配额

### GET /api/ai/privacy

列出当前 owner 的 AI 派生数据（向量/摘要/标签）可见性清单。owner 取请求身份（未认证归一为
`anonymous`）；AI 派生数据**只允许落 at-rest 加密卷**（fail-closed）。

- 响应：
  ```json
  {"enabled": true, "key_source": "volume-master-key",
   "artifacts": [{"kind": "vectors", "rel": "dir/a.txt", "bytes": 128, "ts": "2026-11-03T12:00:00Z"}]}
  ```
  `kind` ∈ `vectors` / `insight` / `tags`；`bytes` 为密文大小
- 错误：`400` AI 隐私未启用；`500` 存储不可用

### POST /api/ai/privacy/purge

删除当前 owner 的**全部** AI 派生数据（用户行使删除权），记审计 `ai.privacy_purge`。

- 响应：`{"deleted": N}`
- 错误：`400` AI 隐私未启用；`500` 存储不可用 / 清除失败

## 通知中心（notify）

需配置 `notify` 段（未启用时下列端点返回 400，纯文本 `notify 未启用`）。

### GET /api/notify/history?limit=N

返回通知历史（最近在前；中心内部为有界 ring，最多保留 200 条）。

- 认证：主 mux `auth`（SproxySig）/ 隧道内层 `tunnel`
- query 参数：`limit`（默认 100；仅接受正整数，非法/非正回落默认）
- 响应：`{"entries": [...]}`；条目字段**无 JSON tag**，键为 Go 字段名：
  `TS`（RFC3339）、`Action`、`Object`、`Channel`、`Status`（`sent`/`debounced`/`failed`/`skipped`）、`Detail`

### POST /api/notify/test?channel=

触发通知渠道自检（会真实外发一条 `[sproxy] 通知渠道自检` 消息），记审计 `notify_test`。

- 认证：同 `/history`
- query 参数：`channel`（可选；缺省 = 全部已注册渠道；不存在的渠道 → `404`）
- 响应：`{"results": {"<channel>": "ok"}}`；发送失败为 `"failed: <error>"`（逐渠道独立，不整体失败）

### GET /api/notify/feed?format=rss|atom&token=

通知历史 RSS/Atom 订阅源。

- 认证：主 mux `public` 裸注册（阅读器抓取不带业务凭据）/ 隧道内层 `tunnel`
- query 参数：`format`（默认 `rss`；仅 `rss`/`atom`，非法 → `400`）；`token`（见下）
- **token 门禁**：`notify.feed_token` 非空时必须携带 `?token=<t>` 或
  `Authorization: Bearer <t>`（常量时间比较），否则 `401` 空 body（不区分未提供/错误，防枚举）；
  空 = 公开（默认零回归）
- 条目上限：`notify.feed_max`（默认 20，上限 50）
- 响应：`200` + `Content-Type: application/rss+xml; charset=utf-8`（或
  `application/atom+xml; charset=utf-8`）；渲染失败 `500`

## 运行时配置（config）

### GET /api/config

返回当前**脱敏**运行时配置（认证：主 mux `auth` / 隧道内层 `tunnel`）。

```json
{
  "log_level": "info", "log_format": "text", "access_keys_set": true,
  "rate_limit_requests": 100, "rate_limit_window": "1m0s",
  "bandwidth_enabled": false, "bandwidth_per_owner_bps": 0,
  "max_storage_bytes": 0, "max_upload_bytes": 1073741824,
  "chunk_size": 4194304, "upload_session_ttl": "24h0m0s",
  "versioning_enabled": false, "versioning_max_versions": 0,
  "cloud_max_concurrent": 3, "cloud_transfer_concurrency": 3,
  "cloud_sync_threshold": 104857600, "hub_enabled": false,
  "tls_enabled": true, "addr": ":18083", "storage_root": "/var/lib/sproxy",
  "web_tunnel": true
}
```

`access_keys_set` = 凭据 Ring 是否非空；`cloud_transfer_concurrency` 是**实际生效值**
（配置 <1 时按并发上限归一）。不返回任何凭据/密钥。

### PUT /api/config

热更新运行时配置（认证：主 mux `auth` / 隧道内层 `tunnel`；请求体 ≤1 KiB）。只更新请求体中出现的
字段，未指定字段保持不变。

```json
{"log_level": "debug", "log_format": "json", "rate_limit_requests": 200,
 "rate_limit_window": "30s", "max_storage_bytes": 10737418240,
 "max_upload_bytes": 209715200, "web_tunnel": true}
```

- 校验：`log_level` ∈ debug/info/warn/error；`log_format` ∈ text/json；`rate_limit_requests`
  > 0；`rate_limit_window` 为合法 duration 且 > 0；`max_storage_bytes`/`max_upload_bytes` ≥ 0
- 空请求体 `{}`（无任何字段）→ `400`；任一字段非法 → `400`（`{"success": false, "message": "..."}`）
- 响应：`{"success": true, "changed": true}`
- 生效范围：日志级别/格式立即重建 logger；限流参数与 `max_storage_bytes` 热更新到现有实例；
  **限流中间件在启动时未挂载则 PUT 不能开启**（无 handler 链可挂，只更新配置值）
- 审计：`config_update`（成功 success；校验失败/空请求记 `denied` 并带原因）

## 集群节点（cluster）

需配置 `cluster.enabled`（未启用 → `400` `{"error": "集群未启用"}`）。

### GET /api/cluster/nodes

列出集群节点注册表。

- 认证：主 mux `public` 裸注册（无 SproxySig）/ 隧道内层 `tunnel`
- 响应：
  ```json
  {"nodes": {"node-1": {"role": "master", "status": "active",
    "joined_at": 1767225600000000000, "last_seen": 1767225600000000000}}}
  ```
  `status` ∈ `joining`/`active`/`draining`/`leaving`；`joined_at`/`last_seen` 为 Unix 纳秒
- 错误：`400` 集群未启用（注册表未装配）

### GET /api/cluster/self

返回本节点身份（来自 `cluster.node_id` / `cluster.role` 配置）。

- 认证：主 mux `public` / 隧道内层 `tunnel`
- 响应：`{"node_id": "node-1", "role": "master"}`
- 错误：`400` 集群未启用

## 一致性巡检与台账（verify / checksums）

### POST /api/verify

按 checksum 台账巡检文件一致性（读文件重算并与台账比对；不一致的文件被隔离）。

- 认证：主 mux `auth`（SproxySig）/ 隧道内层 `tunnel`
- 请求体（≤4096 字节，JSON）：`{"volume": "", "force": true}`
  - `volume` 非空 = 只核该卷视图；空 = 全部视图
  - `force` 保留给客户端语义扩展（当前实现不改变行为：巡检重叠时直接返回 `concurrent`）
  - owner 由请求身份派生；**owner 为空（管理员/未认证）⇒ 全租户巡检**
- 响应：`200` + `Report`
  ```json
  {"total": 10, "ok": 8, "mismatched": [{"path": "user/a.txt", "expected": "...", "actual": "..."}],
   "missing": ["user/b.txt"], "errors": ["..."], "skipped": 1, "concurrent": false}
  ```
  `ok` = 与台账一致的**已核对文件数**；`mismatched` = checksum 不符（已隔离）；`missing` = 台账有但文件缺失；
  `errors` = 读/隔离失败（与不符区分）；`skipped` = 台账为空/损坏被跳过的租户数；
  `concurrent=true` = 与上一次巡检重叠被跳过（提示重跑，不堆叠）
- 错误：`400` 请求体解析失败 / 请求体哈希校验失败
- 副作用：审计 `verify`（detail 汇总计数）；`mismatched+missing > 0` 时联动告警引擎
  （`source=checksum_mismatch`）

### POST /api/checksums/rebuild?owner=

从对象 meta sidecar 回填 checksum 台账（运维修复面：台账「存在但部分缺失」不会触发读自愈，需显式重建）。

- 认证：主 mux `auth` / 隧道内层 `tunnel`
- query 参数：`owner`（可选；缺省 = 请求身份，未认证归一为 `anonymous`）；指定**他人** owner 需
  `admin` 角色，否则 `403`
- 响应：`{"success": true, "owner": "alice", "entries": 42}`（`entries` = 回填条数）
- 错误：`403` 非 admin 指定他人 owner；`503` 台账存储不可用；`500` 其它重建失败
- 审计：`checksum_rebuild`（detail `entries=N`）

## 协议挂载面（WebDAV / S3）

以下两个前缀不是 JSON API，而是**协议端点**（供标准客户端直接读写）：

### GET /dav/

WebDAV 子树挂载（RFC 4918）：把请求者 owner 的 `user` 桶暴露为 WebDAV 端点，任意 WebDAV
客户端（curl / 文件管理器 / rsync / 编辑器）可直接读写。

- 启用：仅 `webdav.enabled: true` 时注册（默认关 = `404`，零回归）
- 认证：`authMiddleware`（SproxySig / APIKey；未认证 `401`）——**不是**无认证裸挂
- 选卷：`?volume=<卷名>`（经 ACL 校验，不可用 → `403`）；缺省走默认卷（同样校验默认卷 ACL）
- 方法：`OPTIONS` / `PROPFIND` / `GET` / `HEAD` / `PUT` / `DELETE` / `COPY` / `MOVE` / `MKCOL` /
  `LOCK` / `UNLOCK`（经 `golang.org/x/net/webdav`；不支持的方法 `405`）
- 路径语义：`/dav/<rel>` 即 owner `user/<rel>`；经 `storage.Root` 适配 ⇒ 加密卷透明解密/加密、
  封装卷封印生效（被封源目录直连读写恒拒 409 / 读视为 404）
- 错误：`400` 卷集未装配 / 卷不存在；`403` 卷不可用（ACL）；`500` user 桶预建失败；其它状态码由
  WebDAV handler 按 RFC 4918 返回

### GET /s3/

S3 兼容网关（SigV4 验签）：bucket = 已装配卷名（映射该卷 user 桶），未命中的首段视为普通 key
（默认卷）。mux 层 `public`（`/s3/` 无限 SproxySig），验签在 handler 内完成。

- 认证：`Authorization: AWS4-HMAC-SHA256 …`，AccessKey = sproxy 凭据 AK → 凭据 Ring 查 SK
  → HMAC 重算签名恒定时间比对；缺失 `Authorization` → `401`；验签失败 → `403`
- 支持的操作：
  - `GET /s3/`：ListBuckets（`ListAllMyBucketsResult` XML，枚举请求者 ACL 可见本地卷）
  - `HEAD /s3/`：桶存在性（`200`）
  - `GET /s3/?list-type=2[&prefix=]`：ListObjectsV2（`ListBucketResult` XML）
  - `GET` / `HEAD` / `PUT` / `DELETE /s3/<bucket>/<key>`：对象读/元信息/写/删
  - 分块上传：`POST ?uploads`（init）/ `PUT ?partNumber=&uploadId=`（part）/ `POST ?uploadId=`（complete）/
    `DELETE ?uploadId=`（abort）
- 语义：写路径经统一写内核（配额双账本 + 元数据/台账）；加密卷透明加解密，`HEAD`/`GET` 的 size 为
  **用户可见（明文）长度**；不支持 CreateBucket / DeleteBucket
- 错误：`400` key 为空 / 卷不可用；`403` 验签失败；`500` 列目录/读写失败

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

## 配额与等待（`GET /api/quota`）

### GET /api/quota

返回**认证用户自己**的跨卷配额视图（owner 恒取自请求认证，不接受请求参数指定 owner）。

```json
{
  "owner": "alice",
  "used": 1048576,
  "capacity": 1073741824,
  "unlimited": false,
  "partial": false,
  "volumes": [
    {"volume": "main", "kind": "local", "used": 1048576, "capacity": 1073741824, "has_quota": true}
  ],
  "wait": {
    "volumes": 1, "waiting": 0, "max_waiting": 3,
    "acquired": 42, "timed_out": 1, "canceled": 0,
    "queue_full": 0, "too_large": 2, "total_wait_ms": 1200
  }
}
```

字段语义（`used` / `capacity` 只是「Σ 有权威卷」的**下界**，`partial=true` 表示含未知部分）：

- `has_quota=false` ⇒ 该卷**无每用户权威** ⇒ `used`/`capacity` **未知**（禁当 0 消费）；
- `capacity=0` 且 `unlimited=true` ⇒ 该卷对该用户**不限**（不是「零配额」）；
- **`wait`（S4 等待可观测）**：跨**有配额池**的可访问卷汇总的等待队列统计 ——
  `volumes` 为参与卷数（0 ⇒ 该账户可访问卷未启用等待队列），`waiting` 当前等待者、
  `max_waiting` 历史峰值、`acquired`/`timed_out`/`canceled` 结果计数、
  `queue_full` 背压拒绝（对应 429）、`too_large` 超容量立即拒绝（不排队）、
  `total_wait_ms` 成功获取者累计等待。
  无配额池的卷**不计入**（不伪造「零等待」）。
