<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 配置参考

sproxy 的运行参数由 4 个来源合并而成，**优先级从高到低**：

1. CLI 旗标（`--addr`、`--uploads-dir`、`--tunnel-key`）
2. 环境变量（前缀 `SPROXY_`，例如 `SPROXY_ADDR=":18083"`）
3. 配置文件 YAML（`--config sproxy.yaml`，默认 `sproxy.yaml`）
4. Default()（`pkg/server/config.go`）

配置文件不存在时不报错，仅使用环境变量与默认值。

## 服务端配置（`sproxy.yaml`）

完整字段一览：

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `addr` | string | `:18083` | HTTP 监听地址（`host:port` 或 `:port`） |
| `uploads_dir` | string | `./uploads` | 文件存储根目录，自动创建 |
| `max_upload_bytes` | int64 | `1073741824` (1 GiB) | 单次普通上传最大字节，超过 413。0 = 不限制 |
| `tunnel_key` | string | (空) | 64 位 hex AES-256 密钥。留空时启动自动生成并回写到 YAML |
| `auth_token` | string | (空) | Bearer 认证 token；留空 = 不认证。除 `/healthz`、`/version`、`/ui/`、`POST /tunnel` 之外全路由生效 |
| `log_level` | string | `info` | `debug` / `info` / `warn` / `error` |
| `log_format` | string | `text` | `text`（默认）或 `json` |
| `max_header_bytes` | int | `1048576` (1 MiB) | HTTP 请求头大小上限 |
| **server_timeouts** | object |  | http.Server 各阶段超时 |
| `server_timeouts.read_header` | duration | `0` | ReadHeader 超时（`"5s"` 风格） |
| `server_timeouts.read` | duration | `0` | 整个请求读取超时 |
| `server_timeouts.write` | duration | `0` | 响应写出超时 |
| `server_timeouts.idle` | duration | `0` | keep-alive 空闲超时 |
| `server_timeouts.shutdown` | duration | `30s` | graceful shutdown 等待活跃请求结束的最长时间 |
| **tls** | object |  | TLS 配置 |
| `tls.enabled` | bool | `false` | 启用 TLS |
| `tls.cert_file` | string | (空) | 证书路径（启用 TLS 时生效） |
| `tls.key_file` | string | (空) | 私钥路径 |
| `tls.auto_tls` | bool | `false` | `true` 时证书/私钥缺失自动生成 ECDSA P-256 自签证书 |
| **rate_limit** | object |  | 速率限制（仅限制 `POST /tunnel` 入口） |
| `rate_limit.enabled` | bool | `false` | 启用 |
| `rate_limit.requests` | int | `10` | 窗口内允许请求数 |
| `rate_limit.window` | duration | `1s` | 滑动窗口大小 |
| **分块上传** |  |  |  |
| `chunk_size` | int64 | `4194304` (4 MiB) | 服务端推荐分块大小 |
| `max_chunk_size` | int64 | `0` | 仅客户端配置，服务端忽略 |
| `max_chunk_upload_bytes` | int64 | `8388608` (8 MiB) | 单块请求体最大限制 |
| `upload_session_ttl` | duration | `24h` | 未完成会话保留时间 |

### Gzip 压缩

服务端自动为 JSON 响应启用 gzip 压缩（当客户端 `Accept-Encoding` 包含 `gzip` 时），
无需额外配置。二进制文件下载流不做压缩。

### 时长字段格式

所有 `*_timeouts.*` 与 `*_ttl` / `window` 字段都使用 Go duration 字符串：
`"5s"`、`"30s"`、`"5m"`、`"24h"` 等。

### tunnel_key 自动生成

如果 `tunnel_key` 在配置加载后仍为空，sproxy 启动时会：

1. 调用 `tunnel.GenerateKey()` 生成新密钥
2. 通过 `server.SaveConfig` 把完整配置写回 `--config` 指定的 YAML 文件
3. 在控制台打印 `Generated tunnel key: <hex>`，请妥善保管

如果不希望 sproxy 写入配置文件，请在启动前提供 `tunnel_key`（环境变量或 YAML）。

## SIGHUP 热重载

对运行中的 sproxy 发送 SIGHUP，会触发部分配置热重载。**仅以下字段在 SIGHUP 后生效**：

- `log_level`
- `log_format`
- `auth_token`

其他字段（`addr`、`uploads_dir`、`tunnel_key`、`rate_limit`、`server_timeouts`、
`max_header_bytes`）需要**重启进程**。SIGHUP 时会打印警告说明哪些字段未生效。

## 客户端配置（sclient）

sclient 的配置默认路径基于 XDG：

| 平台 | 路径 |
|---|---|
| Linux | `~/.config/sproxy/sclient.yaml` |
| macOS | `~/Library/Application Support/sproxy/sclient.yaml` |
| Windows | `%LOCALAPPDATA%/sproxy/sclient.yaml` |

旧路径 `~/.sclient.yaml` 仍会被读取并提示迁移。`--config` flag 可覆盖默认路径。

环境变量前缀 `SCLIENT_`（例如 `SCLIENT_SERVER_URL=http://proxy:18083`）。

完整字段：

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `server_url` | string | `http://localhost:18083` | sproxy 服务端地址 |
| `check_checksum` | bool | `true` | 上传/下载启用 SHA-256 校验 |
| `timeout` | int | `300` | HTTP 客户端超时（秒） |
| `tunnel_key` | string | (空) | 64 位 hex；非空时通过 `POST /tunnel` 加密信道访问 |
| `chunk_size` | int64 | `4194304` (4 MiB) | 默认分块大小 |
| `max_chunk_size` | int64 | `0` | 自适应分块上限；0 = fallback 到 64 MiB |
| `auth_token` | string | (空) | Bearer 认证 token（如果服务端启用） |
| `xfer_name` | string | (空) | 传输层名称（如 `"ws"`），空串 = 使用传统 HTTP POST |
| `hub_url` | string | (空) | Hub 中继地址（启用 xfer 时必需），如 `ws://hub.example.com/ws` |

### Hub 中继配置（服务端）

服务端 `sproxy.yaml` 支持以下 hub 配置段：

```yaml
hub:
  enabled: true                      # 启用 Hub 中继模式（默认关闭）
  node_id: "sproxy-node-1"           # 节点标识，空串自动生成
  transports:
    ws:
      enabled: true                  # 启用 WebSocket 传输监听
      listen: ":18084"               # WebSocket 监听地址
      path: "/ws"                    # WebSocket 升级路径
```

### 当前目录（cd / pwd）

sclient 支持工作目录概念，持久化到 XDG cache（`~/.cache/sproxy/current_dir`）。
详见 [cli.md](./cli.md)。

## 示例

### 服务端

```yaml
# sproxy.yaml
addr: ":18083"
uploads_dir: "/var/lib/sproxy/uploads"
max_upload_bytes: 5368709120     # 5 GiB
tunnel_key: "<64 位 hex>"
auth_token: "your-secret-token"

server_timeouts:
  read_header: "5s"
  read: "30s"
  write: "30s"
  idle: "60s"
  shutdown: "30s"

rate_limit:
  enabled: true
  requests: 100
  window: "1s"
```

### 客户端

```yaml
# ~/.config/sproxy/sclient.yaml
server_url: "https://proxy.example.com"
tunnel_key: "<与服务端相同的 64 位 hex>"
auth_token: "your-secret-token"
check_checksum: true
chunk_size: 8388608    # 8 MiB
```
