# FTP 外部后端补设计（roadmap 12.2-1）

> 日期：2026-09-27 ｜ 状态：补设计（#617 已实现无设计文档，按 roadmap 12.3-14「已实现无设计文档的能力面须补设计」回补）
> 关联：roadmap 12.2-1（FTP 后端补设计，P2，0.5 人日）
> 实现：PR #617（`4f0bca655 feat(volumes): FTP 外部后端——sync.FS 实现 + 注册`）
> 范围：纯文档，不改代码

## 一、背景与目标

### 1.1 现状实证（源码）

FTP 外部后端已实现并合入（PR #617），文件位于 `pkg/volume/ftp/`（4 文件，~700 行）：

| 文件 | 内容 | 关键位置 |
|------|------|----------|
| `ftp_backend.go` | V3 插件注册 + 配置解析 + ExternalBackend 包装 | `ftpExternalBackend`（L24）、`newFTPBackend`（L48）、`RegisterFTPBackend`（L87，sync.Once） |
| `ftp_fs.go` | sync.FS 客户端实现（7 方法 + Close/Ping） | `ClientConfig`（L28）、`FTPFS`（L40）、`NewFTPFS`（L50）、`login`（L87）、`openData`（L175，PASV）、`ListDir`（L294）、`Stat`（L371）、`WriteFile`（L422）、`Rename`（L459）、`Delete`（L474）、`MakeDir`（L487） |
| `ftp_backend_test.go` | 注册 + 配置校验（fail-closed）+ HealthProbe 编译断言 | 3 个测试 |
| `ftp_fs_test.go` | 纯标准库内存 fake FTP 服务端 + 协议语义测试 | fakeFTPServer + 8 个测试 |

装配点：`cmd/sproxy/root.go:600` `ftp.RegisterFTPBackend()`（SyncManager 装配分支内，与 webdav/sftp/s3/baidupcs/federated 同构）。

### 1.2 定位

FTP 后端与 sftp/webdav/s3/baidupcs/federated 并列，是**第六个真实外部卷后端**（V3 通用卷模型）：
把任意 FTP 服务（NAS、企业遗留 FTP 服务器等）适配为 `pkg/sync.FS`（7 方法），经
`registry.RegisterBackend("ftp")` 可插拔接入系统盘（`volumes[] type=ftp`）与用户卷（U1 运行时注册）。
能力边界：**只做客户端适配**（sproxy 是 FTP 的客户端），不提供 FTP 服务端。

### 1.3 目标

本文档回补该能力面的设计依据：FTP 协议语义（被动模式/路径/错误码）、并发与超时模型、
fail-closed 面、测试覆盖现状与缺口、后续片划分。**不改代码**（#617 已合入且功能稳定）。

## 二、组件

### 2.1 `ftpExternalBackend`（ftp_backend.go，注册 + 装配）

```
ftpExternalBackend { fs *FTPFS }
├── FS() syncpkg.FS      // 返回 FTPFS 同步视图（供 syncexec 工厂按 remote.volume 查询）
├── Close() error        // 委托 fs.Close()（QUIT + 关控制连接，幂等）
└── Ping(ctx) error      // 实现 registry.HealthProbe（NOOP 探测，驱动卷状态 healthy/degraded）
```

- 编译期断言：`_ registry.ExternalBackend`、`_ registry.HealthProbe`（ftp_backend.go L35-37）。
- `newFTPBackend(ctx, v volume.Volume)`（L48）从 `v.Extra`（map[string]any，值须为 string）读配置：

| Extra 键 | 必填 | 语义 | 校验（fail-closed） |
|----------|------|------|---------------------|
| `url` | ✅ | `ftp://user@host:port[/root-path]` | 非空；`url.Parse` 成功；scheme == `ftp`；`u.User.Username() != ""`；`u.Host != ""` |
| `password` | ✅ | 密码认证 | 非空（**FTP 无匿名目标**，与 sftp 二选一不同——FTP 必须密码） |
| `root` | ❌ | 远端根目录（默认服务器登录目录） | 透传 NewFTPFS |

- `RegisterFTPBackend()`（L87）用 `sync.Once` 保证只注册一次（多装配/多测试并发调 runServer
  防 registry 重复注册 panic）；`registerFTPBackendWithFactory(typ)` 允许测试用唯一类型名注册。

### 2.2 `FTPFS`（ftp_fs.go，sync.FS 客户端）

**并发模型（核心设计决策）**：

- **每实例独立控制连接**，无共享连接池（仓库硬规则 17：禁共享 `http.DefaultClient`/共享连接池的
  同类要求——FTP 控制连接命令串行执行，跨实例共享会串扰命令/响应配对）。
