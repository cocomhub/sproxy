<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sclient 命令行参考

sclient 是 sproxy 的配套客户端，基于 cobra + pflag。所有命令均支持
`--config`、`--server`、`--access-key` 等全局参数。

## 全局选项

以下参数挂在**根命令**上，对所有子命令生效（下文简称“全局选项”）。

### 连接与认证

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--config` | XDG 路径 | 指定客户端配置文件路径 |
| `--server`, `-s` | (空) | sproxy 服务端地址（覆盖 `server_url` 配置）；为空时用配置值，配置也为空则以 `https://127.0.0.1:18083` 为默认 |
| `--access-key` | (空) | SproxySig 认证 AccessKey（服务端凭据 Ring 登记对应 AK/SK 时需要） |
| `--access-key-secret` | (空) | SproxySig 认证 AccessKeySecret（本地密钥，仅计算签名，永不上线） |
| `--access-key-id` | (空) | SproxySig SK 条目 ID（skey-id；v2 协议必传，`trust renew` 回填） |
| ~~`--volume`~~ | — | **已移除**（2026-10-14）：卷寻址统一为**路径内卷 URI**（`<卷名>:/<路径>`）或 `cd <卷名>:/…` 设定的工作目录卷 |
| `--download-proxy` | (空) | 下载回退代理地址（如 `http://127.0.0.1:1080`）；直连服务端失败/超时自动经此代理重发，空 = 关闭回退 |

> `mesh` / `relay` / `p2p` / `socks` / `udp` 等命令另有各自的 `--hub`（Hub 的 ws/wss 地址）；
> 而 `relay status` / `relay stats` / `relay remove-node` 需要的是 Hub 的 **HTTP 管理地址**，
> 可用 `--server` 直接指定，或由 `--hub` 派生（`ws://` → `http://`、`wss://` → `https://`，丢弃 path）。

### 传输与续传

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--chunked` | false | 启用分块上传/下载模式 |
| `--chunk-size` | 0（用 4MB） | 分块大小（字节） |
| `--concurrency` | 0（用 4） | 上传/下载并发数 |
| `--resume` | false | 续传模式（默认已启用，此开关用于显式声明） |

### 输出

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--output`, `-o` | (空) | 指定下载文件的输出路径 |
| `--json` | false | 以 JSON 格式输出（机器可读；`-o`/人类可读文案被抑制） |
| `--verbose`, `-v` | false | 显示详细输出 |

