<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 配置参考

sproxy 的运行参数由 4 个来源合并而成，**优先级从高到低**：

1. CLI 旗标（`--addr`、`--storage-root`）
2. 环境变量（前缀 `SPROXY_`，例如 `SPROXY_ADDR=":18083"`）
3. 配置文件 YAML（`--config sproxy.yaml`，默认 `sproxy.yaml`）
4. Default()（`pkg/server/config.go`）

配置文件不存在时不报错，仅使用环境变量与默认值。

## 服务端配置（`sproxy.yaml`）

完整字段一览：

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `addr` | string | `:18083` | HTTP 监听地址（`host:port` 或 `:port`） |
| `storage_root` | string | `./storage` | 多租户存储根目录，自动创建 |
| `owner_quotas` | map[string]ByteSize | (空) | 按 owner 配额上限：显式 owner > `"*"` 默认 > 0（不限制）。值支持人类可读大小（`"5GiB"`/`"2GB"`）或纯数字字节 |
| ~~`max_upload_bytes`~~ | — | 1 GiB（硬编码） | **已移除的配置项**：普通上传请求体上限固定为 `internal/size.UploadBodyLimit`（1 GiB），超过 413；该键已不再被读取 |
| `registration` | {disable: bool} | `disable: false` | 注册开关：`false`=允许注册（默认）；`true`=禁止注册（仅存量用户，无法新增） |
| `registration.force_totp` | bool | `false` | `true` 时 register 走 TOTP 分支——注册不生成 SK 条目，用户须经 `sclient trust login` 录入 GA 密钥后登录拿短命 session SK（DEC-B） |
| `registration.session_ttl` | duration | `24h` | TOTP 登录（`login_type=web`，缺省）签发的 session SK 有效期（D3 服务端控） |
| `registration.cli_ttl` | duration | `7d` | TOTP 登录（`login_type=cli`，`sclient trust login` 回填场景）的 session SK 有效期（D3） |
| `registration.login_fail_limit` | int | `5` | per-AK TOTP 登录失败锁定阈值（U4）：连续失败达此数即进入锁定窗口 |
| `registration.login_fail_window` | duration | `15m` | per-AK 登录失败锁定期（U4）：达阈值后锁定该时长，到期自动解锁 |
| `allow_insecure_loopback` | bool | `false` | 无任何凭据时（ring 空）放行 loopback 来源的 GET/HEAD（仅本地调试；生产勿开） |
| `credential_ttl` | duration | `720h` (30d) | 新建 SK 条目有效期（renew 新 SK 用，服务端控 TTL；默认 30d） |
| `credentials.rotation.interval` | duration | `0`（关闭） | 凭据定期自动轮换调度周期：`> 0` 时启用（如 `24h`），`0`/缺省 = 关闭（零回归）。变更需重启进程生效（与 SIGHUP 硬配置同语义） |
| `credentials.rotation.notify_before` | duration | `168h` (7d) | 到期前提前轮换的提前量：SK 到期时间 ≤ now+此值即触发 renew（避免到期当天才换的断链窗口） |
| `credentials.rotation.keep_old` | int | `2` | 轮换后保留的旧 SK 数：>1 时新 SK 生效后旧 SK 在宽限期内仍可用（客户端配置回填前不断签）；超出部分由调度器裁剪删除 |
| `credential_store.encrypt` | bool | `false` | 凭据静态存储加密：`true` = `<tenant>/meta/credentials.json` 以密文落盘（EncryptingStorer 装配）；`false`/缺省 = 明文 JSON（零回归） |
| `credential_store.backend` | string | `aesgcm` | 加密后端枚举：`aesgcm`（缺省，本地 master key AES-256-GCM）或 `vault`（HashiCorp Vault Transit，密钥永不出 Vault）。非法值启动校验拒绝 |
| `credential_store.master_key_file` | string | (空) | aesgcm 专用 master key 文件路径（base64 32B 或 raw 32B）；为空时回落环境变量 `SPROXY_CREDENTIAL_MASTER_KEY`（base64 32B）。`encrypt=true` + backend=aesgcm 且两者皆无时启动失败。生成：`openssl rand -base64 32` |
| `credential_store.vault.addr` | string | (空) | backend=vault 时 Vault 服务地址（http/https，必须）。`encrypt=true` + backend=vault 时缺失启动失败 |
| `credential_store.vault.mount` | string | `transit` | transit engine 挂载路径 |
| `credential_store.vault.key_name` | string | (空) | backend=vault 时 transit 加密 key 名（必须）。`encrypt=true` + backend=vault 时缺失启动失败。**key 需以 `derived=true` 创建**（AAD context 绑定文件身份才生效；非 derived key 忽略 context，见装配冒烟注释） |
| `credential_store.vault.token_file` | string | (空) | Vault token 文件路径（读取后 trim）；为空时回落 `token_env` 环境变量 |
| `credential_store.vault.token_env` | string | `VAULT_TOKEN` | Vault token 环境变量名。`encrypt=true` + backend=vault 且 token_file 与环境变量皆无时启动失败 |
| `credential_store.vault.ca_file` | string | (空) | Vault 自签 CA PEM 路径（可选，默认系统证书池） |
| `credential_store.vault.timeout` | duration | `10s` | Vault HTTP 超时 |
| `credential_store.vault.cache_ttl` | duration | `30s` | decrypt 结果缓存 TTL。设 `0` 无效回落 `30s`（viper 零值歧义——config 层缓存恒默认开，不可显式关；`VaultOptions.CacheTTL` 内部 API 可传 0 关闭，供测试） |
| `log_level` | string | `info` | `debug` / `info` / `warn` / `error`（`debug` 同时打开进程内 telemetry span 调试行——默认 Info 级下 span 静默，见 `docs/architecture.md`「客户端追踪」） |
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
| `rate_limit.coordinated` | bool | `false` | 多实例协调（共享配额）。开启后按 `backend` 装配共享计数，多个 sproxy 实例共享同一限额，防分散绕过 |
| `rate_limit.backend` | string | `local` | 协调后端：`local`（每实例独立计数，默认）/ `file`（storage 根下 `ratelimit/` 目录原子计数文件，多实例共享；跨进程协调语义在 Linux 上验证，Windows 降级为尽力而为） |
| **分块上传** |  |  |  |
| `chunk_size` | int64 | `4194304` (4 MiB) | 服务端推荐分块大小 |
| `max_chunk_size` | int64 | `0` | 仅客户端配置，服务端忽略 |
| `max_chunk_upload_bytes` | int64 | `8388608` (8 MiB) | 单块请求体最大限制 |
| `upload_session_ttl` | duration | `24h` | 未完成会话保留时间 |

