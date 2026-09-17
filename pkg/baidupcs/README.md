# BaiduPCS Plugin (pkg/baidupcs)

百度网盘（BaiduPCS）存储后端插件，**独立 Go module**。

## 方案（R2：fork + replace）

本包**不 fork 裁剪核心库进 sproxy 仓库**——直接引用外部 fork：

- **外部 fork**：`github.com/cocomhub/BaiduPCS-Go`（fork 自 `qjfoidnh/BaiduPCS-Go`）
  - module 声明**保持 `github.com/qjfoidnh/BaiduPCS-Go` 一行不改** → GitHub「Sync fork」零冲突
  - 上游更新 → fork 点 Sync fork → 更新本包 `go.mod` 的 replace commit（一行）
- **replace 接入**：`pkg/baidupcs/go.mod` 里
  ```
  require github.com/qjfoidnh/BaiduPCS-Go v0.0.0
  replace github.com/qjfoidnh/BaiduPCS-Go => github.com/cocomhub/BaiduPCS-Go <commit>
  ```
  Go module 惰性加载，只编译 import 图内的包；fork 内部 import 自引用无需修改。
- **零污染**：开源实现不进本仓，不受 sproxy addlicense/lint 强校验影响；本包只含自有薄 adapter。

## 执行策略

- **二进制优先**：默认调 `BaiduPCS-Go` 二进制（命令语义稳定、子进程隔离、完整传输器内置、可独立升级）
- **库兜底**：二进制缺失/失败/超时 → 回退 fork 库实现（`PrepareUpload`/`DownloadFile`）

## 稳定性保障

- 传输共享 `http.Client`（无整体超时，正文由 ctx 约束）
- 上传后 ETag 复核有界重试 ≤3
- 错误分类映射集中维护（31066/-3/-9 → NotFound 等）
- 子进程 `exec.CommandContext` + 超时

## 构建

```bash
cd pkg/baidupcs
GOSUMDB=off GOPRIVATE=github.com/cocomhub GOWORK=off go mod tidy
GOSUMDB=off GOPRIVATE=github.com/cocomhub GOWORK=off go build ./...
GOSUMDB=off GOPRIVATE=github.com/cocomhub GOWORK=off go test ./...
```

> `GOSUMDB=off GOPRIVATE=github.com/cocomhub` 必须：cocomhub fork 未发布到 sum.golang.org，
> 需跳过 sumdb 校验（私有依赖标准做法）。CI 装配时需同样注入这两个环境变量。

## 维护流程（fork 更新）

1. 上游 `qjfoidnh/BaiduPCS-Go` 更新 → cocomhub fork 点 GitHub「Sync fork」（零冲突）
2. 更新本包 `go.mod` 的 `replace` commit 为 fork 最新：
   ```bash
   gh api repos/cocomhub/BaiduPCS-Go/commits/main --jq .sha
   ```
3. `go mod tidy` + 测试

## P2：库兜底真实现（脱离二进制完整可用）

P2 把库兜底从「最小 stub」提升为**真实现**，二进制缺失/失败/超时时完全可用：

### 分片上传（NewMultiUploader）

- 走上游多线程分片上传器：`Precreate` → 并发分片 `TmpFile` → `CreateSuperFile`（合并）
- `MultiUpload` 三方法由本包 `baiduMultiUpload` 实现（接百度网盘 API）
- 并发度 / 分块大小 / 限速可配（默认 4 并发 / 4MB 分片）
- 分片瞬时失败自动重试；重名策略 skip/overwrite 跟随上传参数

### 断点续传（本地持久化）

- 上传断点：`InstanceState` 持久化到 `<Layout.Resume>/<key>.json`（原子写 tmp+rename）
- 下载断点：上游 `Downloader` 的 Range 断点文件在 `<Layout.Tmp>/<key>.download`（JSON）
- 断点 key = `sanitize(目标路径) + 文件大小`——文件变更（大小不同）则断点自动失效
- **中间状态只依赖本地文件系统**（用户硬约束）：staging/resume/cache/tmp 全在本地 `Layout` 下，网盘只存最终文件