### TLS 与传输回退

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--insecure` | false | 跳过 TLS 证书校验。**双语义**：HTTP 直连面不限地址；xfer tcp+tls 面**仅限 loopback hub**（远程必须改用 `--ca-file`，fail-closed） |
| `--ca-file` | (空) | xfer tcp+tls 的受信 CA 文件（PEM）；服务端自签证书时使用，与 `--insecure` 互斥 |
| `--protocol-salt-key` | (空) | 协议域分离盐自定义密钥（64 hex）；派生替换默认 sproxy 盐防协议指纹识别。⚠️ 必须与握手对端使用相同 key（stealth 部署配合加密内嵌派生盐） |
| `--client-cert` | (空) | mTLS 客户端证书路径（PEM），需与 `--client-key` 成对 |
| `--client-key` | (空) | mTLS 客户端私钥路径（PEM） |
| `--client-cert-allow-missing` | false | 客户端证书加载失败时继续执行（默认失败即退出） |
| `--allow-transport-fallback` | false | 允许隧道/xfer 初始化失败时回退直连（默认严格模式，不回退） |

## 子命令一览

| 命令 | 用途 |
|---|---|
| [`upload`](#upload) | 上传文件 |
| [`download`](#download) | 下载文件 |
| [`delete`](#delete) | 删除文件 |
| [`mv`](#mv) | 重命名 / 移动 |
| [`stat`](#stat) | 查询文件元信息 |
| [`ls`](#ls) | 列出文件/目录（`list` 为等价别名） |
| [`tree`](#tree) | 树形递归显示目录结构 |
| [`mkdir`](#mkdir) | 创建目录 |
| [`rmdir`](#rmdir) | 删除目录 |
| [`search`](#search) | 搜索文件 |
| [`du`](#du) | 查看目录空间占用（递归统计） |
| [`df`](#df) | 查看卷水位与磁盘空间 |
| [`quota`](#quota) | 查看本 owner 配额（**跨卷联邦视图** + 逐卷明细） |
| [`share`](#share) | 分享链接管理（create / list / revoke；**支持外部卷/加密卷**） |
| [`trash`](#trash) | 回收站管理（list / restore / empty；**多卷**：`--volume` / `--all`） |
| [`batch`](#batch) | 从文件逐行读取命令批量执行（并发） |
| [`batch-delete`](#batch-delete) | 批量删除文件 |
| [`batch-rename`](#batch-rename) | 批量重命名文件 |
| [`volume`](#volume) | 管理用户自有卷（网盘盘：create / list / delete） |
| [`backup`](#backup) | 导出卷为 tar 备份到本地（流式下载 + manifest 校验清单） |
| [`migrate`](#migrate) | 迁移向导：单机→多卷/联邦自动化迁移（export/import/mirror-config） |
| [`cd`](#cd) | 切换当前目录 |
| [`pwd`](#pwd) | 打印当前目录 |
| [`context`](#context) | 多环境多用户上下文管理（list/use/get/set/delete/rename + env/user list/use，切换 kubectl 式环境/用户） |
| [`tunnel`](#tunnel) | 通过隧道发送任意 HTTP 请求（`--xfer <name> --hub <addr>` 走 xfer/mux 隧道，启用身份指纹 pinning） |
| [`socks`](#socks) | SOCKS5 代理出口（CONNECT 经 mesh 到出口节点，出口出站拨号） |
| [`udp`](#udp) | UDP 隧道：`udp map` 端口映射（经 mesh 到出口，出口转发到远程 UDP 地址） |
| [`mesh`](#mesh) | mesh 服务发现与连接：`connect` / `node` / `status` / `acl` |
| [`http-proxy`](#http-proxy) | 正向 HTTP 代理（绝对 URI + CONNECT，经出口或本地直连；`http_proxy` 环境变量即用） |
| [`cloud-download`](#cloud-download) | 云端下载（提交→等待→打包→下载→清理，链式或子命令分步） |
| [`identity`](#identity) | 节点长时身份密钥管理（Ed25519，供对端指纹 pinning） |
| [`relay`](#relay) | 中继节点：连接到 Hub，转发请求到本地 HTTP 服务 |
| [`genkey`](#genkey) | 生成 64 hex 随机 AES-256 密钥（自检/手动构造用；隧道密钥由凭据 SK 派生） |
| [`config`](#config) | 配置管理 |
| [`sealfile`](#sealfile) | 本地文件加解密（与 secretdata 卷同内核；加密产物可作卷加载播放） |
| [`upgrade`](#upgrade) | 自更新（GitHub Releases 校验 + 原子替换） |
| [`version`](#version) | 打印版本信息 |

## 卷感知工作目录（`cd <卷名>:/<目录>`）

工作目录**含所在卷**：`cd` 支持卷 URI，其后所有相对路径操作默认只作用于**当前工作目录所在卷**
（用户口径 2026-10-13）。

```bash
sclient cd vault:/docs      # 切到加密卷 vault 的 /docs（工作目录 = vault:/docs）
sclient pwd                 # 打印 vault:/docs
sclient ls                  # 列 vault:/docs（工作目录卷；无需任何 flag）
sclient upload ./a.bin      # 目标卷 = vault，路径 = docs/a.bin
sclient cd /                # 前导 `/` = **本地卷**根（回到默认卷）
sclient cd other:/x         # 切到另一卷 other 的 /x
```

规则：

- `<卷名>:/<路径>` = 该卷根起算的**绝对路径**（不受当前工作目录影响）；`<卷名>:` / `<卷名>:/` = 卷根。
- 前导 `/` 开头的路径 = **本地卷**（默认卷）根起算，并**离开**当前卷。
- 相对路径沿用当前工作目录（含所在卷）。
- 优先级：**位置参数卷 URI** > **工作目录所在卷**（`cd <卷名>:/…`）> 配置 `volume`/auto
  （同一命令多个位置参数指向不同卷时报错，不静默取其一；`mv`/`cp` 的第二端为跨卷目标）。
- 其它命令可用卷 URI 一次性寻址（不改工作目录）：`ls vault:/`、`meta vault:/a.txt`、
  `download vault:/a.txt`、`mv vault:/a vault:/b`、`delete vault:/a`。

### 跨卷操作（两端卷 URI）

```bash
sclient mv vault:/a.txt other:/a.txt                 # 跨卷移动（两端卷 URI；目标不同卷即跨卷）
sclient cp main:/a.txt vault:/a.txt                   # 跨卷复制（两端 URI）
sclient volume move a.txt --from-volume vault --to-volume other   # 旧式跨卷移动（`volume` 子命令）
```

**外部/加密卷已覆盖（2026-10-13）**：任一端为外部/加密卷（secretdata 等）均可工作——复制段经
**统一写内核**（配额双账本 / sidecar / 封印与 `cp`/上传同口径），移动 = 复制成功**之后**删源
（外部卷经接缝硬删、本地卷走既有 `delete` 路径）。移动**非原子**：删源失败会返回错误并说明
「已复制到目标，源删除失败」，此时目标已写入、源仍在（重试或手工清理）。同一文件名（同 rel）在
**其它**卷已存在 → 409（AD-4 防跨卷双份；移动**豁免源卷**本身——它就是被搬走的那份）。

### cp（复制，源保留）

```bash
sclient cp a.txt b.txt                  # 同卷复制（默认与源同卷）
sclient cp disk1:/a.txt disk2:/a.txt      # 目标卷（两端卷 URI）
sclient cp vault:/a.txt vault:/b.txt      # 卷 URI（同卷）
sclient cp main:/a.txt vault:/a.txt       # 跨卷（两端 URI）
sclient cp a.txt b.txt --force            # 允许覆盖已存在目标
```

- **默认不覆盖**：目标已存在 → 409（绝不静默覆盖用户内容）；`--force` 才覆盖（先删后写，非原子）。
- **同卷 + 卷支持底层 blob 直拷**（如 secretdata 加密卷）：复制**不重新加解密**——同容器直接共享
  分块（零 blob 写、O(meta)），跨容器则字节拷贝分块（内容零改动）；删源或删副本都安全（引用计数为
  「存在即引用」）。跨卷因目标卷独立密钥**必须重新加密**。
- **配额与写入同口径**：复制经与上传相同的**双账本预留**（owner Scope + 卷容量池），配额不足 →
  `507` fail-closed（目标不落半态）。注意：外部卷的 `volumes[].capacity` 在**上传与复制**都不做
  写时拦截（由真值/周期对账收敛），二者口径一致。

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

## 卷 URI 寻址（`<卷名>:/<路径>`）

文件操作的位置参数**一律**用**卷 URI**（唯一寻址方式；`--volume` 已移除）：

```bash
sclient ls     vault:/                 # 列出加密卷 vault 的根目录
sclient meta   vault:/docs/top.txt     # 取元信息（含明文 checksum）
sclient download vault:/docs/top.txt   # 下载
sclient mv     vault:/a.txt vault:/b.txt
sclient delete vault:/b.txt
```

- `<卷名>:` 或 `<卷名>:/` = 卷根；路径部分按普通相对路径解析（仍受 `cd` 影响）。
- 等价性：`sclient ls vault:/x` ≡ `cd vault:/ && sclient ls x`。同一命令多个位置参数
  **卷名不一致时报错**（不静默取其一）；一致则等价。
- `mv`/`cp` 的两端可分别带卷名：不同卷 → **跨卷迁移/复制**；同卷 → 同卷改名/复制。
- `upload` 的位置参数是**本地文件路径**（不使用卷 URI）⇒ 目标卷经 `cd <卷名>:/<目录>` 设定
  （工作目录卷）；`download` 的远端参数用卷 URI。
- 与 URL/盘符的歧义按语法消除：要求冒号后紧跟**单个**斜杠，故 `https://…` 与 `C:\…` 不会被误判。

**为什么要它**：加密卷（`type: secretdata`）等非默认卷的浏览/改名/删除不必再记 flag，
路径自述归属；同时这也是列出加密卷内容的推荐方式——**未指定卷的 `ls` 只列本地卷**
（搜索索引不含外部卷）。

## 子命令详情

### sproxy-mcp（MCP server 二进制）

`sproxy-mcp` 把 sproxy 文件能力暴露为 MCP（Model Context Protocol）工具，供 AI CLI
（Claude Code / Codex 等）调用。非 sclient 子命令，而是独立的 `cmd/sproxy-mcp` 模块。

```bash
sproxy-mcp --server https://127.0.0.1:18083 \
  --access-key <AK> --access-key-secret <SK> --access-key-id <skey-id> \
  [--transport stdio|sse] [--sse-addr :18900]
```

- `--server`（必填）：sproxy 服务端地址
- `--access-key` / `--access-key-secret` / `--access-key-id`：SproxySig 凭据三要素
  （全部省略时走服务端免认证场景）
