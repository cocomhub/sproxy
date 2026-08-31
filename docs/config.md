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
| `access_keys` | []{key, secret, mesh_id?} | (空) | SproxySig 请求签名认证（每 mesh 一对 AK/SK）。配置后除 `/healthz`、`/version`、`/ui/`、`POST /tunnel` 之外全路由验签；留空 = 不认证 |
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
| `tls.enabled` | bool | `true` | 启用 TLS |
| `tls.cert_file` | string | (空) | 证书路径（启用 TLS 时生效） |
| `tls.key_file` | string | (空) | 私钥路径 |
| `tls.auto_tls` | bool | `true` | `true` 时证书/私钥缺失自动生成 ECDSA P-256 自签证书 |
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

其他字段（`addr`、`uploads_dir`、`tunnel_key`、`rate_limit`、`server_timeouts`、
`max_header_bytes`、`access_keys`）需要**重启进程**。SIGHUP 时会打印警告说明哪些字段未生效。

## 客户端配置（sclient）

sclient 的配置默认路径基于 XDG：

| 平台 | 路径 |
|---|---|
| Linux | `~/.config/sproxy/sclient.yaml` |
| macOS | `~/Library/Application Support/sproxy/sclient.yaml` |
| Windows | `%LOCALAPPDATA%/sproxy/sclient.yaml` |

旧路径 `~/.sclient.yaml` 仍会被读取并提示迁移。`--config` flag 可覆盖默认路径。

环境变量前缀 `SCLIENT_`（例如 `SCLIENT_SERVER_URL=http://proxy:18083`）。

**多环境**：`SCLIENT_ENV` 环境变量选择 env 后缀配置文件（如 `SCLIENT_ENV=prod` →
`~/.config/sproxy/sclient.prod.yaml`）。为空用默认 `sclient.yaml`，便于同一台机器维护
prod/staging/dev 多套 hub/server/token 配置。通用参数优先级：**CLI flag > 环境变量 >
配置文件 > 默认值**。

完整字段：

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `server_url` | string | `https://127.0.0.1:18083` | sproxy 服务端地址 |
| `timeout` | int | `300` | HTTP 客户端超时（秒） |
| `tunnel_key` | string | (空) | 64 位 hex；非空时通过 `POST /tunnel` 加密信道访问 |
| `chunk_size` | int64 | `4194304` (4 MiB) | 默认分块大小 |
| `max_chunk_size` | int64 | `0` | 自适应分块上限；0 = fallback 到 64 MiB |
| `access_key` | string | (空) | SproxySig 认证 AccessKey（服务端配置了 `access_keys` 时需要） |
| `access_key_secret` | string | (空) | SproxySig 认证 AccessKeySecret（本地密钥，仅计算签名，永不上线） |
| `allow_transport_fallback` | bool | `false` | 允许隧道/xfer 初始化失败时回退直连 |
| `hub_url` | string | (空) | mesh/relay/p2p 共用的 hub 地址（http(s) 或 ws(s)，可带 /ws 路径）。为空时各命令按自身语义回落（mesh→server_url，p2p→报错，relay→本地默认） |
| `relay_token` | string | (空) | hub 中继注册 token（与 relay start --token / hub.relay_token 一致） |
| `node_id` | string | (空) | 本节点默认 ID（mesh/p2p/relay 信令来源与寻址目标；为空回落主机名） |
| `peer_fingerprints` | []string | (空) | 对端身份指纹 pinning 列表（64 hex 或 `sha256:<64 hex>`，逗号分隔）。仅 `sclient tunnel --xfer <name>`（xfer/mux 隧道）消费：握手时 fail-closed 校验对端 Ed25519 身份指纹，防 MITM。对端指纹取 `sclient identity fingerprint` 带外固化；本端身份由 `sclient identity generate` 生成（XDG 目录 `sproxy/identity.json`）。需配置 `access_key`/`access_key_secret`（派生隧道密钥使握手执行；缺 key 但配置了身份/指纹时 fail-closed 报错）。传统隧道/文件直连命令不走 xfer 握手，配置此项会 fail-closed 报错（指引使用 `--xfer`），不静默跳过 |
| `xfer_ca_file` | string | (空) | xfer `tcp+tls` 传输的受信 CA 文件路径（PEM；等价 `--ca-file`）。为空时用系统根池严格校验——服务端为自签证书（auto_tls）时握手报 `x509: certificate signed by unknown authority`，需配置此项或 `xfer_insecure` |
| `xfer_insecure` | bool | `false` | 跳过 xfer `tcp+tls` 传输的证书校验（等价 `--insecure`）。**仅限 loopback hub**（远程 hub + insecure fail-closed 拒绝，需改用 `xfer_ca_file`）；与 `xfer_ca_file` 互斥 |

> **TURN 中继配置（CLI flag，非配置键）**：TURN 凭据只在 sclient 发起 webrtc 打洞时
> 本地使用，**服务端无 TURN 配置**。动态 TURN REST 短期凭证（coturn 标准）通过 CLI
> flag 传入：`--turn-rest <url> [--turn-rest-user <user>] [--turn-rest-service <svc>]`，
> 支持命令为 `mesh connect` / `p2p connect` / `socks` / `udp map` / `mesh node`。REST 优先于
> 静态 `--turn`/`--turn-user`/`--turn-pass`；失败降级仅 STUN（不 panic）。安全边界：
> 端点默认强推 `https://`，明文 `http://` 仅限 loopback；URL/username/service 上限 512
> 字符。详见 [cli.md](./cli.md) 的「TURN 中继」段落。

### Hub 中继配置（服务端）

服务端 `sproxy.yaml` 支持以下 hub 配置段：

```yaml
hub:
  enabled: true                      # 启用 Hub 中继模式（默认关闭）
  node_id: "sproxy-node-1"           # 节点标识，空串自动生成
  dht: ""                            # 节点发现表：""= 内置内存 DHT（默认）；"kad" = Kademlia
  dht_persist_file: ""               # kad k-bucket 落盘路径（仅 dht: kad 时消费；空 = 关闭）
  federation:
    enabled: true                    # 启用 hub 联邦（hub-to-hub peering）
    persist_file: ""                 # 联邦候选节点表持久化路径（空 = 关闭）
  transports:
    ws:
      enabled: true                  # 启用 WebSocket 传输监听
      listen: ":18084"               # WebSocket 监听地址
      path: "/ws"                    # WebSocket 升级路径
```

- `dht_persist_file`：非空且 `dht: kad` 时启用 k-bucket 落盘，重启后恢复上次发现缓存
  （不冷启动）。快照只存 id/route_id/addr（发现缓存无 secret）；损坏/缺失/超限文件按
  空桶启动。路由表仍 hub 权威，DHT 持久化是**缓存语义**。缺省关闭（零行为变更）。
- `federation.persist_file`：非空时把联邦候选节点表持久化，重启后恢复上次同步的候选。
  快照只存 id/addr/mesh（发现缓存无 secret）；损坏/缺失文件按空候选启动。缺省关闭
  （零行为变更）。文件均 0600 权限 + temp/rename 原子写。

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
access_keys:
  - key: "sk-meshA-3f8a..."
    secret: "0123...（64 hex）"

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
access_key: "sk-meshA-3f8a..."
access_key_secret: "0123...（64 hex，与服务端 access_keys 中 secret 一致）"
check_checksum: true
chunk_size: 8388608    # 8 MiB
```
