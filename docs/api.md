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
- **认证**：当 `auth_token` 配置非空时，除 `/healthz`、`/version`、`/ui/`、
  `POST /tunnel` 之外的所有路由都要求 `Authorization: Bearer <token>`。
- **隧道**：所有路由（除 `POST /tunnel` 自身）都可以通过 `POST /tunnel` 走 AES-256-GCM
  加密信道访问，sclient 默认就这么做。
- **Gzip 压缩**：服务端自动为 JSON 响应启用 gzip 压缩（当客户端请求头包含
  `Accept-Encoding: gzip` 时）。二进制下载流不做压缩。

## 基础

### GET /healthz

健康检查。返回 `OK`（text/plain）。无认证。

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
| 大小限制 | `max_upload_bytes`（默认 1 GiB，超过 413） |

响应（成功 200）：
```json
{"success": true, "message": "文件上传成功, size: 12345", "file_checksum": "abc..."}
```

幂等：如果目标已存在且 checksum 一致，返回 200；如果存在且 checksum 不一致，返回 409。

| 状态码 | 含义 |
|---|---|
| 200 | 上传成功或文件已存在 checksum 一致 |
| 400 | 文件名无效 / 缺少 X-File-Checksum / SHA-256 校验失败 |
| 401 | 未授权（auth_token 配置时） |
| 409 | 文件已存在但 checksum 不一致 |
| 413 | 请求体超过 max_upload_bytes |
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
| 410 | 上传已完成 |
| 413 | 单块超过 `max_chunk_upload_bytes` |

### GET /upload/status?upload_id=&filename=

查询上传状态。优先按 upload_id 查；如果 upload_id 找不到，可用 filename 查
（用于跨进程恢复客户端 ID 丢失场景）。

### POST /upload/complete

合并所有分块。请求体 JSON：`{"upload_id": "..."}`。

服务端会按序读取所有 chunk 文件、流式合并、再次计算完整文件 SHA-256 并与
`file_checksum` 对比。校验失败时**不保留**已合并文件。

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

## 隧道

### POST /tunnel

AES-256-GCM 加密的转发请求。请求体为帧协议：

```
[4B big-endian metaLen][encrypted metadata][stream chunks...]
```

`metaLen` 上限 1 MiB（`MaxMetadataBytes`），超过返回 400 并立即关闭。

详见 [tunnel.md](./tunnel.md)。

## 错误码附录

| HTTP | 业务原因（示例） |
|---|---|
| 400 | 路径无效、缺必填头、checksum 不匹配、chunk_checksum 不是 hex |
| 401 | auth_token 校验未通过 |
| 404 | 文件不存在、upload_id 不存在 |
| 409 | 文件 / 目录已存在 |
| 410 | 上传会话已完成 |
| 413 | 请求体超过限制 |
| 416 | Range 越界 |
| 500 | 服务端写文件、目录失败 |