- 卷上下文：由**路径内卷 URI** 或 `cd` 设定的工作目录卷给出（`--volume` 已移除）
- `--transport`：传输方式，`stdio`（默认，本地 AI CLI）或 `sse`（远程 HTTP）
- `--sse-addr`：SSE 传输监听地址，默认 `:18900`（`--transport=sse` 时生效）

SSE 传输（远程 HTTP）：`GET /sse` 建立事件流（首帧 endpoint 下发
`/messages?sessionId=<id>`），`POST /messages` 收 JSON-RPC 请求 → 同分派管线 →
事件流回 message 事件。Bearer 认证复用 `--access-key-secret`（常量时间比较，
未提供/错误统一 401 空 body；SK 为空 = 公开，仅限本地/内网部署形态）。

### upload

```bash
sclient upload <file1> [file2...]
sclient upload --chunked --concurrency 8 large.bin
```

- 自动判断是否启用分块上传（>100 MiB）
- 文件路径中的目录结构会被保留：`sclient upload dir/file.txt` → 服务端 `dir/file.txt`
- 支持 `--chunked` 强制开启分块、`--chunk-size`、`--concurrency`、`--resume`
- 完成后输出传输统计行：`耗时 | 速率 | 文件数 [分块成功率]`；`--json` 时输出 `stats` 对象（file_count/total_bytes/elapsed_ns/avg_rate_bps/files）

### download

```bash
sclient download <filename> [output]
sclient download --chunked --concurrency 8 large.bin
```

- 默认走 `GET /download`（支持标准 Range header）
- `--chunked` 启用并发分块下载（走 `/download/chunk`）
- 不指定 output 时使用原文件名
- 完成后输出传输统计行：`耗时 | 速率 | 文件数 [分块成功率]`；`--json` 时输出 `stats` 对象

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

### ls（别名 list）

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

非空目录在没有 `--force` 时会有交互式确认提示；用户确认（或 `--force`）后 CLI 才把
`force=true` 发给服务端（**服务端恒要求该确认**——这是最后一道闸门）。显式指定**外部/加密卷**
时（`rmdir vault:/dir --force`）删除落在**该卷自己的键空间**：经接缝**递归删除**（先子后父），
绝不牵连本地卷的同名目录。

### cd / pwd

见上文"当前目录概念"。

### sync

```bash
sclient sync push --remote <name> [--src <path>] [--dst <path>] [--recursive] [--wait]
sclient sync push --remotes <r1,r2,r3> [--src <path>] [--wait]   # 多节点扇出
sclient sync pull --remote <name> [--src <path>] [--dst <path>] [--recursive] [--wait]
sclient sync both --remote <name> [--src <path>] [--dst <path>] [--recursive] [--wait]
sclient sync watch --remote <name> [--src <path>] [--dst <path>] [--verify] [--poll <s>] [--debounce-ms <n>] [--direction pull|push|both] [--delete-propagate]
```

- 在本地 sproxy 服务端创建节点间文件同步任务（push 本地→远程 / pull 远程→本地 /
  **both 双向**：一次任务内先 push 再 pull，两端新增/修改互相传播、最终两边一致），
  由服务端 SyncManager 托管执行；`--remote` 是服务端 `sync_remotes` 配置的远程节点名
- **多节点扇出**（`--remotes a,b,c`，与 `--remote` 二选一）：一次提交创建多个 remote 子任务，
  逐节点独立执行/独立失败（单目标失败不阻塞其它）；`GET /api/sync/tasks/{父任务ID}` 返回
  每 remote 子任务状态汇总（FanoutSummary）；失败节点可用 `sclient sync retry` 独立重试
- **watch 连续同步**：订阅本地服务端 `/api/events` 文件变更事件流（roadmap 4.3 P1），
  upload/rename/delete/mkdir/rmdir/version 事件到达 → 触发一次同步任务（去抖窗口
  `--debounce-ms` 默认 500ms 合并连续事件）；事件流不可用（认证失败/断网）自动退化
  `--poll` 秒间隔轮询（默认 30s，退化日志告警不静默丢事件）；SIGINT/Ctrl-C 优雅退出
  （当前任务完成后）；`--verify` 每次同步后校验核对
- `--direction pull|push|both` 触发方向（默认 `pull` 零回归）：`push` = 本地变更推到远端
  （delete 事件**默认跳过**防误删远程，加 `--delete-propagate` 显式传播）；`both` = 每次事件
  触发 push + pull 各一次（两端新增/修改互相传播；防双向死循环：任务结果不反触发新事件）
- `--src`/`--dst` 均为服务端 uploadsDir 相对路径（默认 `""` = 整个根）；`--recursive` 递归子目录
- `--conflict skip|overwrite|lww|conflict-rename` 冲突策略；`--delete-policy skip|propagate`
  源删除传播策略（默认 `skip` 零回归；`propagate` 时源端删除经一次任务反映到目标）；
  `--sync-empty-dirs`/`--follow-symlinks` 可选
- `--verify` 同步完成后校验核对：重读目标 checksum 与源比对（默认关，零开销；开启后
  任务返回 `verify_failed` 计数与逐文件校验失败清单，`GET /api/sync/tasks/{id}` 的
  `results` 含 `verify_failed` 条目，供审计/失败重试）
- **块级增量**（roadmap 4.3 P2 v1）：本地卷间（LocalFS↔LocalFS）同步大文件小改动时，
  对覆盖更新（overwrite）走**固定块 SHA-256 比对**（默认 1MiB 块，与分块上传 chunkSize
  同粒度）——相同块从旧目标拷入、只差异块从源读取写入（省源读取/写放大）。块校验和
  算法与分块上传 `ChunkChecksums` 一致，后续跨 FS 差异块传输（服务端按块表只收差异块）
  可无缝衔接分块续传基建。任一端不支持块访问自动回退整文件复制（零回归）
- `--wait` 阻塞等待任务终态并展示进度（`--timeout` 超时，0=不限）
- `sclient sync retry <task-id> [--files a,b]`：对任务（Results 含 `error`/`verify_failed`
  失败条目的任务）发起**单文件重试**——服务端构造重试子任务（Include 精确限定失败文件，
  不重跑整个任务），重试结果回写原任务 Results；`--files` 逗号分隔指定部分文件（缺省=全部
  失败文件）；默认表格输出明细（重试成功/失败/跳过），`--json` 输出结构化 `{retried, skipped}`
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

### context

多环境多用户上下文管理（kubectl 式 environments/users/contexts 三件套，配置在
`~/.config/sproxy/config.yaml`）：