### 多卷存储（volumes / placement）

可选。配置 `volumes` 后启用多卷存储；**不配置 = 单卷零回归**（`storage_root` 单根，
`volumes[0]` 自动合成为默认卷，卷名 `default`）。多卷模式文件寻址 = `(卷, owner, 相对路径)`，
读路径跨卷定位、写路径自动路由、owner 全局配额跨卷合计。

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `placement` | string | `prefer-default` | 自动路由策略：`prefer-default`（默认卷优先，容量满则换下一候选）或 `spread`（卷用量最少优先）。显式 `--volume` / `volume` 参数时忽略 |
| `volumes` | []object | (空) | 卷集合。`volumes[0]` 为默认卷（root 缺省取 `storage_root`；第二卷起必须显式 root） |
| `volumes[].name` | string | (必需) | 卷名（owner 视图/API 中可见标识） |
| `volumes[].root` | string | (卷0=`storage_root`) | 卷物理根目录（装配时自动 `MkdirAll` + `storage.OpenRoot` LAYOUT_VERSION 校验） |
| `volumes[].vol_capacity` | int64 | `0` | 本卷容量上限（0 = 不限）。auto 路由按容量换卷（每卷独立容量池）。值支持人类可读大小（`"100GiB"`）或纯数字字节 |
| `volumes[].acl.mode` | string | `deny` | 卷 ACL 模式：`deny`（黑名单，`owners` 列出的 owner 禁止）或 `allow`（白名单，仅列出的 owner 允许）。缺省 `deny` + 空 `owners` = 默认开放（兼容旧单根） |
| `volumes[].acl.owners` | []string | (空) | ACL 名单。空名单在 `deny` 下全部放行、在 `allow` 下全部拒绝 |

配置示例见 `config.example.yaml`。

**API / 客户端**：