- 单实例内控制连接命令经 `mu sync.Mutex` 串行化（`exec` L162）；**数据连接（PASV）逐命令建立**，
  不缓存（FTP 无标准连接复用协议，逐命令建连最简单可靠）。
- `closed bool` 标志 + `exec/openData` 前置检查：Close 后一切操作 fail-fast 报「连接已关闭」。

**FTP 主动/被动模式**：

- **只实现被动模式（PASV，RFC 959）**（`openData` L175）：客户端发 `PASV` → 服务端 227 回
  `(h1,h2,h3,h4,p1,p2)` → `parsePASV` 解析（L238）→ 客户端主动拨数据连接。**不实现主动模式**
  （PORT）——sproxy 是服务器侧客户端，通常处 NAT/防火墙后，被动模式是标准选择。
- **EPSV 未实现**（IPv6 场景缺口，见测试面/片划分）。
- **PASV 回退策略**（L195-204）：服务端常回 `0.0.0.0`/`127.0.0.1`（NAT/网关简化实现）；此时若控制
  连接目标（`dialHost`）是真实非回环 IP，回退用控制主机地址——与 curl/lftp 同策略。
- 数据连接拨号超时 30s（L221）；数据连接读取无显式超时（流读，见风险）。

**TLS/FTPS**：

- **未实现**（无 `AUTH TLS`/`AUTH SSL` 隐式显式协商、无 `ftps://` scheme）。当前仅明文 FTP。
- fail-closed 面：url 强制 `ftp` scheme（`ftps://`/`sftp://` 直接构造失败，不静默明文回落）。
- 建议片 2 补 FTPS（见 §6）。

**路径语义**：

- FS 相对路径（正斜杠、无根前缀）→ `abs(rel)`（L279）映射远端绝对路径：
  `root == ""` → `"/" + rel`（服务器登录目录为根）；否则 `"/" + root + "/" + rel`。
- 根用 `url.Path`（去前导 `/`）或 `Extra.root` 归一（L82-85）。
- `ListDir` 返回条目的 `Path` 为**完整相对路径**（L310-318，拼接 base + 子名）——对齐
  `pkg/sync.FS` Path 契约（审查 R3：引擎递归/rename 依赖完整相对路径）。
- 路径穿越防御：**FS 路径本身由上层（ValidateFilePath 等）把关**；FTP 层 `abs` 只做拼接。
  相对路径中的 `..` 段会原样进入 FTP 命令（CWD ../ 等）——这是与本地卷的差异面（见 §5 缺口）。

**超时**：

| 点 | 值 | 位置 |
|----|-----|------|
| 拨号（控制连接） | `DialTimeout`，默认 10s（0 = 默认） | L58-60 |
| 登录握手 | 同拨号 deadline（`SetDeadline(now+timeout)`，登录后清除） | L64-66 |
| 数据连接拨号 | 固定 30s | L221 |
| 尾部响应消费 | 5s 有界读（`consumeTailReply`） | L228-233 |
| 控制连接命令响应 | **无超时**（读阻塞到服务端响应/断连） | exec/readReply |

**协议实现细节**：

- `readReply`（L134）：单行/多行响应（RFC 959，`-` 分隔，末行 `code ` 结尾）。
- `login`（L87）：`220` → `USER` → `331` → `PASS` → `230` → `TYPE I`（二进制，LIST/RETR/STOR 数据面）。
- `consumeTailReply`（L226）：数据命令尾部响应（226）消费；注释明确**不能发 NOOP 代读**
  （NOOP 自身回复会滞留控制流，导致下一命令响应错位）。
- `Close`（L255）：发 QUIT 再关连接，幂等。

### 2.3 sync.FS 7 方法实现

| 方法 | FTP 命令 | 语义要点 |
|------|----------|----------|
| `ListDir` | `LIST <abs>` | 单层不递归；数据连接读尽 → `parseList`（类 Unix 行：`d` 目录 / `-` 文件 / `l` 链接）；结果按 Path 排序 |
| `Stat` | `CWD <abs>`（目录）/ `SIZE <abs>`（文件） | 不存在 → `(nil, nil)`；目录用 CWD 250 探测、文件用 SIZE 213 |
| `OpenRead` | `RETR <abs>` | 返回 `dataReadCloser`（Close 关数据连接 + 消费尾部响应，once 幂等） |
| `WriteFile` | `ensureParentDirs`（MKD 逐级幂等）+ `STOR <abs>` | 全量覆盖写；`mtime` 忽略（FTP 无标准 mtime 设置，服务端定） |
| `Rename` | `RNFR <from>` → 350 → `RNTO <to>` → 250 | 两步命令，中间态由协议保证 |
| `Delete` | `DELE <abs>` | **550（不存在）→ 幂等 nil** |
| `MakeDir` | `MKD <abs>` | **550（已存在）→ 幂等 nil** |