```bash
sclient context list              # 列出全部 context（标 * 当前）
sclient context use <name>        # 切换 current-context（写 config.yaml）
sclient context get [name]        # 显示解析后合并视图（access_key_secret 脱敏）
sclient context set <name> --env-name <e> --user-name <u> [--volume-name <v>]  # 创建/更新（flag 名避开 root 全局 --env/--user/--volume，避免同名异意）
sclient context delete <name>     # 删除（current 拒绝，先 use 其它）
sclient context rename <old> <new>
sclient env list / env use <name>     # 切环境（更新当前 context 的 environment）
sclient user list / user use <name>   # 切用户（更新当前 context 的 user）
```

脚本化：`sclient --env <e> --user <u> <cmd>` 或 `--context <name>`；环境变量
`SCLIENT_CONTEXT` / `SCLIENT_ENV` / `SCLIENT_USER` 同效（flag > env > current）。

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
# 裸 TCP 中继（hub.transports.tcp.listen）
sclient relay --transport tcp --hub 127.0.0.1:18084 --local http://127.0.0.1:8080 --node-id my-node
# QUIC 中继（hub.transports.quic.listen，UDP 形态；自带 TLS）
sclient relay --transport quic --hub 127.0.0.1:18088 --local http://127.0.0.1:8080 --node-id my-node
# 自定义 WS 升级路径（与服务端 hub.transports.ws.path 一致；被动伪装层形态对齐）
sclient relay --hub ws://hub.example.com --ws-path /api/v1/stream --local http://127.0.0.1:8080 --node-id my-node
# 自定义 WS 升级附加校验头（与服务端 hub.transports.ws.upgrade_header 一致才连通）
sclient relay --hub ws://hub.example.com --ws-upgrade-header my-profile --local http://127.0.0.1:8080 --node-id my-node
```

作为中继节点连接到 Hub，注册自身节点标识，然后等待远程请求并通过隧道转发到本地
HTTP 服务。适用于跨网络服务暴露、集群间请求转发等场景。

**参数：**

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--transport` | `ws` | 连接到 Hub 的传输层：`ws`（默认，WebSocket）/ `tcp`（裸 TCP，`hub.transports.tcp.listen`）/ `quic`（QUIC UDP，`hub.transports.quic.listen`；UDP 形态抗 DPI 干扰，自带 TLS，客户端经 `SPROXY_QUIC_CA_CERT` 校验自签服务端证书） |
| `--hub` | `ws://127.0.0.1:18084/ws` | Hub 地址（ws 默认 `ws://127.0.0.1:18084/ws`；tcp 默认 `127.0.0.1:18084`；quic 默认 `127.0.0.1:18088`） |
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
  - `--smart`：**自动选最佳路由**——并行竞速「直连 / hub 中继 / 经中间节点多跳」三类候选，
    按端到端建连耗时择优（胜者缓存 TTL 30s 内单路复用；抖动链路可配 `--smart-ttl` 缩短缓存
    以更敏感重竞速）。候选展开：目标节点可达路径 + 每个可选中间节点（X）的
    `via-relay:X`（数据面经 hub 中继）与 `via-direct:X`（数据面 webrtc 直连 X，X 侧出口拨号）
    双候选。不做活跃连接实时迁移（新建连接时择优）；评分仅用建连耗时近似 RTT（不改 mux 核心）。
    **优雅降级**：竞速全部候选失败 / 无可选路径时，回退固定顺序 `webrtc → relay`（`mesh.Dial`），
    连接仍可用而非报错（`--smart` 默认关闭 = 现有固定顺序，零回归）。
  - `--quality-routing`：**传输质量感知选路（显式开关，默认关）**——与 `--smart` 搭配启用时，
    竞速候选按历史质量（mux 重传率，来自 `RegisterQualitySource` 注入的质量源）预排序：
    健康候选（零重传）优先启动，高重传候选**延迟启动**（避免劣化载体抢先），无历史候选中性
    不歧视。质量数据复用 #430 指标（`sproxy_mux_retransmits_total` 等计数器聚合）；
    开启时日志输出各候选质量分（可观测，`--verbose` 可见）。默认关 = 纯延迟竞速零回归。
  - `--e2e`：**端到端加密（显式开关，默认关）**——mesh.Dial 的 hub 中继数据面包
    `DialE2EStream`（ECDH 握手 + AES-256-GCM 字节流），中间节点 X / hub 只透传密文
    （即使持有 SproxySig SK 也读不到 L⇄T 明文，与 SK 解耦）。启用时输出实际加密模式
    （指纹 pinning 防 MITM / 纯 ECDH 防窃听）——**安全开关生效状态可观测，禁静默降级**。
    - `--e2e-identity <file>`：本端身份文件路径（默认 XDG 目录 `sproxy/identity.json`，
      经 `sclient identity generate` 生成；无身份文件 = 自动生成临时身份，纯 ECDH 防窃听）。
    - `--e2e-peer-fp <fp>...`：对端指纹白名单（可重复 / 逗号分隔，StringSlice）——非空时
      握手 fail-closed 校验对端指纹（**显式 pinning 防 MITM**）；空 = 纯 ECDH 防窃听
      （建议配置 pinning）。接线范围（#406/#408/#410 已全量落地）：hub 中继 L 直连 T、
      via-relay:X 多跳（X 中间节点纯字节泵透传，不见明文）、webrtc/mDNS 直连——`--e2e`
      覆盖全部选路路径；via-direct:X 出口拨号失败（X 未回结果帧）时 fail-closed 直接失败。
  - `--trust-x <node-id>...`：中间节点白名单（可重复 / 逗号分隔，StringSlice）。仅白名单内的
    X 生成 `via-relay:X` / `via-direct:X` 候选（信任收敛，减少攻击面）；空 = 全部有
    `outbound-dial` 能力的在线节点均可选（兼容现状）。
  - `--mdns` / `--mdns-secret`：纯 mDNS 局域网直连（不经 hub），经 mDNS 发现宣告该服务的
    mesh node（`mesh node --mdns`），直连信令建立 webrtc 数据面；`--mdns-secret` 为共享密钥
    （TXT 与信令均 HMAC 签名校验；为空 = 无认证 LAN 信任）。
    **身份双层（可选增强）**：配置身份（`sclient identity generate` / `NodeConfig.Identity`）后，
    mDNS TXT 广播携带身份指纹 `fp=`（指纹入 HMAC 签名内容防篡改），接受侧按白名单
    （`AllowedPeerFingerprints`）fail-closed 校验——缺 `fp=` / 不匹配即拒绝；未配置身份 /
    白名单时保持 LAN 信任（向后兼容）。
  - 竞速结果展示：`mesh connect` 建立后输出**实际路径**（`webrtc` / `relay` / `via-node` /
    `via-direct`）+ **建连耗时**（SmartDial 填充，端到端链路就绪）；单路径 Dial（未开
    `--smart`）仅显示路径不显示耗时（零回归）。`mesh status` 列 hub 服务状态，**不**展示
    竞速结果（连接时点信息无数据来源）。
  - `--virtual-subnet`：虚拟 IP 子网（需与 `hub.virtual_subnet` 一致，默认 CGNAT 100.64.0.0/10）；
    出口侧仅放行 `--service` 宣告端口或 `--vip-allow-port`（端口白名单红线）。