- `GET /api/volumes`（auth + per-owner）→ `{volumes: [{name, mode, capacity, usage, allowed}]}`
  仅返回当前 owner 允许的卷（ACL 收紧卷绝不列出）。
- `POST /api/volumes/move?from_volume=<v>&to_volume=<v>&filename=<rel>`（同 owner 同相对路径跨卷迁移）。
- `POST /api/volumes/rebalance?from_volume=<v>&to_volume=<v>&max_bytes=<n>`（卷再平衡：把 from 卷文件按大小降序逐文件迁到 to 卷，直到 max_bytes 用尽或无可迁文件；max_bytes 缺省=0 不限。单文件失败跳过，尽力而为；同 rel 并发由 uploadingFiles 锁串行化）。
- upload / download / list / stat / delete / rename 支持可选 `volume` 参数（upload 表单字段、
  其余 query）；缺省 = auto（无卷语义 / 服务端自动路由）。
- 上传成功响应头 `X-Volume` 标识落盘卷；`/api/files` 列表文件条目带 `volume` 字段。
- sclient：`volumes` 子命令（可见卷 + 用量）、`upload --volume` / `list --volume` /
  `download`/`delete`/`meta` 可选 `--volume`、`mv --to-volume <卷>`（跨卷 move）。
- WebUI：文件行卷 badge + 监控弹窗「卷」仪表 + 上传「卷」下拉（`/api/volumes` 驱动；
  未配 AK/SK 时仪表优雅降级，不破坏无认证浏览）。

**版本管理与多卷注意**：`versioning.max_versions` 按**卷目录**独立计数——跨卷 `move`
后，留在源卷的版本仍然可见（版本字节不随文件迁移），但不会被源卷的新覆盖写触发修剪，
故可见版本数可能超过 `max_versions`。如需收紧请手工 `DELETE /api/versions` 清理。

`versioning.retention`（保留期，duration）可按版本创建时间清理历史版本：超过保留期的
版本在每次写路径清理（`SaveVersion`/restore 顺带执行）时删除；`versioning.gc_interval`
（周期 GC 间隔）可额外启动一个周期任务，按保留期扫描整仓版本目录（两者默认 0 = 关闭，
零行为变化）。

### Gzip 压缩

服务端自动为 JSON 响应启用 gzip 压缩（当客户端 `Accept-Encoding` 包含 `gzip` 时），
无需额外配置。二进制文件下载流不做压缩。

### 云端下载（cloud_download_*）

服务端离线下载任务（`POST /api/cloud/download` 等，见 [api.md](./api.md)「云端下载」）配置：

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `cloud_sync_threshold` | int | `20971520` (20 MiB) | 同步模式阈值（当前 handler 提交时大小未知恒异步，字段保留供未来按大小同步） |
| `cloud_downloader` | string | `http` | 云端下载器名称（当前仅内置 http 实现） |
| `cloud_max_concurrent` | int | `3` | 最大并发下载数 |
| `cloud_max_batch_urls` | int | `100` | 批量/组下载单次最大 URL 数，超过服务端返回 400 |
| `cloud_task_ttl` | duration | `24h` | 完成任务保留时间，过期自动清理 |
| `cloud_failed_task_ttl` | duration | `1h` | 失败/取消任务保留时间 |
| `cloud_download_timeout` | duration | `30m` | 单次下载尝试整体超时（超时后自动重试续传） |
| `cloud_download_idle_timeout` | duration | `1m` | 响应体读取空闲超时：超过该时长未收到数据即中断本次尝试 |
| `cloud_max_retries` | int | `10` | 瞬时失败（网络/5xx/超时）最大重试次数 |
| `cloud_retry_delay` | duration | `10s` | 重试间隔 |
| `cloud_download_allow_private` | bool | `false` | 允许下载私有 IP 地址（默认关闭，SSRF 防护） |
| `cloud_download_exit_node` | string | (空) | 云端下载经 mesh 出口节点 ID（空=服务端本地直连）。非空时下载器 Transport.DialContext 指向「本地直连优先→失败回退经出口（hub 中继 RelayStream）」拨号；需 mesh.hub_url + mesh.access_key/secret（fail-closed） |
| `cloud_archive_max_bytes` | int | `0` | 单次云归档允许的原始文件大小总和（0 = 不限制，仍受 `max_storage_bytes` 兜底） |