## 三、数据流

```
sproxy 配置（volumes[] type=ftp / 用户卷 U1）
  → 装配层（cmd/sproxy/root.go）
      ├─ ftp.RegisterFTPBackend()（sync.Once 注册工厂到 registry.backendFactories）
      └─ registry.NewBackend(ctx, v) → newFTPBackend
           └─ 读 v.Extra{url,password,root} → 校验（fail-closed）
                └─ NewFTPFS(ClientConfig) → 拨号 → login（220/USER/331/PASS/230/TYPE I）
                     └─ ftpExternalBackend{fs} → 持有在 Set.external[volName]
  → 运行时消费
      ├─ syncexec 工厂：remote.volume 查 Set.External(volName) → FS() 同步视图
      │    ├─ 读路径：WalkEntries → ListDir/Stat/OpenRead
      │    ├─ 写路径：WriteFile/Rename/Delete/MakeDir（引擎递归 + 冲突重命名）
      │    └─ 健康探针：Ping（NOOP 200）→ GET /api/volumes state=healthy/degraded
      └─ Set.Close / RemoveExternalVolume：be.Close() → fs.Close()（QUIT 幂等）
```

单次文件读（RETR）时序：

```
exec("PASV") → 227 解析 (host,port) → [0.0.0.0/127.0.0.1 回退 dialHost]
  → exec("RETR <abs>") → 150/125 → net.DialTimeout(数据连接, 30s)
  → 读数据连接至 EOF → Close 数据连接 → consumeTailReply（226，5s 有界读）
```

## 四、错误处理

### 4.1 构造期（fail-closed）

| 场景 | 行为 |
|------|------|
| `v.Type` 空/`local` | 拒绝（外部卷才走后端分派） |
| 缺 `extra.url` / url 非法（scheme≠ftp / 无 user / 无 host） | 构造失败，错误含卷名与期望格式 |
| 缺 `extra.password` | 构造失败（**FTP 无匿名目标**，不回落匿名） |
| 拨号失败 / 握手非 220 / USER 非 331 / PASS 非 230 | `NewFTPFS` 返回错误并关连接 |

### 4.2 运行期错误 → 错误语义

| FTP 码 | 场景 | 客户端行为 |
|--------|------|-----------|
| 421 | 服务忙/关闭控制连接 | 命令失败透传错误；后续命令因连接关闭 fail-fast |
| 425/426 | 数据连接建立失败/传输中断 | `openData` 报错；写路径 `io.Copy` 错误上抛 |
| 450/550 | 资源不可用/不存在 | 因操作而异：Delete/MakeDir → **幂等 nil**；Stat → `(nil,nil)`；Rename/WriteFile → 报错上抛 |
| 530 | 未登录（连接被服务端踢出） | 命令失败透传；无自动重连（见风险） |
| 非 2xx | LIST/RETR/STOR/PASV 被拒 | `openData` 统一「%s 被拒（%d）」上抛 |

**重连语义**：**无自动重连**。连接中断（网络/服务端 421/超时）后 FTPFS 进入不可用态，
后续操作报错；恢复依赖上层（同步任务失败 → SyncManager 重试 → 重新构造/查询卷）。
与 sftp（`pkg/sftp` 客户端无自动重连）同策略，属已知边界（片 2 候选）。

**fail-closed 面汇总**：
- 无匿名回落（密码必填）；无 FTPS 时 `ftps://`/`sftp://` scheme 直接拒绝（不静默明文降级）。
- Close 后所有操作 fail-fast；未知 LIST 行类型报错（不猜）。
- 卷不可达 → `Ping` 报错 → 卷状态 degraded 可观测（不假装 healthy）。

## 五、测试面

### 5.1 现有覆盖（ftp_backend_test.go + ftp_fs_test.go，全部 `t.Parallel()`）