- `sclient p2p connect --peer <id> --tcp <addr>` — WebRTC 打洞直连对端
- `sclient socks -l :port --exit <node>` — SOCKS 代理出口
- `sclient udp map -l :udp --exit <node> --remote <host:port>` — UDP 端口映射
- `sclient mesh node ...` — 常驻 mesh 节点（自动对等发现 + 本地网关）
- `sclient mesh status` — 列出 hub 上的 mesh 服务（带虚拟 IP）；`--gateway <addr>` 改查本地
  mesh node 的网关拓扑
- `sclient mesh acl` — 列出**本 owner** 的跨节点授权（卷 × 节点 × `scope` = read/write/rw）；
  可见性由服务端按已认证身份判定（仅 owner 自身），故**没有** owner 参数（故意不给客户端指定他人的能力）。
  指纹不截断，便于与配置逐字对照
- `sclient mesh status --server` — 查询**服务端（sproxy）自身**的跨节点面/角色状态
  （`GET /api/mesh/status`）：只读/写面的实际监听地址与 pin 数、mesh node 角色是否**运行中**
  （配置启用但未启动会显式显示「未运行」）、hub 与信令开关。与 Web UI 的 Hub 面板状态卡同口径
  （同样不含任何秘密：指纹是公开标识）。

**UDP 映射（`sclient udp map`）的丢包与顺序语义**

映射的出口写在 leaf 侧**异步且有界**（同时在途写上限 64 条，与客户端 `udp map` 出口同值）：
在途写饱和时**丢弃该数据报**并计入 `sproxy_mux_datagram_handler_drops`；单次写失败同样丢弃
（只打 Debug 日志，不单独计数）。二者都不会阻塞该映射所在 mux 上的其它流。另外**不保证同一
flow 的数据报到达顺序**（UDP 本身不保证有序）。需要严格保序的协议请改用 TCP 路径（如
`sclient mesh connect <service>`、`sclient p2p connect --peer <id> --tcp <addr>` 或
`sclient socks -l :port --exit <node>`）；注意 `sclient tunnel` 是**HTTP 请求隧道**，不是 TCP 端口映射。

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

### socks

```bash
sclient socks -l :1080 --exit <node> [--socks-user u] [--socks-pass p]
```

启动本地 **SOCKS5 代理**（`--socks5-hostname` 域名直通，目标写进 dial 帧经 mesh 到出口）：

- 监听默认 `127.0.0.1`（裸 `:port` 归一）；
- `--socks-user`/`--socks-pass` 配置后要求 RFC 1929 认证；
- 支持 `--route` 分流（见下）、`--gateway`、`--smart`/`--smart-ttl`、`--mdns`/`--mdns-secret`、
  `--hub`/`--node-id`/`--insecure`、`--stun`/`--turn` 族、`--turn-rest` 族；
- 安全边界：SSRF 边界在出口节点 dial 策略（内网/loopback 目标默认拒绝，除非出口宣告该服务）。
- **出口节点组**（socks/udp/http-proxy 通用）：`--exit-group node-a,node-b,...` 指定出口组；
  `--exit-group-mode` 选负载均衡模式（默认 `failover` 按序，`round-robin` 轮询，`weighted` 加权）；
  `--exit-group-weight node:weight,...` 配合加权模式（如 `--exit-group-weight a:3,b:1`，仅 `weighted`
  模式有效，缺失/长度不足自动等权回落并告警）。模式只影响**每连接起点选择**：无论模式，选中节点
  失败都会循环尝试组内其余节点（failover 兜底，模式切换不牺牲可用性）。

**`--route` 分流规则（socks / http-proxy / udp map）**

```bash
sclient socks -l :1080 --exit node-default \
  --route .example.com=node-a,node-b \
  --route 10.0.0.0/8=node-intranet
```

- 规则格式 `domain|cidr=exit-group`：`--route` 可重复/逗号分隔多规则，按**声明序首个命中**；
- 域名后缀匹配（`*.example.com`/`.example.com`/`example.com` 同义，子域名命中，不依赖 DNS）；
  `cidr` 网段匹配（`netip` 解析，IPv4/IPv6 均可）；
- 命中 → 走该出口组（本地直连优先 + 组内 failover，同 `--exit-group` 语义）；**未命中 → 回落默认出口**
  （`--exit`/`--exit-group`/`--exit-auto`/本地直连）；
- 非法规则（无 `=`/空组/非法 CIDR）启动即报错（fail-closed，不静默忽略）；
  `--route` 与 `--exit-only` 语义冲突（恒经出口时分流无意义）→ 拒绝；与 `--exit`/`--exit-group`/`--exit-auto` 可共存；
- `udp map` 是**单 mux 固定出口**：命中规则时替换出口节点为组内第一个（组内 failover 不适用）；
- 规则数量建议保持小规模（每连接 O(规则数) 线性匹配，开销可忽略）。

```bash
sclient udp map -l :5300 --exit <node> --remote <host:port>
```

UDP 端口映射：本地 UDP 数据报经 mesh（mux FrameDatagram）到出口节点，出口转发到
`--remote` 目标；目标响应原路回传（双向 UDP 转发）。

- 出口仅转发到 `--remote` 指定地址，且需通过出口节点拨号策略（默认仅公网 + 宣告的服务地址）；
- 丢包与顺序语义：出口写在 leaf 侧**异步且有界**（同时在途写上限 64 条），在途写饱和时丢弃
  并计入 `sproxy_mux_datagram_handler_drops`；不保证同一 flow 数据报到达顺序（UDP 语义）。
  需要严格保序的协议请改用 TCP 路径（`mesh connect` / `socks` / `p2p connect`）；
- 与 socks 相同的 TURN / mDNS / hub 参数组。

### mesh

```bash
sclient mesh connect <service> [-l :port]   # 按服务名/虚拟 IP 连接（webrtc 直连优先，hub 中继回落）
sclient mesh node ...                        # 常驻 mesh 节点（注册 + 中继 + webrtc 直连 + 自动重连）
sclient mesh status                          # 列出 hub 上的 mesh 服务（带虚拟 IP）；--gateway 改查本地网关拓扑
sclient mesh status --server                 # 查询服务端自身的跨节点面/角色状态（GET /api/mesh/status）
sclient mesh acl                             # 列出本 owner 的跨节点授权（卷 × 节点 × scope）
```