### Hub 中继与传输（hub.*）

mesh / relay / p2p 的中继与传输配置：

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `hub.enabled` | bool | `false` | 启用 Hub 中继（注册/寻址/信令/兜底中继） |
| `hub.node_id` | string | (空) | 本节点 ID |
| `hub.virtual_subnet` | string | `100.64.0.0/10` | 虚拟 IP 子网（CGNAT；mesh connect `--virtual-subnet` 需一致） |
| `hub.transports.ws.enabled` | bool | `false` | WebSocket 传输 |
| `hub.transports.ws.listen` | string | (空) | WS 监听地址 |

### 时长字段格式

所有 `*_timeouts.*` 与 `*_ttl` / `window` 字段都使用 Go duration 字符串：
`"5s"`、`"30s"`、`"5m"`、`"24h"` 等。

### tunnel_key 已废除

旧版由配置文件/环境变量提供 `tunnel_key`（64 hex）再随配置写回 YAML 的行为**已移除**：
服务端启动不再读取或生成 `tunnel_key`。隧道密钥现在由凭据 Ring 中条目的 SK 经
`tunnel.DeriveTunnelKey`（HKDF）自动派生——无需手动配置，也无需在客户端配置
`tunnel_key`（该键被忽略，仅历史兼容）。

## SIGHUP 热重载

对运行中的 sproxy 发送 SIGHUP，会触发部分配置热重载。**仅以下字段在 SIGHUP 后生效**：

- `log_level`
- `log_format`

其他字段（`addr`、`storage_root`、`owner_quotas`、`bucket_limits`、`rate_limit`、
`server_timeouts`、`max_header_bytes`、`tls.enabled`）需要**重启进程**。SIGHUP 时会打印警告说明哪些字段未生效。

凭据已 store 化（`<storage_root>/<owner>/meta/credentials.json`），不再经配置文件——
SIGHUP 与凭据无关；轮换/管理凭据请用 `sclient trust renew` / `/api/credentials`。

## 备份与恢复

多租户布局（`<tenant>/{user,cloud,archive,chunk,version,meta}/` 桶）的整根备份与恢复：

```bash
# 备份：产出 build/backups/sproxy-backup-<时间戳>.tar.gz（内嵌 manifest.json）
make backup                # 或 scripts/sproxy-backup.sh --storage-root <path> --output <dir>

# 恢复：BACKUP=<tar.gz> 指定备份文件，恢复到 storage_root
make restore BACKUP=build/backups/sproxy-backup-xxx.tar.gz
# 或 scripts/sproxy-restore.sh --backup <tar.gz> --target <storage_root>
```

- 备份包含全部桶（含 meta 桶的凭据 store 与分享持久化文件）；`--include-audit` 可显式带上审计导出。
- 恢复前校验备份 manifest 版本与当前版本一致，**拒绝跨版本恢复**（防旧布局覆盖新布局）。
- 脚本测试：`make test-backup-restore`（纯 bash 夹具，无网络）。

### WebDAV 网关（`sproxy dav`）

`remote://<node>/<vol>[/<path>]` 远端卷可暴露为本地 WebDAV 端点，任意工具（curl / rsync /
编辑器 / 文件管理器）直接读写，无需了解 mesh 内部寻址：

```bash
sproxy dav --listen 127.0.0.1:8080 remote://nodeA/main
```

- `--listen` 默认 `127.0.0.1:8080`（仅回环）；WebDAV 协议（RFC 4918：PROPFIND/PUT/GET/
  MKCOL/DELETE/MOVE/COPY）。
- 子路径起点：`remote://nodeA/main/subdir` 把 WebDAV 根对准卷内 `subdir`。
- 凭据复用主配置 `mesh.hub_url` / `mesh.access_key` / `mesh.access_key_secret`；
  hub 地址为空时指向本机 HTTP 面。
- 权限：WebDAV 端点本身不带额外鉴权（监听仅回环时依赖回环安全；暴露到非回环需自行加
  反向代理鉴权——当前版本建议仅回环使用）。


### sclient trust register（TOTP 注册）与 trust login（TOTP 登录回填）

