---
title: sproxy 完全组网·阶段 4·文件同步 / 复制设计
status: planning
---

# sproxy 阶段 4·文件同步 / 复制设计

> 日期：2026-08-30
> 来源：路线图（`docs/superpowers/specs/2026-08-29-sproxy-fullmesh-roadmap.md` §3 阶段4，P2，`pkg/server/transfer*` 未来）+ 阶段 3 复盘（§8 建议：先确认 transfer mgr 实际能力与 spec 状态再规划）
> 分支：feature/mesh-p4-filesync（建议）
> 前置：阶段 1–3 已合入（master `7048018`）；#115「传输管理器」（前端 Web transfer mgr + 服务端分块管线）已合入
> 修订：2026-08-30 v2——设计审查（C-2 空文件、I-4 deadline、I-5 Rename、I-6 模块边界、I-7 空目录/符号链接、I-9 并发模型、M-5/M-6/M-7）已融入
> 修订：2026-08-30 v3——用户决策：**v1 就要服务端任务**（SyncManager + 服务端 API + 前端 sync_task 频道），不再推迟到 v2；§1 范围/§AD-5/§4.4/§8 已更新

## 1. 目标

给 sproxy 提供**节点间文件同步 / 复制**能力：把文件/目录从一台 sproxy 节点复制到另一台节点（经 mesh 链路），复用 #115 合入的分块传输管线与「stat 校验续传」范式，在前端传输管理器中以独立任务类型（`sync_task`）呈现，并由服务端 `SyncManager` 托管任务生命周期。

第一版能力范围（v3，含服务端任务）：

1. `sclient sync push <path> --remote <node> --dst <path>`：本地目录/文件推送到远程节点；
2. `sclient sync pull --remote <node> --src <path> <dst>`：远程拉取到本地；
3. **服务端 SyncManager 托管同步任务**（`pkg/server/sync/`）：任务模型/状态机/持久化/并发/配额，照搬 CloudTask 生命周期；
4. **服务端 API** `POST /api/sync/tasks`、`GET /api/sync/tasks`、`GET /api/sync/tasks/{id}`、`POST /api/sync/tasks/{id}/cancel`、`DELETE /api/sync/tasks/{id}`（localMux + srvMux 双注册）；
5. **前端传输页 `sync_task` 频道**（transfer-store 增加 kind、app-render 渲染、传输页频道筛选）；
6. 目录递归 + 包含/排除过滤器；文件级增量（两端 checksum 相同则跳过）；
7. 冲突策略（skip / overwrite / last-writer-wins / conflict-rename）；
8. 分块续传（复用确定性 upload_id + 断点续传），进度/状态可见；
9. **空文件与空目录有明确策略**（v2，审查 C-2/I-7，见 AD-3）。

**非目标（第一版）**：双向同步（A↔B 同一目录自动双向）；块级增量（仅传差异块，需要两端块索引）。

## 2. 现状盘点（已核实源码）

### 2.1 #115「传输管理器」的实际能力

- **形态**：前端 Web 传输管理器（浏览器↔其所在 sproxy 节点），非服务端间同步。`web/static/transfer-store.js`（`TransferItem` localStorage + IndexedDB 块缓存）、`download.js`（分块下载管线，`chunksBitmap` + stat 校验续传）、`upload.js`（上传会话状态机）。
- **服务端依赖面**：`/upload/init|chunk|complete|status`、`/download/chunk`、`/api/files`、`/api/files/stat`。
- **结论**：transfer mgr 是**浏览器↔单节点**传输管理；其「分块 bitmap + stat（size/mtime/checksum）校验续传」客户端范式正是服务端间同步 worker 应复用的模式。文件同步不是前端功能，而是**服务端/CLI 到服务端的复制**，但复用同一分块管线与任务生命周期。

### 2.2 可复用资产