### 本地目录布局

```
<BaseDir>/
  staging/  本地上传暂存（配合 quota）
  resume/   上传断点（InstanceState JSON）
  cache/    下载缓存（可选）
  tmp/      通用临时文件（下载断点 .download、.part 半截文件）
```

### 配额挂钩（P2 基础版）

- `Quota` 内存计数：staging/cache 预留 → 传输完成释放，防止本地磁盘被中间态占满
- 超限返回错误（调用方暂停/拒绝继续写 staging）
- P4 与 sproxy `pkg/quota.Scope` 融合时替换为 owner 配额池

## P3：sync.FS 适配 + VolumeBackend（可同步的存储后端）

百度网盘已成为「可同步的存储后端」——`pkg/sync` 引擎可在本地↔网盘间同步（与本地↔本地共用同一套编排逻辑）。

### 能力

- **`StorageFS`**（`syncfs.go`）：把 `Storage` 适配为 `pkg/sync.FS`（7 方法接口）
  - `ListDir` → `Storage.List`（Path 相对 FS 根、正斜杠）
  - `Stat` → `Storage.Stat`（不存在返回 nil；网盘根恒为目录）
  - `OpenRead` → `Storage.Get`（本地临时文件 + 自动清理）
  - `WriteFile` → 本地 staging 临时文件 + `Storage.Put`（**中间态只依赖本地 FS**）
  - `Rename` → `Storage.Copy` + `Storage.Delete`（网盘无原子 MOVE 两步）
  - `Delete` → `Storage.Delete`（幂等）；`MakeDir` → no-op（网盘无独立目录纯概念）
- **`VolumeBackend`**（`volume.go`）：`NewVolumeBackend` 构造「网盘卷 = StorageFS + 本地中间态根目录」
  - 寻址：装配后 `baidupcs://<卷名>/<path>` 可被 `syncmgr.Job` 使用
  - `RootDir` 是本地中间态基目录（staging/cache 落它之下，符合 `volume.RootDir` 契约）
- **quota 借贷**（`StorageFS.WithQuota`）：装配层注入 `QuotaTracker` 实现（如 `pkg/quota.Scope`）
  - staging 写入预留 size → 上传网盘成功释放；失败归还

### 同步用法（syncmgr / pkg/sync）

```go
// 本地 → 网盘（push）
job := &sync.Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip}
engine.Sync(ctx, sync.NewLocalFS(localRoot, nil), storageFS, job)

// 网盘 → 本地（pull，同引擎反向）
engine.Sync(ctx, storageFS, sync.NewLocalFS(localRoot, nil), job)
```

### 中间态约束（用户硬规则）

所有暂存/断点/缓存只依赖本地文件系统（`<LocalRoot>/`）：WriteFile 流先落 staging 临时文件再上传；
OpenRead 经本地临时文件返回流；网盘侧只存最终文件。

## P4：syncmgr 双向同步集成 + quota 融合（服务端真实装配）

百度网盘已成为**服务端可编排的同步目标**：`sync_remotes[].kind=baidupcs` 的同步任务由
`syncmgr`（任务编排）+ `syncexec`（执行器）驱动，本地↔网盘双向同步与本地↔本地共用同一套
编排逻辑（统计/冲突策略/递归）。

### 配置示例

```yaml
# 1. 启用网盘后端（baidupcs 段；默认关闭）
baidupcs:
  enabled: true            # 启用才装配（sync_remotes[].kind=baidupcs 的前提）
  name: "mydisk"           # 卷名（必填；sync_remotes[].volume 引用它）
  root: "/"                # 网盘根路径（空 = "/"）
  local_root: ""           # 本地中间态基目录（空 = <temp>/baidupcs/<name>）
  bduss: ""                # 百度网盘登录凭据（库兜底需要）
  binary_path: ""          # BaiduPCS-Go 可执行路径（空 = PATH 查找）

# 2. 声明同步远端（kind=baidupcs：本机网盘卷，无网络对端）
sync_remotes:
  - name: "mydisk"
    kind: "baidupcs"
    volume: "mydisk"       # 本机卷名（= baidupcs.name）
```