`force_totp: true` 部署下，用户先 `sclient trust register` 注册（打印 AK / TOTP 密钥，
不建 SK 条目），录入 Authenticator 后再 `sclient trust login` 完成登录绑定。

- **`trust register [username]`**：调 `POST /api/credentials/register` 注册（用户名
  即 owner，位置参数可选，默认=AK），打印 `ak` / `base32_secret`（**只展示这一次**）
  / `otpauth_uri`；响应 `admin:true` 时额外提示「您是首个注册用户，将成为 admin」
  （S2）。回填配置 `access_key`（供后续 login 默认使用；无 session SK 故不回填
  secret/id），并提示用 `trust login <用户名>` 完成绑定。**不读动态码、不登录**。
- **`trust login [username]`（推荐，用户名直接作为位置参数，免记 AK）**：登录身份
  = 位置参数 <username>（owner 反查 AK）> 配置 `access_key` > `--ak <AK>`；三者都
  没有 → 报错指引 `trust register`（**不再隐式注册新账号**，注册由 register 子命令
  承担）。已注册 → 提示输入 6 位动态码。
- `RequestTOTPNonce` → `LoginTOTP(..., "cli")`（`login_type=cli`，D3）→ 服务端签发
  短命 session SK（`KindTOTPWrap` 信封，`registration.cli_ttl` 默认 7d）→ 客户端解开后
  回填 `access_key` / `access_key_secret` / `access_key_id`。
- 回填前 D4 覆盖确认：已有 `access_key_secret`（可能来自 renew 的长命 SK）时提示并
  交互确认（y/N），`--overwrite` 跳过；拒绝覆盖 → 非零退出（独立哨兵
  `errLoginOverwriteDenied`）。
- register 无 flags（用户名位置参数）；login flags：`--ak`（仅需要时手动指定）、
  `--overwrite`。
- TOTP 注册/登录走**显式无凭据客户端**（M14）：公开端点直达，不携带签名头。

## 客户端多环境多用户（context 模型，v2）

sclient 从单份平铺配置升级为 kubectl 式 **environments / users / contexts 三件套**：

```yaml
# ~/.config/sproxy/config.yaml
apiVersion: sclient/v1
kind: Config
current-context: sg-prod
environments:
  - name: sg-prod
    server_url: https://hub.example.com:18083
    hub_url: wss://hub.example.com:18083/ws
    node_id: home
    turn: [{uri: "turn:turn.example.com:3478", user: u, pass: p}]
    stun: []
    virtual_subnet: "100.64.0.0/10"
users:
  - name: alice
    access_key: ak-xxx
    access_key_secret: <64hex>
    access_key_id: skey-xxx
    owner: alice
contexts:
  - name: sg-prod
    environment: sg-prod
    user: alice
```

- **environments[]**：连接面（server_url / hub_url / node_id / TLS / TURN / STUN / virtual_subnet）——mesh/relay/p2p/socks 从当前 env 回落连接参数。
- **users[]**：凭据面（access_key / access_key_secret / access_key_id / owner）——明文 + 文件 600。
- **contexts[]**：env+user 组合 + 卷覆盖；`current-context` 指针写在本文件。
- **解析优先级**：`--context` > `--env`+`--user` > `current-context` > 旧 `SCLIENT_ENV` 映射。
- **脚本化**：`sclient --env sg-prod --user alice upload big.bin`；环境变量 `SCLIENT_CONTEXT` / `SCLIENT_ENV` / `SCLIENT_USER` 同效。

**迁移（零破坏）**：启动时检测到旧 `~/.config/sproxy/sclient.yaml`（或 `SCLIENT_ENV` 对应 `sclient.<env>.yaml`）且无 config.yaml → 自动导入为 context（名 = env 或 default），打印「已导入为 context <name>；旧文件保留可删」。

**上下文切换命令**：

```bash
sclient context list              # 列出全部 context（标 * 当前）
sclient context use sg-prod       # 切换 current-context
sclient context get [name]        # 显示解析后合并视图（secret 脱敏）
sclient context set demo --env sg-prod --user alice   # 创建/更新 context
sclient context delete <name>     # 删除（current 拒绝）
sclient context rename <old> <new>
sclient env list / env use <name>     # 切环境
sclient user list / user use <name>   # 切用户
```