| 资产 | 位置 | 复用方式 |
|------|------|----------|
| 分块上传管线 | `pkg/server/chunked_upload.go` + `upload_store.go` | 接收侧写入器：`init(幂等 already_exists)→chunk(逐块 SHA-256)→status(missing_chunks)→complete(合并+校验+写 ChecksumStore)`；确定性 upload_id 天然支持幂等续传。**注意：`uploadInit` 拒绝 `total_size<=0`（chunked_upload.go:134）→ 0 字节文件走轻量路径（见 AD-3）** |
| 分块下载 | `pkg/server/chunked_download.go` `/download/chunk`（offset/length + X-Chunk-Checksum） | pull 时按 Range 拉取 |
| stat / 列表 | `pkg/server/download_handler.go` stat（X-File-Size/MTime/Checksum）、`list_handler.go` `/api/files` | 差异比对：源/目标 stat 比较 size+mtime+checksum |
| ChecksumStore | `pkg/server/checksum_store.go`（相对路径→SHA-256 权威账本） | 内容账本；同步落盘后写同一 store，形成可重扫差异面 |
| 任务生命周期模板 | `pkg/server/cloud_download.go` `CloudTask` | 状态机（pending/downloading/completed/failed/cancelled）+ JSON 持久化 + 信号量并发 + ReservedSize 配额对账 + 重启恢复，同步任务照搬（第二版服务端化） |
| 下载器续传 | `pkg/server/downloader/` `Downloader` + `HTTPDownloader`（Range/If-Range/ETag/.partial） | pull 时可复用（从远程 `/download` 按 Range 拉，失败保留 partial） |
| 配额 | `pkg/server/storage_manager.go` `TryReserve/Release` + `.__*` 目录约定 | **服务端分块管线已预留（`CategoryChunked`，chunked_upload.go:194）与写 ChecksumStore；同步客户端不自留配额**（v2，审查 M-5 表述修正） |
| 文件名校验 | `pkg/cloudfilename`（`ResolveFilename`/`Safe`/`ValidateEntries`） | 远端文件名生成/安全 |
| 远程文件客户端 | `pkg/client` `FileClient.ChunkedUpload/ChunkedDownload/Stat/Delete/Rename` | 目标节点写入器骨架（确定性 upload_id + 续传 + 校验；**`Rename` 在 client.go:792 已存在**，供 conflict-rename） |
| mesh 通道 | `pkg/tunnel/mesh`（`mesh.Dial`/`GatewayConnect`/中继）+ `relay.DialPolicy` | 经 mesh 链路连到远程节点 sproxy 文件服务（见 AD-1 与 I-6 模块边界） |

## 3. 架构决策

### AD-1：传输通道 = 经 mesh 链路调远程节点文件 API（最大复用，最少新协议）

同步引擎通过 mesh 链路建立到**远程节点 sproxy HTTP 文件服务**的连接，然后在上面跑既有文件 API（`/api/files`、`/upload/init|chunk|complete`、`/download/chunk`）。**不发明同步专用帧协议**。

- 通道获取：`sclient`（mesh 客户端）用 `mesh.Dial`/`GatewayConnect`/中继拨到远程节点，目标 = 远程节点 sproxy HTTP 端口（`--service sproxy:127.0.0.1:<port>` 宣告，或虚拟 IP——阶段 4 工作项 1 的产物；两者选其一可用即可）。
- 认证（v2，审查 M-6 修正）：mesh 流是**直连远程 sproxy 的 HTTP 端口**（普通 HTTP 请求，走 `srvMux` + `authMiddleware`），配置了 `access_keys` 时必须 SproxySig（`--access-key/--access-key-secret`）；**不适用 localMux 免签面**（localMux 是隧道内层，本设计不经本机 sproxy 隧道）。
- **数据面加密**：经 mesh 链路（隧道 AES-GCM）或直连 TLS，天然加密。
- **模块边界（v2，审查 I-6）**：`pkg/tunnel/mesh` 是独立 go.mod，主 go.mod 无 replace；`pkg/sync` **不得 import `pkg/tunnel/mesh`**。`pkg/sync` 只定义 `Dial func(ctx) (net.Conn, error)` 函数类型，mesh 拨号器由 `cmd/sclient` 组装注入（`pkg/sync` 保持仅依赖核心 go.mod）。