创建同步任务（sclient/API）时 `remote: "mydisk"`，`src`/`dst` 为 FS 根相对路径
（如 `sub/dir`；空 = 整个根）。**push** = 本地→网盘；**pull** = 网盘→本地。

### 寻址与装配链路

`kind=baidupcs` 的远端**不经网络拨号**：装配层（`cmd/sproxy/baidupcs_sync.go` 的
`setupBaidupcsFSFactory`）启动时按 `baidupcs.name` 构造 `VolumeBackend`（= `StorageFS` +
本地中间态根目录），维护 `卷名 → *VolumeBackend` 映射；执行时按 `remote.Volume` 查表返回
对应 `StorageFS`，交给 `pkg/sync` 引擎：

```
syncmgr.Manager ── Executor.Run ── BaidupcsFS 工厂（按 volume 查表）── StorageFS ── Storage ── 网盘
```

`pkg/syncexec` 只消费 `sync.FS` 抽象 + 工厂签名（`SetBaidupcsFSFactory`），不依赖
`pkg/baidupcs` 具体类型——领域包互不依赖，卷名映射在唯一装配层维护（与 mesh 载体同构）。

### quota 融合（staging 记账）

`StorageFS.WithQuota` 注入 `scopeQuotaTracker`（`QuotaTracker` 的卷级 `pkg/quota.Scope`
适配器）：本地 staging 写入前 `TryReserve(size)` + `Commit(size)` 入账，上传完成/失败后
`ReleaseUsage(size)` 从 committed 扣减——**中间态只依赖本地 FS**（用户硬约束）且占用被
记账。

> **当前为记账形态（无限额）**：`Scope(0)` 只记录 staging 占用、不设上限；per-owner 分桶与
> 限额（防本地磁盘占满）是 **P5 演进**（`BaidupcsFSFactory` 签名无任务 owner 上下文，与
> mesh 工厂同构——需要时扩展签名或改卷级限额配置）。

### fail-closed 语义

- `baidupcs.enabled=true` 时 `name` 必填、`bduss`/`binary_path` 至少一个非空——否则
  **启动即拒绝**（Validate 层，fail-closed：二进制优先与库兜底都无可用执行路径）。
- Storage 构造失败（凭据/客户端错误）→ 装配层**不注入**并告警：`kind=baidupcs` 的远端
  以 `syncexec.ErrBaidupcsNotWired` 明确失败，**绝不回落 direct**（回落会让「已声明本机卷」
  的配置静默走远程 HTTP，破坏卷寻址语义——与 mesh 同一原则）。
- `remote.volume` 未装配（`baidupcs.name` 不匹配）→ 创建/执行任务明确报错。

### 疑虑清单闭环

- **List 单层过滤（P3 疑虑，已修）**：`Storage.List` 由「Stat 包装（单对象）」改为**真目录
  单层列举**（库 `FilesDirectoriesList`，含目录条目）；`Storage.Stat` 走库
  `FilesDirectoriesMeta`（isdir/mtime/size）。`StorageFS.ListDir` 单层透传，树遍历由
  `pkg/sync` 引擎自递归。
- **quota 卷级记账（P5 per-owner）**：如上，当前卷级 Scope(0) 记账；per-owner 分桶与限额留 P5。
- **`Storage.Copy` = Get + Put 组合**（P3 遗留缺口补齐）：`StorageFS.Rename`（Copy+Delete
  两步）可走通，但整文件下载再上传（非服务端直拷）。真实百度网盘有 COPY API，将来 Adapter
  扩展可优化为服务端直拷。