**凭据命令作用于当前 context**：`trust register [username]` 用当前 env 注册并**自动切到该用户**；`trust login [username]` / `trust renew` 作用于当前 context 的 env/user 段。

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
| `tunnel_key` | string | (空) | **已废除**（被忽略，仅历史兼容）；隧道密钥现由凭据 SK 经 HKDF 自动派生，无需配置 |
| `chunk_size` | int64 | `4194304` (4 MiB) | 默认分块大小 |
| `max_chunk_size` | int64 | `0` | 自适应分块上限；0 = fallback 到 64 MiB |
| `access_key` | string | (空) | SproxySig 认证 AccessKey（服务端凭据 Ring 中的 AK，见首启日志 / `sclient trust` 登记的 AK） |
| `access_key_secret` | string | (空) | SproxySig 认证 AccessKeySecret（对应 AK 的 SK——本地密钥，仅计算签名，永不上线） |
| `allow_transport_fallback` | bool | `false` | 允许隧道/xfer 初始化失败时回退直连 |
| `hub_url` | string | (空) | mesh/relay/p2p 共用的 hub 地址（http(s) 或 ws(s)，可带 /ws 路径）。为空时各命令按自身语义回落（mesh→server_url，p2p→报错，relay→本地默认） |
| `relay_token` | string | (空) | **已废除**（不再有该配置键）：hub 注册准入由服务端凭据 Ring 的 SproxySig AK+HMAC proof 提供；配置该键不生效 |
| `node_id` | string | (空) | 本节点默认 ID（mesh/p2p/relay 信令来源与寻址目标；为空回落主机名） |
| `peer_fingerprints` | []string | (空) | 对端身份指纹 pinning 列表（64 hex 或 `sha256:<64 hex>`，逗号分隔）。仅 `sclient tunnel --xfer <name>`（xfer/mux 隧道）消费：握手时 fail-closed 校验对端 Ed25519 身份指纹，防 MITM。对端指纹取 `sclient identity fingerprint` 带外固化；本端身份由 `sclient identity generate` 生成（XDG 目录 `sproxy/identity.json`）。需配置 `access_key`/`access_key_secret`（派生隧道密钥使握手执行；缺 key 但配置了身份/指纹时 fail-closed 报错）。传统隧道/文件直连命令不走 xfer 握手，配置此项会 fail-closed 报错（指引使用 `--xfer`），不静默跳过 |
| `xfer_ca_file` | string | (空) | xfer `tcp+tls` 传输的受信 CA 文件路径（PEM；等价 `--ca-file`）。为空时用系统根池严格校验——服务端为自签证书（auto_tls）时握手报 `x509: certificate signed by unknown authority`，需配置此项或 `xfer_insecure` |
| `xfer_insecure` | bool | `false` | 跳过 xfer `tcp+tls` 传输的证书校验（等价 `--insecure`）。**仅限 loopback hub**（远程 hub + insecure fail-closed 拒绝，需改用 `xfer_ca_file`）；与 `xfer_ca_file` 互斥 |

> **TURN 中继配置（CLI flag，非配置键）**：TURN 凭据只在 sclient 发起 webrtc 打洞时
> 本地使用，**服务端无 TURN 配置**。动态 TURN REST 短期凭证（coturn 标准）通过 CLI
> flag 传入：`--turn-rest <url> --turn-rest-user <user> [--turn-rest-service <svc>]`
> （`--turn-rest-user` **必填**：配 `--turn-rest` 时缺省会命令终止），
> 支持命令为 `mesh connect` / `p2p connect` / `socks` / `udp map` / `mesh node`。REST 优先于
> 静态 `--turn`/`--turn-user`/`--turn-pass`；失败降级回落静态凭据（若有）/仅 STUN
> （不 panic）。安全边界：
> 端点默认强推 `https://`，明文 `http://` 仅限 loopback；URL/username/service 上限 512
> 字符。详见 [cli.md](./cli.md) 的「TURN 中继」段落。