### AD-2：同步引擎独立包 `pkg/sync`（仅依赖核心 go.mod）

新增 `pkg/sync`（纯逻辑，不依赖 cmd、不 import mesh），含：

```go
type Job struct {
    ID             string
    Direction      Direction // push | pull
    Src            string    // 源路径（push=本地；pull=远程）
    Dst            string    // 目标路径（push=远程；pull=本地）
    Recursive      bool
    Filters        []Filter  // include/exclude glob
    ConflictPolicy ConflictPolicy
    Status         Status    // pending/syncing/completed/failed/cancelled
    Stats          Progress  // files/bytes done+total
    Results        []FileResult // {path, action, size, mtime, checksum, error}
    // Remote 描述远程节点与寻址（状态展示/续传 JSON 保留；Engine 不感知 mesh）——v2 审查 R-2
    Remote         RemoteRef
}
```

- `Engine` 负责编排：`Sync(ctx, job, transport)`。
- `Transport` 接口（远程文件操作抽象，v2 审查 I-5 补 `Rename`）：

```go
type Transport interface {
    ListDir(ctx, path string) ([]Entry, error)   // {name,size,mtime,checksum,is_dir}
    Stat(ctx, path string) (*Entry, error)
    PushFile(ctx, src io.Reader, size int64, remotePath, mtime, checksum string) error
    PullFile(ctx, remotePath string, dst io.Writer) error
    Rename(ctx, from, to string) error           // 供 conflict-rename（远端改名）
    Delete(ctx, path string) error
    Close() error
}
```

- 实现：`HTTPTransport` 用 `pkg/client` FileClient（ChunkedUpload/ChunkedDownload/Stat/List/Rename/Delete）打远程节点；底层 `http.Transport.DialContext` 自定义为「返回经 mesh 链路到远程节点文件服务的 TCP 流」（由 sclient 注入 `Dial func(ctx) (net.Conn, error)`）。

### AD-3：同步模型 = 单向 + 文件级差异 + 空文件/空目录/符号链接明确策略（v2，审查 C-2/I-7）

- **方向**：第一版单向（push / pull 分开的命令）。双向 = 两次单向的编排，不引入自动双向状态机。
- **递归 + 过滤器**：目录递归枚举；`--include/--exclude` glob（`pkg/sync/filter.go`，`path.Match` 纯 stdlib）。
- **差异判定**：对每个文件，源 stat（size+mtime+checksum）与目标 stat 比对：
  - 目标不存在 → 传输；
  - 目标存在且 checksum 相同 → skip（记 `skipped`）；
  - 目标存在且 checksum 不同 → 按冲突策略处理。
  - checksum 缺失（目标无 ChecksumStore 记录）→ 回落 size+mtime 比较。
- **增量**：文件级。传输走分块管线（`ChunkedUpload` 确定性 upload_id），中断后重跑同一 job 即幂等续传。块级增量第一版不做。
- **空文件（v2，审查 C-2）**：分块管线 `uploadInit` 拒绝 `total_size<=0`（chunked_upload.go:134），客户端 `ChunkedUpload` 同样拒绝（client/chunked.go:673）。**0 字节文件走轻量路径**：`HTTPTransport.PushFile` 检测 `size==0` → 调简单上传（multipart `POST /upload`，FileClient.Upload 已支持）或等价轻量写；不经过分块会话。空文件 stat 有 checksum（SHA-256 空串）→ 差异判定照常。
- **空目录（v2，审查 I-7）**：递归枚举产生空目录时，push 方向经目标节点 `/api/files` 无建目录 API（分块管线只在写文件时 `MkdirAll` 父目录）——**明确策略：空目录默认跳过并记 `skipped`（Warn 提示），`--sync-empty-dirs` 可选开启时经目标节点 `POST /mkdir`（已存在，dirs 相关 handler）创建**。
- **符号链接（v2，审查 I-7）**：**默认跳过**（记 `skipped_symlink`，防环），`--follow-symlinks` 可选跟随（跟随含自环软链时用访问过的 `(dev,ino)` 集合检测环）。`.__*` 内部元数据目录一律跳过（对齐 `list_handler.go isInternalDir`）。