| 测试 | 覆盖 |
|------|------|
| `TestFTPBackend_RegisterAndBackends` | 注册后 `registry.BackendTypes()` 含 `"ftp"`（GET /api/backends 数据源） |
| `TestFTPBackend_NewBackendConfigValidation` | 缺 url / 缺认证 / scheme 非 ftp / 本地卷类型 → 构造失败（fail-closed） |
| `TestFTPBackend_ImplementsHealthProbe` | 编译期断言 + Ping healthy |
| `TestFTPFS_WriteReadDelete` | Write→Read→Stat→ListDir→Delete 往返 + 中文内容 + size |
| `TestFTPFS_MakeDirRename` | 建目录（幂等）→ 写文件 → Rename → 根列表含目录 |
| `TestFTPFS_ListDir_CompletePath` | ListDir 返回完整相对路径（引擎递归依赖）+ 单层不递归 + 目录/文件标记 |
| `TestFTPFS_Stat_Missing` | 不存在 → `(nil, nil)` |
| `TestFTPFS_DeleteMissing_Idempotent` | 删除不存在 → 幂等 nil |
| `TestFTPFS_Ping` | NOOP 探针 healthy |
| `TestFTPFS_AuthFail` | 错误密码 → 构造失败（fail-closed） |

**测试基建**：`fakeFTPServer`（纯标准库内存 fake：文件/目录 map + 控制会话 + PASV/EPSV 数据连接，
只绑 127.0.0.1）。值得注意：fake 已实现 EPSV 分支但客户端未用；fake 先回 150 再 accept 数据连接。

### 5.2 缺口（建议补）

| # | 缺口 | 建议测试（TDD 红绿灯） | 优先级 |
|---|------|------------------------|--------|
| G1 | **连接中断重连语义** | 服务端关控制连接后操作 → 明确错误（非挂死）；断言无静默重连 | P1 |
| G2 | **路径穿越防御** | `relPath = "../x"` / `"a/../../b"` 断言 FTP 命令不含未预期 `..`（或上层拦截点文档化） | P1 |
| G3 | **FTPS 握手失败** | 对 TLS 服务端明文拨号 → 构造失败且错误明确（实现后） | P2（片 2） |
| G4 | **数据连接中断（426）** | RETR 中途断数据连接 → OpenRead/Read 报错（当前无测试钉住 426 路径） | P2 |
| G5 | **EPSV 优先** | 服务端仅 EPSV（IPv6/防火墙）时连接成功（当前只 PASV；fake 已有 EPSV 分支） | P2（片 2） |
| G6 | **被动模式回退** | 227 回 `0.0.0.0` 时回退控制主机拨号成功（fake 恒回 127.0.0.1，未覆盖回退分支） | P2 |
| G7 | **mtime 语义** | WriteFile 传 mtime 被忽略（文档化）；Stat.MTime 恒 0 的断言（当前 Entry.MTime 未填） | P3 |

变异点示例：删除 550→幂等 nil 分支 → DeleteMissing/Stat_Missing 红；删除 PASV 回退 → G6 红。

## 六、片划分与零回归

### 6.1 已落地（#617，片 1）

注册（sync.Once）+ 配置解析（fail-closed）+ FTPFS 客户端（PASV、TYPE I、7 方法、Close/Ping）+ 测试基建与语义测试。

### 6.2 建议片 2（不在 #617 范围，按需推进）

| 片 | 内容 | 验收 |
|----|------|------|
| **P2-1 连接健壮性** | G1（断连语义测试 + 文档化）、G4（426 路径）、G6（PASV 回退测试钉住现有实现） | 新增测试全绿 + 变异命中 |
| **P2-2 FTPS** | `ftps://` scheme + `AUTH TLS`（显式）/隐式 TLS 拨号；G3 测试；文档补 FTPS 配置 | 与真实 FTPS 服务（vsftpd/proftpd）E2E |
| **P2-3 EPSV/IPv6** | EPSV 优先 + IPv6 数据连接；G5 测试 | 双栈服务端往返成功 |
| **P2-4 配置文档化** | `docs/config.md` 补 FTP 后端配置节（对齐 SFTP 节 L697）+ `docs/api.md` backends 列表补 `ftp` | R15 门禁通过 |

### 6.3 零回归保证

- **纯文档 PR**（本文档）：不改任何代码 → 零回归；docs-only 通道（ci.yml paths-ignore 命中 docs/**）占位检查秒过。
- #617 已合入 master 且测试全绿；本文档只补设计依据，不改变已实现行为。
- 已知边界（如实记录，不承诺）：
  - 无自动重连（断连后依赖上层任务重试）；
  - 无 FTPS（明文 FTP；`ftps://` 构造拒绝，fail-closed）；
  - 控制连接命令响应无超时（对端挂死会阻塞该实例操作——单实例模型，不阻塞其它卷）；
  - `mtime` 不可设置（FTP 协议无标准机制；同步引擎对 FTP 目标不保留 mtime）。