> **mesh 多跳端到端加密（via-node / via-direct，T1）**：数据面加密**与 SproxySig SK 解耦**——
> 会话密钥由 ECDH（L/T 各自的 Ed25519 身份经握手派生，前向保密）+ 公开指纹派生的静态密钥
> 构成，**不是**由 SK 派生（SK 是群准入凭证：L/X/T 三节点都持有；X 中间节点即使持有 SK
> 也无法派生会话密钥，读不到 L⇄T 明文，只能透传密文）。启用：本端身份（`sclient identity
> generate` 生成，XDG 目录 `sproxy/identity.json`，或 `NodeConfig.Identity` 字段）+ 对端
> 指纹 pinning（`AllowedPeerFingerprints` 白名单；无 pin 时 fail-closed 拒绝）。X 侧只做
> mux 流字节泵（`ServeE2ERelay`），不建隧道不解密。当前接入范围：**协议层能力已落地（DialE2E/ServeE2ERelay API + 测试）**，
> 生产数据面**尚未启用**——via-node/via-direct 多跳出口拨号与 mDNS 直连的端到端加密接线见后续片。

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

### 跨节点同步（`sync_remotes` / `mesh`）

`sync_remotes` 描述「有哪些远端」，`mesh` 段描述「A 侧怎么接触 hub 与信令」。两个载体：

```yaml
sync_remotes:
  - name: "node-a"            # direct：HTTP 直连（必须有 url + access_key/access_key_secret）
    url: "https://192.168.1.10:18083"
    access_key: "ak-…"
    access_key_secret: "…"
  - name: "node-b-mesh"       # mesh：经 mesh 隧道（必须有 node/volume/peer_pins；不需要 url/凭据）
    kind: "mesh"
    node: "node-b"
    volume: "main"
    peer_pins: ["sha256:…"]
    transport: "auto"         # relay | auto | webrtc

mesh:
  hub_url: ""                 # 空 = 本机 hub（自连）；非空 = **远端 hub**（需凭据，见下）
  node_id: ""                 # WebRTC 信令用的本节点 ID；transport=webrtc 时必需
  access_key: ""              # 远端 hub 的 SproxySig 凭据（hub_url 留空时无需配置）
  access_key_secret: ""
  skey_id: ""
  insecure_tls: false
  stun: ["stun:stun.l.google.com:19302"]   # 实例级 ICE（本进程独享，不污染 CLI 全局）
  turn: []
  turn_user: ""
  turn_password: ""
```

**`transport` 选路矩阵**（`kind: mesh` 时生效）：

| 值 | 打洞 | 回落 hub 中继 | 前置 |
|---|---|---|---|
| `relay` | ✗ | ✓（唯一路径） | — |
| `auto`（缺省） | 尝试 | ✓ | 无（无 `mesh.node_id` ⇒ 退化为纯中继） |
| `webrtc` | 必须 | **✗**（失败即报错） | `mesh.node_id`（配置期与运行期**双重** fail-closed） |

**启动期校验（fail-fast）**：`hub_url` 非空时必须有 `access_key`/`access_key_secret`/`skey_id`
且 URL 为 http(s)；`transport: webrtc` 必须有 `mesh.node_id`；两者都只在**确有 `kind: mesh` 远端**时校验。

**B 侧形态（两种，二选一）**：对端 sproxy 的 `remote_read.listen` / `remote_write.listen` 强制
loopback，跨节点可达性由 mesh 提供 ⇒ B 侧必须把这两个地址**宣告**为服务并允许出口拨号：

1. **进程内角色（推荐，`mesh.node.enabled: true`）**：由 sproxy 自身承担，部署形态收敛为单进程。
   服务声明按 `remote_read`/`remote_write` 的**监听地址自动派生**（`volread`/`volwrite`），
   并自动精确放行这些 loopback 地址；`mesh.node.webrtc: true` 时同时接受 WebRTC 直连。
   `node_id` 回落链：`mesh.node.node_id` → `mesh.node_id` → `hub.node_id`。

   ```yaml
   mesh:
     node:
       enabled: true
       webrtc: true          # 接受直连；false = 只提供中继
       # hub_url: ""         # 空 = mesh.hub_url；再空 = 本机 hub
       # extra_services: ["ssh:127.0.0.1:22"]
   ```

2. **外部 sidecar**（不在服务端进程内跑 mesh node 时）：

   ```bash
   sclient mesh node --hub wss://hub.example.com/ws --node-id node-b      --service volread:127.0.0.1:19000 --service volwrite:127.0.0.1:19001 --dial-allow
   ```