- `mesh connect <service>`：`--smart` 并行竞速「直连 / hub 中继 / 经中间节点多跳」择优（胜者缓存 TTL 30s）；
  `--mdns` 纯局域网直连；`--virtual-subnet` 虚拟 IP 子网（默认 CGNAT 100.64.0.0/10，需与 hub 配置一致）；
  无 `-l` 时为单次 stdin/stdout 模式。
- `mesh node`：单进程常驻（稳定 node-id + 服务宣告 + per-node secret），并行提供经 hub 中继与
  WebRTC 直连，断线指数退避重连；`--service name:addr` 宣告服务（`mesh connect` 可发现）、
  `--dial-allow` 开启出口拨号（mesh connect 恒发 dial 帧，依赖此开关）、`--socks` 本地 SOCKS5 出口、
  `--discover` 自动对等发现（full-mesh 拓扑）、`--mdns` 纯 mDNS 局域网模式。
- 安全边界：hub 注册准入由凭据 Ring 的 SproxySig AK + HMAC proof 提供；`--dial-allow-cidr` 显式放行网段，
  默认仅公网目标；`--service` 精确放行宣告地址（防 SSRF）。

### http-proxy

```bash
sclient http-proxy -l :1080 [--exit <node>] [--exit-auto] [--exit-exclude <id>[,<id>...]]
                    [--exit-only] [--local-timeout 3s] [--proxy-user u] [--proxy-pass p]
                    [--route <domain|cidr>=<exit-group>...]
```

启动本地**正向 HTTP 代理**（标准代理，绝对 URI + CONNECT）：任意程序配
`http_proxy` / `https_proxy` / `no_proxy` 环境变量即用（curl/wget/浏览器/Git/Go·Python·Node 应用），
HTTPS 走 CONNECT 隧道（端到端 TLS，代理不可见明文）。

- **路由**（本地直连优先）：网络良好时本地直连目标（零 mesh 开销）；本地失败/超时（被墙/网络差）
  自动回退出口节点：`--exit <node>` 指定固定出口，`--exit-auto` 自动从 hub 节点列表选出口
  （`outbound-dial` 能力优先，候选 failover）。`--exit-auto` 配合 `--exit-exclude` 排除某些节点作为
  出口（逗号分隔 node-id，可多次）——被排除节点**仍可被 `--smart` 选为中转中间节点**（能中转但不出站）；
  `--exit` 固定节点时 `--exit-exclude` 无意义（fail-closed 报错）。`--exit-only` 强制恒经出口；无 `--exit`/`--exit-auto` 时
  恒本地直连（本机出口语义）。`--local-timeout` 控制本地直连探测超时（默认 3s）。
- **上游代理**（线路B）：`--upstream-proxy http://user:pass@host:port`——本地 mesh 拨号失败/超时后
  自动经上游 HTTP 代理 CONNECT 转发（本地直连不通时经国内服务器→新加坡出口）。数据面目标 TLS
  端到端加密；上游控制面 CONNECT 带 Basic 认证。
- **路由追踪**：`--mesh-route-header`（响应加 X-Mesh-Path 头：`<exitNode>|<kind>|<e2e>`）+
  `--mesh-trace-header`（X-Mesh-Trace 请求级追踪 ID）——绝对 URI 转发 Debug 日志带 route/trace；
  CONNECT 隧道仅日志（数据面透明无法插头）。默认关（拓扑不泄露给客户端）。
- **认证**：`--proxy-user`/`--proxy-pass` 任一配置即启用 `Proxy-Authorization: Basic` 校验
  （未认证回 407）；监听默认 `127.0.0.1`（裸 `:port` 归一，LAN 暴露需显式监听地址）。
- 与 socks 相同的 TURN / mDNS / hub / `--gateway` / `--smart` 参数组。

### cloud-download

```bash
sclient cloud-download <url> [url...]        # 链式：提交→等待→打包→下载→清理
sclient cloud-download --url-file <file>     # 每行 URL 或 URL<TAB>FILENAME
```

通过 sproxy 服务端从外部 URL 下载文件（服务端异步执行，落服务端存储后打包 tar.gz 下载到本地）：

- `--keep-files` 跳过清理；`--output-dir` 本地输出目录；`--archive-name` 归档名；
- 子命令分步：`submit` / `wait` / `archive` / `download` / `download-archive` / `delete` / `list` /
  `cancel` / `resume-chain` / `resume-download` / `group`（任务组）/ `resume`；
- 任务状态：`pending | downloading | completed | failed | cancelled`；失败自动重试（`cloud_max_retries`）。

**归档转存（统一转存）**：`cloud-download archive` 与 `cloud-download-group archive` 支持
`--target <卷 URI 路径>`（cd/mv 同规约，`<卷名>:/<路径>`）——打包后转存到该卷（**任意存储卷，含
本地卷**——统一卷模型 U3；本地卷 target 产物落 `<owner>/user/<rel>`、可经 `/download` 命中），不落服务端
archive 桶；不带卷名的纯路径回落**当前工作目录**（`cd <卷名>:/<目录>`，与 cp/mv/delete 同语义：
相对路径拼当前目录，前导 `/` 绕过当前目录，`..` 拒绝）。可选 `--encrypt`（流式加密，密钥来自
服务端 `archive.key_file`，产物为 `<名>.tar.gz.aes`）、`--cipher`（默认 aes-256-gcm，**须与
`--encrypt` 同用**）、`--compress`（gzip 默认 / zstd / brotli）——均仅与 `--target` 同用
（否则报错，不静默忽略）。未指定 `--target` 时行为不变（落 archive 桶）。CLI 仅把 URI 归一为
请求体的 `target_volume`/`target_path`，协议零改动：

```bash
sclient cloud-download archive <task-id> --target vault:/backups/a.tar.gz   # 指定文件路径
sclient cloud-download archive <task-id> --target disk2:/                    # 目标是目录（默认文件名）
sclient cloud-download archive <task-id> --target vault:/b/a.tar.gz --encrypt --compress zstd
cd vault:/backups && sclient cloud-download archive <task-id> --target a.tar.gz  # 当前目录内（= vault:/backups/a.tar.gz）
sclient cloud-download-group archive <group-id> --target vault:/backups/     # 组归档转存
```

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

### upgrade

```bash
sclient upgrade                 # 升级到最新版本（已最新则提示）
sclient upgrade --check          # 只检查最新版本，不安装
sclient upgrade --to v0.18.0     # 升级到指定版本
sclient upgrade --force          # 跳过版本比较强制升级
```

