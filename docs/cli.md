<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sclient 命令行参考

sclient 是 sproxy 的配套客户端，基于 cobra + pflag。所有命令均支持
`--config`、`--server`、`--tunnel-key` 等全局参数。

## 全局选项

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--config` | XDG 路径 | 指定客户端配置文件路径 |
| `--server` | `https://127.0.0.1:18083` | sproxy 服务端地址（覆盖 server_url 配置） |
| `--tunnel-key` | (空) | 启用 tunnel 模式；64 位 hex AES-256 密钥 |
| `--no-checksum` | false | 跳过 SHA-256 校验（不推荐） |

## 子命令一览

| 命令 | 用途 |
|---|---|
| [`upload`](#upload) | 上传文件 |
| [`download`](#download) | 下载文件 |
| [`delete`](#delete) | 删除文件 |
| [`mv`](#mv) | 重命名 / 移动 |
| [`stat`](#stat) | 查询文件元信息 |
| [`list`](#list) | 列出文件 |
| [`mkdir`](#mkdir) | 创建目录 |
| [`rmdir`](#rmdir) | 删除目录 |
| [`search`](#search) | 搜索文件 |
| [`batch-delete`](#batch-delete) | 批量删除文件 |
| [`batch-rename`](#batch-rename) | 批量重命名文件 |
| [`cd`](#cd) | 切换当前目录 |
| [`pwd`](#pwd) | 打印当前目录 |
| [`tunnel`](#tunnel) | 通过隧道发送任意 HTTP 请求（`--xfer <name> --hub <addr>` 走 xfer/mux 隧道，启用身份指纹 pinning） |
| [`identity`](#identity) | 节点长时身份密钥管理（Ed25519，供对端指纹 pinning） |
| [`relay`](#relay) | 中继节点：连接到 Hub，转发请求到本地 HTTP 服务 |
| [`genkey`](#genkey) | 生成 64 hex 密钥（tunnel_key 已废除，仅历史用途） |
| [`config`](#config) | 配置管理 |
| [`version`](#version) | 打印版本信息 |

## 当前目录概念

sclient 维护一个**持久化的工作目录**（存于 XDG cache），影响所有以相对路径
传入的子命令。

- `sclient cd sub/dir` → 后续 `upload a.txt` 实际目标是 `sub/dir/a.txt`
- `sclient cd /` → 回到根目录
- `sclient cd ..` → 返回上级
- `sclient pwd` → 打印当前目录
- 使用 `/` 开头的**绝对路径**可以绕过当前目录（例如 `sclient upload /shared/file.txt`）
- 包含 `..` 的相对路径在**客户端**就被拒绝（与服务端 ValidateFilePath 对称），
  无需向服务端发送注定失败的请求

## 子命令详情

### upload

```bash
sclient upload <file1> [file2...]
sclient upload --chunked --concurrency 8 large.bin
```

- 自动判断是否启用分块上传（>100 MiB）
- 文件路径中的目录结构会被保留：`sclient upload dir/file.txt` → 服务端 `dir/file.txt`
- 支持 `--chunked` 强制开启分块、`--chunk-size`、`--concurrency`、`--resume`

### download

```bash
sclient download <filename> [output]
sclient download --chunked --concurrency 8 large.bin
```

- 默认走 `GET /download`（支持标准 Range header）
- `--chunked` 启用并发分块下载（走 `/download/chunk`）
- 不指定 output 时使用原文件名

### delete

```bash
sclient delete <filename>
```

每次仅接受一个参数。删除前会本地计算文件 SHA-256 用于服务端校验。

### mv

```bash
sclient mv <from> <to>
```

- 重命名或移动远端文件
- 先 `Stat(from)` 获取 checksum，再 `Rename(from, to, checksum)`
- 目标父目录不存在时服务端自动创建
- 目标已存在时返回错误

### stat

```bash
sclient stat <filename>
```

输出文件 size、checksum、mod_time。不下载内容。

### list

```bash
sclient list                # 列当前目录
sclient list --subdir dir1  # 列指定子目录
```

### mkdir

```bash
sclient mkdir <dirname>
```

创建子目录（递归），类似 `mkdir -p`。

### rmdir

```bash
sclient rmdir <dirname>
sclient rmdir --force <dirname>
```

非空目录在没有 `--force` 时会有交互式确认提示。

### cd / pwd

见上文"当前目录概念"。

### sync

```bash
sclient sync push --remote <name> [--src <path>] [--dst <path>] [--recursive] [--wait]
sclient sync pull --remote <name> [--src <path>] [--dst <path>] [--recursive] [--wait]
```

- 在本地 sproxy 服务端创建节点间文件同步任务（push 本地→远程 / pull 远程→本地），
  由服务端 SyncManager 托管执行；`--remote` 是服务端 `sync_remotes` 配置的远程节点名
- `--src`/`--dst` 均为服务端 uploadsDir 相对路径（默认 `""` = 整个根）；`--recursive` 递归子目录
- `--conflict skip|overwrite|lww|conflict-rename` 冲突策略；`--sync-empty-dirs`/`--follow-symlinks` 可选
- `--wait` 阻塞等待任务终态并展示进度（`--timeout` 超时，0=不限）
- **自动重试**：同步遇瞬时网络错误（连接拒绝/超时/5xx）由 SyncManager 指数退避自动重试
  （`sync.max_retries` 次内），任务状态在重试期间显示 `retrying`；达上限转 `failed` 且错误
  信息含"已重试 N 次"；`retries` 字段为已重试次数（持久化，重启后续计）。取消/删除在
  重试等待期间仍立即生效

### tunnel

```bash
sclient tunnel <url>
sclient tunnel -X POST -H "Content-Type: application/json" -d '{"k":"v"}' <url>
# xfer/mux 隧道模式（启用身份指纹 pinning，见下）
sclient tunnel --xfer tcp --hub 127.0.0.1:18090 <url>
# xfer TLS 传输（tcp+tls）：连 sproxy 服务端 xfer_tls listener（auto_tls 自签证书时用 --ca-file 信任）
sclient tunnel --xfer tcp+tls --hub 127.0.0.1:18087 --ca-file ./sproxy-cert.pem <url>
```

通过加密隧道发送任意 HTTP 请求。可用于调试或转发到其他服务。

**xfer/mux 隧道模式（P1 身份 pinning）**：加 `--xfer <name> --hub <addr>` 走
xfer/mux 隧道（如 `tcp`、`tcp+tls`、`ws`）。该模式在 ECDH 握手时交换 Ed25519 长时身份并做
对端指纹 pin 校验：

- 本端身份：`sclient identity generate` 生成（XDG 目录 `sproxy/identity.json`）；
- 对端 pin：配置 `peer_fingerprints`（`sclient config set peer_fingerprints <fp>`，
  多个逗号分隔；对端指纹取 `sclient identity fingerprint` 带外固化）；
- 需配置 `access_key`/`access_key_secret`（派生隧道密钥使握手执行；缺 key 但配置了
  身份/指纹时 fail-closed 报错，不静默降级）；
- fail-closed：pin 不匹配或对端无身份即拒绝；传统隧道（未加 `--xfer`）与文件直连
  命令不做身份交换，配置 `peer_fingerprints` 会 fail-closed 报错并指引使用 `--xfer`。

**`tcp+tls` 传输的客户端 TLS 配置（阶段5 工作项1）**：`tcp+tls` 对服务端证书做标准
TLS 校验，按 `--ca-file` / `--insecure`（或配置 `xfer_ca_file` / `xfer_insecure`）装配：

- `--ca-file <pem>`：信任该 PEM 文件中的 CA（自签服务器证书时把服务端证书或签发它的
  CA 放入此文件），严格校验、不跳过；
- `--insecure`：跳过证书校验，但**仅限 loopback hub**（远程 hub + `--insecure`
  fail-closed 拒绝，需改用 `--ca-file`）；与 `--ca-file` 互斥；
- 两者均未指定：用系统根证书池严格校验——服务端为自签证书（auto_tls）时握手报
  `x509: certificate signed by unknown authority`，此时应显式 `--ca-file` 或
  `--insecure`（fail-closed，不静默降级）。

> **远程 hub 的 SAN 限制（审查 M-4）**：sproxy 默认 auto_tls 自签证书 SAN 仅覆盖
> `localhost` / `sproxy.local` / `127.0.0.1` / `::1`。即使配了 `--ca-file`，**远程
> hub**（非 loopback 主机名/IP）也会因证书 SAN 不匹配握手失败（`x509: certificate
> is valid for ... not <remote>`）。远程部署需服务端显式配置 `tls.cert_file` 提供
> 含该主机名/IP SAN 的证书（见 `config.example.yaml`），仅信任 CA 不够。

> **接线现状**：`--xfer tcp+tls` 已对接真实 sproxy 服务端的 **xfer_tls listener**
> （配置 `hub.transports.xfer_tls`，默认 `127.0.0.1:18087`；auto_tls 自签证书时客户端
> 需 `--ca-file` 或 `--insecure`）。`--xfer tcp` / `ws` 仍对接 xfer/mux listener
> （测试或自定义服务端，如 `sclient relay`/`mesh node` 建立的自定义隧道对端）；真实
> sproxy hub/relay/mesh 节点的数据面协议仍以各自传输为准，示例中 `127.0.0.1:18090`
> 仅为示意。

### identity

```bash
sclient identity generate [--file <path>] [--force]   # 生成并持久化节点身份密钥，打印指纹
sclient identity show [--file <path>]                 # 展示本节点身份指纹与公钥
sclient identity fingerprint [--file <path>]          # 仅打印指纹（供脚本/复制）
```

管理节点长时身份（Ed25519，P1 身份 pinning）。身份文件默认存 XDG 配置目录
`sproxy/identity.json`（`--file` 覆盖）。`generate` 已存在时报错（`--force` 覆盖）；
`show`/`fingerprint` 在文件缺失/损坏时返回非 0 退出码并提示恢复路径。

> 注意：`--file` 为独立管理用途——`tunnel --xfer` 的 pinning 恒从默认 XDG 路径
> （`sproxy/identity.json`）加载本端身份，自定义路径生成的身份仅供展示/备份，
> 不会参与 xfer 隧道的身份交换。如需自定义身份路径参与隧道，请改用默认路径。

### relay

```bash
# 作为中继节点连接到 Hub
sclient relay --hub ws://hub.example.com/ws --local http://127.0.0.1:8080 --node-id my-node
```

作为中继节点连接到 Hub，注册自身节点标识，然后等待远程请求并通过隧道转发到本地
HTTP 服务。适用于跨网络服务暴露、集群间请求转发等场景。

**参数：**

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--hub` | `ws://127.0.0.1:18084/ws` | Hub 的 WebSocket 地址 |
| `--local` | `http://127.0.0.1:8080` | 本地 HTTP 服务地址 |
| `--node-id` | 时间戳 | 节点唯一标识 |

**工作原理：**

1. 通过 WebSocket 传输层连接到 Hub
2. 创建 mux 多路复用连接（Listener 角色）
3. 在控制流上发送 NodeID 完成注册
4. 使用 `Tunnel.Serve` 接受中继请求
5. 每个请求通过 `http.Client` 转发到本地 `--local` 地址
6. 响应通过隧道原路返回

### TURN 中继（mesh / p2p / socks / udp / mesh node）

webrtc 打洞直连在对称 NAT 下需要 TURN 中继。以下命令均支持「静态 TURN 凭据」与
「动态 TURN REST 短期凭据」两种配置（互不排斥，REST 优先）：

- `sclient mesh connect <service>` — 连接 mesh 服务（webrtc 直连优先，hub 中继回落）
- `sclient p2p connect --peer <id> --tcp <addr>` — WebRTC 打洞直连对端
- `sclient socks -l :port --exit <node>` — SOCKS 代理出口
- `sclient udp map -l :udp --exit <node> --remote <host:port>` — UDP 端口映射
- `sclient mesh node ...` — 常驻 mesh 节点（自动对等发现 + 本地网关）

**静态 TURN（`--turn` 族）**

| 参数 | 说明 |
|------|------|
| `--turn <url>...` | TURN 中继服务器地址（可重复/逗号分隔，如 `turn:relay.example.com:3478`） |
| `--turn-user <user>` | TURN 用户名（静态密码模式，配 `--turn`/`--turn-pass` 使用） |
| `--turn-pass <pass>` | TURN 密码（静态密码模式，配 `--turn`/`--turn-user` 使用） |

**动态 TURN REST（`--turn-rest` 族，coturn 标准短期凭证）**

| 参数 | 说明 |
|------|------|
| `--turn-rest <url>` | TURN REST API 短期凭证端点（如 `https://turn.example.com/turn`）。REST 优先于静态 `--turn-user`/`--turn-pass` |
| `--turn-rest-user <user>` | REST API 认证用户名（透传给服务端）。**必填**：配 `--turn-rest` 时缺省会命令终止 |
| `--turn-rest-service <svc>` | 可选 `service` 参数（透传给服务端，如区分 realm/service） |

- 协议：首次建立 webrtc 连接前惰性拉取 `GET {url}?username=<user>[&service=<svc>]`，
  响应 `{username: "<ttl>:<user>", password: "<base64(HMAC-SHA1)>", ttl: <秒>}` 透传为
  ICE TURN 凭据；缓存至 TTL 到期前续期（单飞，并发首次只拉一次）。
- 安全（fail-closed）：端点默认强推 `https://`；明文 `http://` 仅限 loopback（本机调试），
  非 loopback 的 http 拒绝（否则凭据与 TURN 流量可被中间人读取）。URL / username /
  service 长度上限 512 字符；响应 username 必须符合 coturn `TTL:user` 格式。非法配置
  命令终止，不静默忽略。
- 失败降级：REST 拉取失败时沿用仍有效的旧缓存；无有效缓存则回落静态凭据/仅 STUN，
  日志告警但不 panic（回落 hub 中继不受影响）。
- 未配置 `--turn-rest` 时相关命令行为不变（no-op）。

### genkey

```bash
sclient genkey
```

打印新的 64 位 hex AES-256 密钥（不写入配置文件）。

### config

```bash
sclient config                       # show（同 show）
sclient config show
sclient config set server_url http://proxy:18083
sclient config set access_key_secret <64hex>   # SproxySig Secret（本地凭据）
sclient config set access_key_id <sk-...>      # 多 SK 时可选（SK 条目 ID；trust renew 回填）
```

### version

```bash
sclient version
```

打印 sclient 版本、配置文件路径、生效的 server / AccessKey 摘要（Secret 全掩）。

### search

```bash
sclient search <keyword>
```

- 递归搜索文件名中包含 `<keyword>` 的文件（不区分大小写）
- 关键字为空时返回空列表
- 输出格式与 `list` 相同，包含 name、size、checksum、mod_time、is_dir

### batch-delete

```bash
sclient batch-delete <file1> [file2...]
```

- 批量删除多个文件，一次调用减少网络往返
- 每个文件先本地计算 SHA-256 再发送删除请求
- continue-on-error：部分文件删除失败不影响其他文件
- 输出每个文件的操作结果（成功/失败及原因）

### batch-rename

```bash
sclient batch-rename <from1> <to1> [from2 to2...]
```

- 批量重命名/移动多组文件
- 参数必须成对出现：源路径和目标路径交替排列
- 每组操作前自动获取源文件 checksum
- continue-on-error：部分操作失败不影响后续
- 输出每个操作的结果（成功/失败及原因）

## 常见错误排查

| 现象 | 可能原因 |
|---|---|
| `路径包含父级引用 '..'` | 客户端预拦截了不安全路径，去掉 `..` 或用绝对路径 |
| `tunnel error (HTTP 401)` | 外层 `POST /tunnel` SproxySig 验签失败（凭据缺失/非法/过期/重放），检查 `access_key`/`access_key_secret` 是否与服务端凭据 Ring 一致 |
| `tunnel error (HTTP 400)` | 隧道密钥与服务端不一致，或网络中间层破坏了请求体 |
| `unauthorized` (401) | SproxySig 签名缺失/非法/过期（服务端凭据 Ring 非空时），检查 `~/.config/sproxy/sclient.yaml` 的 `access_key`/`access_key_secret` 是否与服务端一致 |
| `源文件 SHA-256 校验失败` | mv 期间本地文件已变，刷新本地 checksum 后重试 |
| `文件已存在但 checksum 不匹配` (409) | 服务端已有同名文件且内容不同，先 mv 或 delete |