**安全说明（重要）**：WebRTC 直连**只是数据面**（绕过 hub 中继，不绕过授权）。授权仍逐请求在隧道层
生效：A 侧必须通过双向 Ed25519 指纹 pin（`peer_pins`）握手，B 侧再做 `mesh_readers` 的
**scope 校验**（`read`/`write`/`rw`，读不隐含写）+ 卷 ACL + 文件级锁。出口拨号另有白名单
（`--dial-allow` / 服务宣告地址），是 B 侧的第二道闸。

### 当前目录（cd / pwd）

sclient 支持工作目录概念，持久化到 XDG cache（`~/.cache/sproxy/current_dir`）。
详见 [cli.md](./cli.md)。

## 示例

### 服务端

```yaml
# sproxy.yaml
addr: ":18083"
storage_root: "/var/lib/sproxy/storage"
owner_quotas:
  "*": "10GiB"        # 默认每租户 10 GiB（人类可读，= 10737418240 字节）
  alice: "20GiB"       # alice 20 GiB（人类可读）

`owner_quotas` 对**外部卷同步的 staging 中间态**同样生效（P5，quota per-owner）：同步任务
（push 到 baidupcs/WebDAV/S3 卷，或用户卷）写本地 staging 时按任务 owner 分桶预留——
owner 配额不足则该文件失败（任务不中止，记 ActionError），上传完成立即释放。未配置
owner 的配额（scopeFor 返回 nil）→ 不装配 staging 配额（兼容旧装配）。
# max_upload_bytes 已移除（普通上传请求体固定 1 GiB 上限）
# 凭据不再写在配置文件：首次启动自动生成 anonymous 凭据（SK 落盘，见启动日志 AK），
# 后续经 sclient trust renew 轮换、/api/credentials 管理。mesh 身份从 AK 派生、隧道密钥
# 由 SK HKDF 派生——mesh/tunnel 密钥均由凭据自动派生，无需手动配置。
registration:
  disable: false        # false=允许注册（默认）；true=禁止注册（仅存量用户）
  force_totp: false     # true=TOTP 注册分支（sclient trust login 登录回填）；默认简单 AK/SK
  session_ttl: 24h      # TOTP 登录（web）session SK 有效期（默认 24h，D3）
  cli_ttl: 168h         # TOTP 登录（cli 回填）session SK 有效期（默认 7d，D3）
  login_fail_limit: 5   # per-AK 登录失败锁定阈值（U4，默认 5）
  login_fail_window: 15m # per-AK 锁定窗口（U4，默认 15m）
allow_insecure_loopback: false  # 无凭据时回环是否放行读取（默认 false 更严格）
credential_ttl: 720h  # SK 默认有效期（默认 30d）

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
access_key: "ak-meshA-3f8a..."
access_key_secret: "0123...（64 hex，与服务端该 AK 对应的 SK 一致，见首启日志 / `sclient trust` 输出的 SK）"
check_checksum: true
chunk_size: 8388608    # 8 MiB
```

## 百度网盘后端（BaiduPCS plugin）

`sproxy baidupcs --bduss <BDUSS> [--binary /path/to/BaiduPCS-Go] [--root /baidu]`
百度网盘存储后端自检。独立 module（pkg/baidupcs）引用外部 fork，二进制优先+库兜底。

## 外部卷容量纳管（C2-C4）

外部卷（baidupcs/webdav/s3 系统盘 + 用户卷）的容量是**本系统可用限额**（UserVolume.Capacity
或 volumes[].vol_capacity；0 = 不限）——与本地卷（owner_quotas 物理资源）不同，外部卷不占
本机磁盘，限额是「本系统授权占用外部卷的额度」。

- **卷级计数记账**：外部卷写入累计（超限拒绝）、删除释放——`pkg/volume/capacity`（CapacityFS
  装饰器包装 backend FS，counter 持久化 `<root>/<owner>/meta/volume/<name>.capacity.json`）。
- **用量查询**：`GET /api/volumes/user`（用户卷）与 `GET /api/volumes`（系统盘）返回每卷
  `usage`（本系统已用）；backend 支持时另有卷总量（baidupcs 配额 / S3 bucket 用量，
  WebDAV 无标准 API 仅限额维度）。
- **Web/CLI**：卷面板 + `sclient volume list` 显示容量/已用。