### AD-4：冲突策略（v2，审查 I-5 补 Rename/非原子说明）

| 策略 | 行为 | 默认 |
|------|------|------|
| `skip` | 目标存在且不同 → 跳过（记 `skipped_conflict`） | ✅ 保守默认 |
| `overwrite` | 目标存在且不同 → 覆盖。**非原子窗口说明**：服务端 `checkExistingFileForInit` 对同名不同 checksum 回 409（chunked_upload.go:72-75），不会覆盖；客户端需先 `Delete` 再上传，中间有目标缺失窗口。优先路径：push 先 `Rename(remotePath, remotePath+".sync-tmp")` 再上传后 `Rename(tmp, remotePath)` 覆盖（窗口内目标以 `.sync-tmp` 存在，非完全缺失）；实现时确认 rename 覆盖语义，否则文档化 delete+upload 的非原子窗口 |
| `lww` | last-writer-wins：mtime 新者胜（相同 mtime 比 checksum；仍同则 skip）。同样经 rename 交换或 delete+upload（v2） |
| `conflict-rename` | 目标存在且不同 → 目标改名 `<name>.conflict-<unixnano>`（`Transport.Rename`）再写入，**不破坏原目标** |

- 策略在 job 上配置；`lww`/`conflict-rename` 需两端 mtime 可靠（服务端 `recordCompleteMetadata` 已 `os.Chtimes` 保留 mtime；**Windows mtime 精度/时区行为在测试中确认**，v2 审查 M-7；mtime 相同回落 checksum 的分支必测）。
- 冲突时**不静默破坏目标**：`skip`/`conflict-rename` 保证目标文件不被无声覆盖；`overwrite`/`lww` 为显式选择，且非原子窗口文档化。

### AD-5：任务状态与持久化 = 服务端 SyncManager（v3，用户决策：服务端任务进 v1）

**v1 即服务端托管**：`sclient sync push/pull` 在本地 sproxy 创建同步任务（`POST /api/sync/tasks`），服务端 `SyncManager`（`pkg/server/sync/`）负责执行、持久化与前端可见。

- **服务端前置**：执行远程同步需要服务端有 mesh 通道（sproxy 配置/运行了 mesh 能力，经网关 `GatewayConnect` 或 `mesh.Dial` 到远程节点 sproxy 文件服务）。服务端未配置 mesh 通道时，创建**远程**同步任务 fail-closed 拒绝（报「服务端未配置 mesh 通道」）；纯本地路径（若未来有）不受影响。
- **任务模型**（照搬 CloudTask 生命周期）：`SyncTask{ID, Direction, Remote, Src, Dst, Recursive, Filters, ConflictPolicy, Status(pending/syncing/completed/failed/cancelled), Stats(Progress), Results[]FileResult, CreatedAt/UpdatedAt/ExpiresAt, ReservedSize}`。
- **持久化**：`uploadsDir/.__sync__/<id>.json`（对齐 cloud `.__downloads__` 模式）；终态/进度按 CloudTask 的 dirty-flush 模式；重启恢复只重启 `syncing` 状态。
- **并发与配额**：`semaphore` 信号量限制并发同步任务；配额由分块管线服务端侧 `TryReserve(CategoryChunked)` 承担（同步客户端不自留）。
- **API**：`POST /api/sync/tasks`（创建）、`GET /api/sync/tasks`（列表）、`GET /api/sync/tasks/{id}`（查询）、`POST /api/sync/tasks/{id}/cancel`（取消）、`DELETE /api/sync/tasks/{id}`（删除）。localMux + srvMux authMiddleware 双注册，对齐 `handlers.go:228-233` 分块路由模式。
- **前端**：`transfer-store.js` 增加 `kind:'sync_task'`；`app-render.js` 增加 `sync_task` 频道渲染（复用 `filterTransferItems` 频道谓词 + 统一行组件）；传输页频道条新增 `sync`。
- **CLI**：`sclient sync` 仍负责：参数解析 → `POST /api/sync/tasks` 创建 → 轮询 `GET /api/sync/tasks/{id}` 展示进度/结果；`--json` 输出便于脚本。