从 GitHub Releases 拉取最新（或 `--to` 指定）版本发布，按 `GOOS/GOARCH` 匹配归档
（`sproxy_<ver>_<GOOS>_<GOARCH>.tar.gz/.zip`），经 `checksums.txt` SHA-256 校验后
解包替换自身二进制（临时文件 + 原子 rename；Windows 下若 exe 被占用，先写
`.upgrade.bat` 两段式兜底，提示退出后运行完成替换）。

- `--check`：只查询不安装，**恒 exit 0**（脚本解析 `--json` 的 `update_available` 字段）
- `--to <ver>`：指定目标版本（不存在时报「版本不存在」）
- `--force`：跳过版本比较（当前版本为快照/脏构建无法判定时必用）
- `--json`：输出 `current/latest/update_available/action/path` 机器可读字段
- 当前版本无法解析（SNAPSHOT/dirty）时 `--check` 提示「无法判定」，升级需 `--force`

### search

```bash
sclient search <keyword>
```

- 递归搜索文件名中包含 `<keyword>` 的文件（不区分大小写）
- 关键字为空时返回空列表
- 输出格式与 `list` 相同，包含 name、size、checksum、mod_time、is_dir

### du

```bash
sclient du [path]
```

- 查看目录空间占用（递归统计：文件数/字节/子目录数）
- 缺省 path = 当前目录；受 `cd` 与 `--vol` 影响
- 输出人类可读字节；`--json` 输出机器可读对象

### df

```bash
sclient df
```

- 查看磁盘与配额水位（复用服务端 `/api/stats`）
- 配额受限时显示已用/上限/水位百分比；未受限显示已用/不限
- `--json` 输出原始统计对象


### quota

查看本 owner 的配额。**权威源是跨卷联邦视图**（片 5，设计
`docs/designs/2026-10-16-quota-volume-layer.md` §1.5）：用户配额**按卷持有** ⇒ 用户视图 =
对「该用户可访问的卷」**求和**（只含 ACL 允许的卷，不泄露他人卷用量）。

```bash
sclient quota          # 汇总 + 逐卷明细
sclient quota --json   # {"quota": <集中树水位>, "federated": <跨卷视图>}
```

输出示例：

```
已用 2.9 KiB / 上限 8.0 KiB
  v1  1000 B / 4.0 KiB
  v2  2.0 KiB / 4.0 KiB
```

语义要点：

- **未知 ≠ 0**：某卷无每用户配额权威（未装配配额层 / 该 owner 未登记）时显示
  `未知（该卷无每用户配额权威）`，**不会**显示成「已用 0」；此时汇总带 `提示：……汇总**不完整**`。
- **上限 0 = 不限**（不是「零配额」）：显示 `/ 不限`，汇总带 `（含不限卷）`。
- `--json` 的 `quota` 键是**旧集中树**水位（`/api/stats`，向后兼容保留），`federated` 键是
  **跨卷权威视图**；服务端无 `/api/quota`（旧版本）时向 stderr 告警并回落集中树视图。

### batch

```bash
sclient batch <file>
sclient batch --workers 8 ops.txt
```

- 从文件逐行读取命令并批量执行（每行一个完整子命令调用，如 `delete dir/a.txt`）
- `--workers` 并发数（默认 4；`0/1` 串行退化；超过行数时按行数钳位）
- 空行与 `#` 注释行跳过；结果按文件行序输出（与逐行执行逐字节一致，脚本兼容零回归）
- 任一行失败时命令以非 0 退出码结束
- 当前支持子命令子集：`delete` / `mkdir` / `rmdir` / `meta`（其余子命令名返回显式 FAIL）

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

### volume

```bash
sclient volume create <name> --type baidupcs --extra '{"bduss":"...","baidu_root":"/disk1"}' [--capacity 100GiB]
sclient volume list
sclient volume delete <name>
sclient volume copy <file> --from-volume <a> --to-volume <b>
sclient volume move <file> --from-volume <a> --to-volume <b>
sclient volume rebalance --from-volume <a> --to-volume <b> [--max-bytes <n>]
```

- 管理当前用户的**用户自有卷**（网盘盘，仅外部类型）：create 创建 / list 列出我的 / delete 删除
- 卷间操作：`copy` 跨卷复制（保留源）、`move` 跨卷移动（源被移除）、`rebalance` 卷再平衡（异步提交）
- `copy`/`move` 的 `--from-volume` 缺省取当前 `--vol` 卷上下文；`--to-volume` 必填
- `--type` 后端类型（如 `baidupcs` / `webdav`，须服务端已注册 backend）；`--extra` 类型特有配置 JSON
  （baidupcs 类型支持：`bduss` 登录凭据、`baidu_root` 网盘根路径（空 = `/`）、`binary_path`
  BaiduPCS-Go 路径（空 = PATH 查找）、`local_root` 本地中间态基目录（空 = 默认）；
  webdav 类型支持：`url` WebDAV 根 URL（必填，如 `https://nextcloud.example.com/remote.php/webdav`）、
  `username` + `password`（Basic 认证）或 `token`（Bearer，优先））
- `--capacity` 可选容量上限（人类可读大小如 `100GiB`，缺省 0 = 不限制；独立卷容量，不计 owner 配额）
- `volume list` 输出 name/type/capacity 表格，`--json` 输出机器可读
- `volume delete` 删除用户卷：被**活跃同步任务引用**时服务端返回 409（需先取消任务）
- 用户卷寻址：同步任务 `remote.volume` 填用户卷名，任务 owner 必须匹配卷 owner（跨用户 404 防枚举）

### backup

```bash
sclient backup <vol> <dest>
sclient backup <vol> -o <path>
sclient backup "" all-volumes.tar    # 导出全部可见卷
```

- 把卷导出为 tar 备份到本地（服务端 `GET /api/volumes/export` 流式导出）
- `<vol>` 指定卷名（留空 = 导出当前凭据可见的全部卷）；`<dest>` 本地目标 `.tar` 文件路径（父目录自动创建，原子落盘）
- 导出 tar 含全部文件内容 + 尾部 `manifest.json`（每条目相对路径 + SHA-256 + size + mtime + 台账交叉校验）
- 配合服务端 `POST /api/volumes/import` 可跨实例迁移 / 恢复

### migrate

```bash
sclient migrate export <out> [--yes]
sclient migrate import <in> [--yes] [--ignore-errors] [--no-verify]
sclient migrate mirror-config <in> [--print] [--write <path>]
```

迁移向导（roadmap 11.7-⑩）：单机 → 多卷 → 联邦的自动化迁移。

