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