### AD-6：HTTPTransport 并发模型（v2，审查 I-9）

- **第一版选型：单连接串行分块 + 文件级并发**。`HTTPTransport` 固定底层 `http.Transport.MaxConnsPerHost=1`（避免每并发开一条 webrtc 打洞/中继流、耗尽对端 accept 环；HTTP/1.1 无 pipelining，分块并发本会被串行化）。`Engine` 在**多文件之间**并行（受 `--concurrency` 限制，默认 3）。
- **deadline 失效兜底（v2，审查 I-4）**：webrtc 直连路径的 `MuxStreamConn.SetDeadline` 是 no-op（mesh.go:105-107），`http.Transport` 依赖 deadline 的超时在直连路径静默失效。处理：
  - 为 mesh 流包一层 deadline-aware 连接（记录 deadline，超时强制 `Close`/`Abort` 底层流）——优先；或
  - 每请求显式 ctx 取消 + 应用层 watchdog 兜底。
  - E2E 必加「对端停读 → 同步请求超时失败」用例（webrtc 直连路径）。

## 4. 关键接口

### 4.1 `pkg/sync`（新包，仅依赖核心 go.mod）

- `Engine` / `Sync(ctx, job, transport)`：目录枚举 → 差异比对 → 冲突决策 → 传输 → 结果记录。
- `Transport` 接口（§AD-2，含 `Rename`）+ `HTTPTransport`（`pkg/client` 打远程）+ `Dial func(ctx) (net.Conn, error)` 注入点（**由 `cmd/sclient` 组装 mesh 拨号器**，pkg/sync 不 import mesh）。
- `filter.go`：`ParseFilters` / `Match(path, filters)`（include/exclude glob，`path.Match` 纯 stdlib）。
- `conflict.go`：`Decide(conflict, src, dst Entry) Action`（纯函数，表驱动测试重点）。
- `Diff(ctx, srcListFn, dstStatFn) []DiffEntry`：差异计算纯函数（可测：给 mock 列表/stat）。
- `entry.go`：`Entry{Name, Size, MTime, Checksum, IsDir}`；目录枚举含空目录/符号链接判定。

### 4.2 `cmd/sclient/sync.go`（新命令）

- `newCmdSync`：子命令 `push` / `pull`；flags `--remote`（节点）/ `--dst` / `--src` / `--recursive` / `--include` / `--exclude` / `--conflict`（skip|overwrite|lww|conflict-rename）/ `--follow-symlinks` / `--sync-empty-dirs` / `--concurrency` / `--json` / `--access-key` / `--access-key-secret` / `--hub` / `--gateway`（复用 mesh 寻址）。
- cmd 保持薄：flag 解析 → 组装 `sync.Job` + 建 `HTTPTransport`（注入 mesh `Dial`）→ 调 `sync.Sync` → IO 展示。

### 4.3 `pkg/client`（复用）

- `FileClient` 现有 `ChunkedUpload`/`ChunkedDownload`/`Stat`/`ListWithPagination`/`Rename`/`Upload` 已满足；`HTTPTransport` 内部调用。**`Rename` 已存在（client.go:792）**，供 conflict-rename 与 overwrite rename 交换。

### 4.4 服务端 SyncManager + API + 前端（v3，v1 实现）

- `pkg/server/sync/` `SyncManager`：任务生命周期（照搬 `cloud_download.go` 模式）+ `.__sync__/` 持久化 + 并发信号量 + 配额对账 + 重启恢复。执行器复用 `pkg/sync.Engine`（`Transport` 的 mesh `Dial` 由服务端 mesh 网关能力组装注入）。
- `pkg/server/sync_handler.go`：`/api/sync/tasks` 等端点（localMux + srvMux 双注册，`handlers.go RegisterRoutes`）。
- 服务端 mesh 通道：spoxy 服务端若运行 `mesh node`（或配置了可用的 mesh 网关），`SyncManager` 经网关 `GatewayConnect`/`mesh.Dial` 建立到远程节点 sproxy 文件服务的连接；否则创建远程任务 fail-closed 拒绝。
- `web/static/transfer-store.js` / `app-render.js`：`sync_task` kind + 频道渲染；传输页频道条新增 `sync`。
- `Makefile`：`web-test` 加入新 JS 的 `node --check` 与测试（若前端变更）。