- `export <out>`：递归列出源机全部文件（含子目录）→ 下载到 `<out>/files/`（相对路径保持）→ 逐文件校验 SHA-256 → 原子写 `<out>/manifest.json`（schema:1 + server 引用 + 文件条目）
- `import <in>`：读 `<in>/manifest.json`（schema/checksum 自检，损坏/不符拒绝导入）→ 逐文件上传 → 目标 stat/checksum 复核；同名同 checksum 跳过（幂等），不同 → CONFLICT 报错；默认快速失败，`--ignore-errors` 跳过继续
- `mirror-config <in>`：读 manifest + 目标机卷列表（`GET /api/volumes`）→ 生成 `volumes[].mirror_to`（多卷冗余）/ `sync_remotes`（源机为 direct 同步远端）/ `federation.peers`（源机为联邦对端）片段 YAML，默认打印（`--print`），`--write <path>` 落盘
- 通用 flags：`--yes`（跳过交互确认，纯脚本模式）、`--ignore-errors`、`--no-verify`（跳过导入后校验，慎用）
- 迁移期间源端变化：manifest 记录 mtime/checksum，导入后校验发现差异 → 提示重导该文件（幂等重跑即可收敛）

### sealfile

```bash
sclient sealfile encrypt <file> [--key <密钥>] [--owner <owner>] [--output-dir <目录>]
sclient sealfile decrypt <输出目录> [卷内文件名] --key <密钥> [-o 还原文件]
sclient sealfile info <输出目录> [文件名] --key <密钥>
```

本地文件加解密（**不连服务端**），与 sproxy `secretdata` 卷**同一写内核**（`secretdata.NewFS`）：
加密产物可直接作为 `type: secretdata` 卷根加载到 sproxy，播放器 Range 直接播放。

- **encrypt**：加密源文件到输出目录。产物为 secretdata 卷布局（逻辑键 `<owner>/user/<名>`，
  磁盘为随机加密容器）。视频文件缺省按扩展名自动关键帧分块（MP4 走 go-mp4、其它走 ffprobe
  兜底，与 sproxy 完全同款）；`--video-keyframe` / `--fixed` 显式覆盖（互斥）。输出打印
  blocklet 模式（关键帧是否生效的可观测性）。
- **decrypt**：整文件解密还原（含 SHA-256 全量校验，fail-closed）。位置参数 1 = 产物目录，
  位置参数 2 = 卷内文件名（**短名**，缺省取 `<owner>/user/` 下唯一文件）；`-o/--output` 指定
  还原路径（缺省 = 产物目录父目录/<短名>）。
- **info**：查看产物卷信息（文件列表 / 大小 / sha256 / 分块数 / blocklet 模式）。
- **--key**：64 hex 字面量或 secrets 卷密钥文件路径（文件内容**原样字节**作 secret——与
  `secret_url` 读取同一语义，不要求 hex）。缺省读配置 `sealfile.key`。
- **--owner**：逻辑 owner（默认 `anonymous`）。产物布局 `<owner>/user/<名>` 对齐服务器键空间
  （`Location.FSPath` 恒 owner 前缀）。**注意**：签名请求的 owner 即 **AK 字符串**（非凭据
  Ring 的 Owner 字段），加密产物要被某请求命中须用对应 AK；未认证请求用 `anonymous`。
- **加载到 sproxy**：产物目录配置为 `type: secretdata` 卷（`extra.root` 指向产物目录 +
  `secret_url` 指向同一密钥文件），播放器直接 Range 播放（`/download?filename=<名>&volume=<卷名>`）。

**配置段（config.yaml 顶层）**：

```yaml
sealfile:
  key: "..."            # 密钥：64 hex 字面量或密钥文件路径（原样字节）
  algorithm: ""         # 加密算法（缺省 shardseal 默认）
  block_policy:         # 分块策略（同 secretdata extra.block_policy）
    mode: random
    min: "1MiB"
    max: "200MiB"
  out_dir: "./sealed"   # 输出目录（encrypt 产物 / info 定位）
  owner: anonymous      # 逻辑 owner（默认 anonymous）
```

优先级：CLI flag > 配置 > 默认。

### share

分享链接管理。**分享源可以是外部卷/加密卷**（如 `secretdata` vault）：卷定位走卷 URI、
`cd` 设定的工作目录卷，或（都不给时）服务端**自动定位**（本地卷优先，未命中再扫外部卷）。

```bash
sclient share create vault:/docs/report.pdf        # 卷 URI（推荐：多卷同名时语义确定）
sclient share create docs/report.pdf               # 用 cd 的工作目录卷
sclient share create report.pdf --ttl 168h --one-time
sclient share list                                 # 输出含 VOLUME 列（源卷）
sclient share revoke <token>
```

- 卷优先级：**位置参数卷 URI** > **`cd` 工作目录卷** > 服务端自动定位（不带 `volume` 字段）。
- 分享记录**固化源卷**：创建后即使同名文件出现在别的卷，链接仍指向创建时的卷。
- 外部/加密卷分享访问时由服务端**透明解密**返回明文，`Content-Length` 为明文长度。

### trash

回收站管理。**每卷独立回收站**（软删落文件所在卷）；列表默认**汇总** owner 可见的全部卷，
每条带 `VOLUME` 列。

```bash
sclient trash list                     # 汇总（全部卷）
sclient trash list --volume vault       # 只看某卷
sclient trash restore <trash_rel>       # 缺省跨卷定位
sclient trash restore <trash_rel> --volume vault
sclient trash empty --yes               # **只清默认卷**（安全默认，不静默扩大）
sclient trash empty --yes --volume vault
sclient trash empty --yes --all         # 清全部卷（含外部/加密卷）
```

`--all` 与 `--volume` 互斥；`empty` 必须带 `--yes`。

## 常见错误排查

| 现象 | 可能原因 |
|---|---|
| `路径包含父级引用 '..'` | 客户端预拦截了不安全路径，去掉 `..` 或用绝对路径 |
| `tunnel error (HTTP 401)` | 外层 `POST /tunnel` SproxySig 验签失败（凭据缺失/非法/过期/重放），检查 `access_key`/`access_key_secret` 是否与服务端凭据 Ring 一致 |
| `tunnel error (HTTP 400)` | 隧道密钥与服务端不一致，或网络中间层破坏了请求体 |
| `unauthorized` (401) | SproxySig 签名缺失/非法/过期（服务端凭据 Ring 非空时），检查 `~/.config/sproxy/config.yaml` 当前 context user 段的 `access_key`/`access_key_secret` 是否与服务端一致（或 `sclient trust login` 重新绑定） |
| `源文件 SHA-256 校验失败` | mv 期间本地文件已变，刷新本地 checksum 后重试 |
| `文件已存在但 checksum 不匹配` (409) | 服务端已有同名文件且内容不同，先 mv 或 delete |