## 5. 边界与安全面（汇总）

1. **路径校验**：源/目标路径经 `ValidateFilePath` 校验（防穿越、防绝对路径、Windows 非法字符）；`pkg/cloudfilename.Safe` 兜底远端文件名；符号链接默认跳过防环。
2. **认证**：远程文件 API 访问需 SproxySig（`--access-key/--access-key-secret`）——mesh 流直连远程 sproxy HTTP 端口走 `srvMux` + `authMiddleware`（v2，M-6）。未配置凭据且远程配置了 access_keys → fail-closed 拒绝。
3. **配额**：服务端分块管线 `uploadInit` 已 `TryReserve(CategoryChunked)`，写 CheckSumStore 由服务端负责；同步客户端不自留配额（v2，M-5）。`ErrStorageFull` 上报为「目标存储空间不足」，不破坏已存在文件。
4. **SSRF/出口边界**：远程节点地址解析由 mesh 选路策略约束（`relay.DialPolicy`，`--dial-allow` + `ServiceAddrs` 精确放行）；同步通道目标只能是远程节点宣告的 sproxy 服务。
5. **防环**：第一版单向，无自动双向；同目录双向由用户编排并自行避免（文档提示）。
6. **不静默覆盖**：`skip`/`conflict-rename` 保证目标不被动无声覆盖；`overwrite`/`lww` 显式选择且非原子窗口文档化（v2，I-5）。
7. **目录遍历安全**：跳过 `.__*` 内部元数据目录；空目录默认跳过（`--sync-empty-dirs` 显式开启）。
8. **超时兜底**：webrtc 直连路径 deadline 失效有 deadline-aware 包装/ctx 取消/watchdog（v2，I-4）。

## 6. 与现有模块的接口点（汇总）

| 模块 | 文件 | 变更 |
|------|------|------|
| 同步引擎（新） | `pkg/sync/`（engine.go/transport.go/filter.go/conflict.go/diff.go/entry.go） | `Engine`/`Transport`/纯函数；不 import mesh |
| CLI | `cmd/sclient/sync.go`（新） | `sclient sync push/pull` → 创建服务端任务 + 轮询展示；CLI 侧 mesh `Dial` 注入服务端 |
| 远程文件客户端 | `pkg/client`（复用，小改可选） | `HTTPTransport` 打远程（含 `Rename`） |
| 服务端任务（v1） | `pkg/server/sync/`（SyncManager + sync_handler.go）、`pkg/server/handlers.go`、`storage_manager.go` | 任务生命周期 + `.__sync__/` 持久化 + `/api/sync/tasks` 双注册 + 配额 |
| 前端（v1） | `web/static/transfer-store.js`、`app-render.js`、`index.html` | `sync_task` kind + 频道渲染 + 传输页频道条 |
| 测试 | `pkg/sync/*_test.go`、`cmd/sclient/sync_test.go`、`test/e2e_test.go` | 见 §7 |

## 7. Definition of Done

1. `sclient sync push <dir> --remote <node> --dst <path>` 把本地目录（含子目录）推到远程节点，目标落盘正确、ChecksumStore 写入、mtime 保留。
2. `sclient sync pull --remote <node> --src <path> <dir>` 拉取到本地。
3. 差异判定：已同步文件（checksum 相同）跳过；变更文件重新传输；新增文件传输。
4. 冲突策略 4 种各有测试：`skip`（默认）不覆盖、`overwrite` 覆盖（含 rename 交换非原子窗口行为）、`lww` mtime 新者胜（mtime 相同回落 checksum 分支）、`conflict-rename` 目标改名保留（`Transport.Rename`）。
5. 递归 + include/exclude 过滤器生效。
6. **空文件同步成功**（0 字节走轻量路径，C-2 闭环）；**空目录默认跳过 + `--sync-empty-dirs` 创建**；**符号链接默认跳过 + `--follow-symlinks` 跟随（含自环检测）**。
7. 分块续传：中断后重跑同一 job 只补缺失块（幂等）。
8. **对端停读 → 同步请求超时失败**（webrtc 直连路径 deadline 兜底，I-4 闭环）。
9. 安全：路径校验拒绝穿越/绝对路径；未认证远程访问拒绝（fail-closed）；目标配额满报错不破坏既有文件；`.__*` 目录不外泄。
10. **服务端 SyncManager（v3）**：`POST /api/sync/tasks` 创建任务 → `GET /api/sync/tasks/{id}` 查询进度 → `POST /api/sync/tasks/{id}/cancel` 取消 → `DELETE /api/sync/tasks/{id}` 删除；任务状态机 pending/syncing/completed/failed/cancelled 正确流转；`.__sync__/` 持久化 + 重启恢复（只重启 syncing）；服务端未配置 mesh 通道时创建远程任务 fail-closed 拒绝。
11. **前端 sync_task 频道（v3）**：传输页频道条出现 `sync`；`sync_task` 任务按统一行渲染（状态徽章 + 进度 + 操作按钮），`filterTransferItems` 频道谓词有单测；`make web-test` 全绿。
12. `pkg/sync` 不 import `pkg/tunnel/mesh`（编译验证）；核心 go.mod 零三方新增。
13. 质量门禁：受影响包 `go test -race -count=1 ./...` 全绿；`make lint` 0 issues（主 + 每个子 go.mod）；`make build-all` + `make test-all` + `make check-loopback` 全绿。
14. 对抗式审查全部发现（含 Minor/参考级）修复，reviewer 逐条关单；无未解决 Critical/Important。
15. 合并后写 `docs/superpowers/learnings/2026-08-30-stage4-filesync.md`。

## 8. 拆分子任务建议（v3，含服务端任务，共 6 个子任务，逐个 PR 合并）

1. **A：`pkg/sync` 核心**（Transport 接口含 Rename + Engine + 目录枚举/差异/冲突/空文件/空目录/符号链接纯函数 + 单测；不 import mesh）。
2. **B：`HTTPTransport` + deadline-aware**（pkg/client 复用，mesh `Dial` 注入 → HTTP；webrtc 直连 deadline 兜底 I-4；单连接串行 + 文件级并发 I-9）。
3. **C：服务端 `SyncManager` + `/api/sync/tasks`**（`pkg/server/sync/`：任务生命周期/`.__sync__/` 持久化/并发/配额/重启恢复 + sync_handler.go 双注册 + `handlers.go` 接线；服务端 mesh 通道能力探测）。
4. **D：`sclient sync push/pull` CLI**（命令 + flag + 创建服务端任务 + 轮询进度/结果展示 + JSON 输出）。
5. **E：前端 `sync_task` 频道**（transfer-store kind + app-render 渲染 + 传输页频道条 + Makefile web-test 接线）。
6. **F：E2E + 安全补强 + 对抗式审查 + 修复**。

每子任务独立 feature 分支 → PR → CI → 人工 squash 合并，流程沿用阶段 3。子任务 C/D/E 可串行或按依赖并行（D 依赖 C 的 API，E 依赖 C 的 API）。

## 9. 与工作项 1（虚拟 IP）的关系

- 虚拟 IP 为同步提供「远程节点 sproxy 文件服务」的通用寻址（`--remote <node>` 可解析到 `<vip>:<sproxy-port>`）；两者独立可交付，但共享 mesh 链路与 `DialPolicy` 边界。
- 同步第一版不**依赖**虚拟 IP：远程节点用 `--service sproxy:127.0.0.1:<port>` 宣告即可寻址。虚拟 IP 合入后可选用它寻址。
- 建议实现顺序：先文件同步（独立、复用多、风险低），后虚拟 IP（架构级、需分配/持久化/网关 NAT）。两者 PR 可并行推进。
