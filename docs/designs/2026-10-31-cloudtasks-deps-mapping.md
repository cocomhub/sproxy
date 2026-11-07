# cloudtasks 依赖映射：19 个装配依赖 → 8 字段 Deps（P1-c / S0）

> 状态：**映射分析（只读产出）**，2026-10-31。前置：
> `docs/designs/2026-10-31-policy-object-extraction.md` §2.3（`cloudtasks.Orchestrator`，Deps ≤8）、
> P1-a `volume.Router`（设计已定）、P1-b `quota.Settlement`（已落地，`pkg/quota/settlement.go`）。
> 本文是 P1-c 的 **S0**：把 `cloud_download_handler.go` + `cloud_archive_handler.go` 的真实依赖
> 逐条映射成 ≤8 个窄接口，并给出 S1–S4 的拆分、验收与「必须逐字等价」清单。
> **本文不授权任何代码改动**——S1 起才动 `.go`。

## 0. 结论摘要

| 项 | 实测 |
|---|---|
| 两个文件规模 | 2165 行（1326 + 839；设计文档写的 1357/844 已漂移） |
| 函数 | 67 个：49 个 `*Handlers` 方法（16 个 HTTP 入口 + 33 个内部 helper）、18 个包级函数 |
| `h.*` 引用 | **185 处 / 51 个不同符号** |
| 真正的装配依赖 | **19 个符号 / 130 处**（其余 32 个符号 / 55 处是文件内定义、以 `h.` 引用的 helper；另有 `recordBypassWrite` 只在装配层被引用） |
| 无法用 `h.*` 统计到的入边 | `sendJSONResponse` **102 处**、`ActorFrom` 23 处、`drainAndVerifyBody` 8 处、`msg*` 常量 32 处等（见 §4） |
| 收敛结果 | 19 → **8 个 Deps 字段**（7 个接口 + `*slog.Logger`），接口方法合计 37 个 |
| 最大残余风险 | ①「预留 → 写 → 对账 → 提交/回滚」的**分支顺序**（5 条路径）；② 转存写锁的**键空间与持锁窗口**；③ 102 处响应写要改成 typed error + 装配层状态映射 |

**关于「≤8 是门禁」的订正**：R52 的现行上限是 **12**
（`internal/archcheck/deps_field_cap_test.go:34` 的 `const depsFieldCap = 12`），
「≤8」来自 P1 设计文档的验收口径，**不是现行门禁**。因此 S1 必须在包内补
`reflect.TypeOf(Deps{}).NumField() <= 8` 断言（R52 不会替我们守住 8）；**禁止**为过门禁上调
`depsFieldCap`（设计文档 §3 已明文禁止）。

## 1. 统计口径（可复现）

```bash
# 全部 h.* 引用，按出现次数排序（去重 51）
grep -oE '\bh\.[A-Za-z_][A-Za-z0-9_]*' \
  pkg/server/cloud_download_handler.go pkg/server/cloud_archive_handler.go \
  | sed 's/.*://' | sort | uniq -c | sort -rn

# 分文件：download = 39 unique / 111 处；archive = 24 unique / 74 处；并集 51 / 185 处
```

口径说明：

- **「装配依赖」= 符号定义在其它文件、需要经构造期注入的能力**（19 个）。
- **「随迁 helper」= 定义在本文件内的 `*Handlers` 方法**（33 个：32 个以 `h.*` 形式被引用 +
  `recordBypassWrite` 仅由装配层 `routes_setup.go:648` 引用，故不进 51 符号统计）——它们不是 Deps，
  会跟着编排核心一起搬进 `pkg/server/cloudtasks`；它们对 19 个装配依赖的引用已被计入那 130 处。
- 只统计 `h.*` 会**低估**入边：`sendJSONResponse`/`ActorFrom`/`msg*` 等包级符号也是跨文件依赖，
  单独列在 §4；其中 `filesPhysFootprinter{h: h}` 是**隐藏的 `h` 依赖**，必须并入接口，
  否则迁移后会出现「Deps 是 8 个、实际第 9 条入边藏在包级适配器里」。

## 2. 装配依赖清单（19 个，按出现次数）

行号格式 `d=` download 文件、`a=` archive 文件；`*` 表示该行是注释。

| # | 依赖 | 次数 (d/a) | 行号 | 用途分类 | 真实类型 / 签名 | 归入接口 |
|---|---|---|---|---|---|---|
| 1 | `cloudMgr` | 35 (33/2) | d:96,119,131,192,206,220,231,277,284,295,317,347,387,402,408,419,474,478,479,488,505,507,510,517,535,538,576,664,744,794,800,962,963；a:300,542 | 任务存储 | `*cloud.CloudDownloadManager`（17 个方法，见 §5.1/5.2） | `Tasks` + `Groups` |
| 2 | `logger` | 32 (11/21) | d:557,734,802,807,817,824,831,855,931,940,970；a:121,152,161,171,181,246,255,265,275,329,336,361,411,427,450,544,548,556,562,570,768 | 日志 | `*slog.Logger`（含 `.With(...)` 派生） | `Logger` |
| 3 | `storageMgr` | 11 (4/7) | d:914,915,936,942；a:157,163,251,257,468,471,477 | 配额（回退账本） | `*capacity.StorageManager`：`WaitReserve(ctx,n,cat,timeout) error`、`Release(n,cat)`、`TryReserve(n,cat) error`（`pkg/storage/capacity/wait.go:100`） | `Quota`（适配为 3 个 CategoryCloud 语义方法） |
| 4 | `RecordAudit` | 8 (8/0) | d:300,307,318,325,415,543,550,1230 | 审计 | `func (h *Handlers) RecordAudit(ctx context.Context, evt AuditEvent)`（`pkg/server/audit.go:119`） | `Audit` |
| 5 | `tenantMu` | 6 (0/6) | a:716,717,754,758,763,767 | 锁（**包内状态**） | `sync.Mutex`（`deps.go:248`，保护 `archiveUsage` 等懒建 map） | 不入 Deps：`archiveUsage` 随迁后由包内 `usageMu` 承担 |
| 6 | `tenantFor` | 6 (2/4) | d:815,848；a:327,354,554,734 | 源读取 / 租户解析 | `func (owner string) *storage.Tenant`（`handlers.go:281`） | `Tenants` |
| 7 | `archiveUsage` | 6 (0/6) | a:718,719,721,724,755,764 | 状态（**archive 桶已确认占用**） | `map[string]map[string]int64`（`deps.go:247`；`routes_setup.go:194` 初始化） | 不入 Deps：随迁为包内状态 |
| 8 | `volumeAllowedFor` | 4 (2/2) | d:54,707；a:400,433 | 卷路由（ACL） | `func (owner, volName string) bool`（`volumes_api.go:428`，未装配/未知卷 → false） | `Volumes.Allowed` |
| 9 | `volSet` | 4 (4/0，3 个调用点) | d:621,635,643(×2) | 卷路由 / 卷级容量池 | `*registry.Set`：`Pool(name) *quota.Pool`（`set.go:511`）、`HasLocalRoot(name) bool`（`set.go:231`） | `Volumes.IsLocal` + `Volumes.LocalPool` |
| 10 | `fileService` | 3 (3/0) | d:1133,1136,1152 | 派生状态（checksum 台账 + 搜索索引） | `*files.Service`：`RecordBypassChecksum(owner, rel, checksum, vol string)`、`RecordBypassWrite(owner, rel string, size int64, checksum, volume string, root *storage.Root)`（`pkg/files/service.go:638/661`） | `Targets` |
| 11 | `externalVolumeTransferFS` | 3 (3/0) | d:602,1016,1187 | 目标写入（卷 FS 解析） | `func (volumeName string) (sync.FS, string, volume.Volume, bool)`（`volumes.go:733`） | `Volumes.TransferFS` |
| 12 | `transferWriteLock` | 2 (2/0) | d:1213（1198 为注释） | 锁（跨写面同键空间） | `func (owner, rel string) (func(), bool)`（`uploading_lock.go:101`；键归一 `lockRelFor` :92） | `Targets.Lock` |
| 13 | `quotaWaitTimeout` | 2 (2/0) | d:904,906 | 配额 | `time.Duration`（0 = `quota.DefaultWaitTimeout`；`deps.go:249`） | `Quota.WaitTimeout` |
| 14 | `quotaScopeFor` | 2 (2/0) | d:615,1067 | 配额 | `func (owner, rel string) *quota.Scope`（`handlers.go:614`） | `Quota.ScopeFor` |
| 15 | `quotaBucketFor` | 2 (1/1) | d:902；a:760 | 配额 | `func (owner, bucket string) *quota.Scope`（`handlers.go:568`） | `Quota.BucketScope` |
| 16 | `verifySkipped` | 1 (1/0) | d:1091 | 配置 | `func () bool`（`volumes.go:800`；每次读 `cfgPtr`） | `Targets.SkipVerify` |
| 17 | `stagingQuotaFS` | 1 (1/0) | d:1049 | 目标写入（本地 staging 配额 gate） | `func (owner string, fs sync.FS) sync.FS`（`volumes.go:763`） | `Targets.StagingFS` |
| 18 | `cfgPtr` | 1 (0/1) | a:86 | 配置 | `*atomic.Pointer[Config]`；仅读 `CloudArchiveMaxBytes` | `Targets.MaxArchiveBytes` |
| 19 | `archiveKey` | 1 (0/1) | a:826 | 配置 / 密钥 | `func () []byte`（`archive.go:49`；每次读 `archive.key_file`） | `Targets.ArchiveKey` |

补充：`filesPhysFootprinter{h: h}` 在 **d:1184**（`archiveTargetCharge` 内）以包级适配器形式
隐式依赖 `Handlers`（`pkg/server/files_phys.go:27`、`PhysFootprint` 在 :75）——统计 `h.*` 时看不到，
必须并入 `Volumes.PhysFootprint`。同理 `recordBypassWrite`（d:1131）在本文件内无 `h.` 调用点，
只经 `routes_setup.go:648` 注入给 `cloud` 领域，但它仍是必须随迁的同文件实现。

## 3. 随迁 helper（33 个），不是 Deps

这些方法定义在两个文件内，随编排核心整体进 `pkg/server/cloudtasks`；它们对外部依赖的引用
已经计入上表 130 处。`recordBypassWrite` 在本文件内没有 `h.` 调用点（由
`routes_setup.go:648` 注入给 `cloud` 领域），因此不出现在 51 符号统计里，但同样必须随迁。

| helper | 次数 | 签名（现状） | 迁移形态 |
|---|---|---|---|
| `checkTransferACL` | 3 | `(owner string, transfer *cloud.TransferSpec) string` | 返回 `error`（`ErrForbidden` + 原文案） |
| `deleteGroupArchive` | 1 | `(owner, archiveFile string) error` | 原样（内部改走 `Volumes`/`Quota`） |
| `settleVolumePoolCharge` | 1 | `(volName string, before, after int64)` | 原样 |
| `isLocalStorageVolume` | 4 | `(volName string) bool` | 换 `Volumes.IsLocal` |
| `cloudArchiveMaxBytes` | 1 | `() int64` | 换 `Targets.MaxArchiveBytes` |
| `cloudArchiveParseRequest` | 1 | `(w, r) (*CloudArchiveRequest, bool)` | HTTP 适配层（留装配层） |
| `cloudArchiveResolveName` | 1 | `(w, req, groupID string) (string, bool)` | 核心返回 `(name, error)`；响应留适配层 |
| `cloudArchiveCollectFiles` | 1 | `(w, groupID, owner string) ([]fileWithRelPath, []string, int64, bool)` | 核心返回 `([]sourceFile, []string, int64, error)` |
| `cloudArchivePrepTarget` | 1 | `(w, owner string, req, archiveName string) (*storage.Root, string, bool)` | 核心返回 `(root, rel, error)` |
| `cloudArchiveMaxBytesOK` | 2 | `(w, totalSize int64) bool` | `(error)`：超限 → `ErrTooLarge`（400） |
| `cloudArchiveReserveQuota` | 3 | `(ctx, w, owner string, total int64) (*quota.Settlement, int64, bool)` | `(settlement, pre, error)`（507 由 Kind 映射） |
| `cloudArchiveCreateTar` | 1 | `(w, groupFiles, root, rel, res, pre, groupID) (string, bool)` | `(checksum string, error)`；冲突/清理语义不变 |
| `cloudArchiveGroupToVolume` | 1 | `(w, r, groupID, owner string, groupFiles, req, archiveName)` | `(result, error)` |
| `cloudArchiveToVolume` | 3 | `(w, r, t archiveVolumeTransfer, logger *slog.Logger, onSuccess func(string))` | `(result, error)`；**顺序仍是 §8 的 17 步** |
| `recordBypassWrite` | 1 | `(owner, rel string, size int64, checksum, volume string, root *storage.Root)` | 包级纯逻辑 + `Targets.RecordBypass*` |
| `recordArchiveBypassState` | 1 | `(t archiveVolumeTransfer, fs sync.FS, rel string, size int64)` | 同上（meta.Provider 读 `TotalSHA256`） |
| `archiveTargetCharge` | 3 | `(volName, owner, fsRel, userRel string) int64` | 换 `Volumes.PhysFootprint` + `Volumes.TransferFS(Stat 回落)` |
| `archiveVolumeWriteGate` | 1 | `(ctx, fs sync.FS, owner, rel string, size int64) (func(), int, string)` | `(release func(), error)`；409/507 由 Kind 映射 |
| `recordArchiveTransferAudit` | 1 | `(ctx, t, scheme, rel string, count int, terr error)` | 换 `Audit.Record` |
| `cloudArchivePrepare` | 1 | `(w, r, taskID) (*cloud.CloudTask, CloudArchiveRequest, *storage.Tenant, string, bool)` | 返回 `(task, req, srcTenant, srcRel, error)` |
| `cloudArchiveOutput` | 2 | `(w, r, archiveName) (owner string, root *storage.Root, rel, name string, ok bool)` | 返回 `(root, rel, name, error)` |
| `normalizeArchiveName` | 4 | `(w, archiveName) (string, bool)` | `(string, error)`；Base/后缀/255 字节逐字保留 |
| `cloudArchiveTaskToVolume` | 1 | `(w, r, taskID, task, req, srcTnt, srcRel, info os.FileInfo, archiveName)` | `(result, error)` |
| `cloudArchiveBatchToVolume` | 1 | `(w, r, req, files, totalSize, archiveName)` | `(result, error)` |
| `cloudArchiveSettleQuota` | 3 | `(w, root, rel string, res *quota.Settlement, pre int64, logger) (int64, bool)` | `(actual int64, error)` |
| `cloudArchiveBatchParse` | 1 | `(w, r) (CloudArchiveBatchRequest, bool)` | HTTP 适配层 |
| `cloudArchiveCollectTasks` | 1 | `(w, r, taskIDs) (files, skipped []string, total int64, ok bool)` | 核心返回 `(files, skipped, total, error)` |
| `cloudArchiveTaskSource` | 1 | `(r, taskID) (fileWithRelPath, int64, bool)` | `(sourceFile, size, skip bool)`（keep skip 语义） |
| `recordArchiveUsage` | 3 | `(owner, archiveName string, actual int64)` | 包内状态 + `usageMu` |
| `deleteCloudArchive` | 1 | `(owner, archiveName string) error` | 原样（锁不嵌套，见 §8-R5） |
| `validateArchiveExtras` | 3 | `(w, ex archiveExtras) bool` | `(error)`（400） |
| `archiveBadRequest` | 2 | `(w, msg string) bool` | 删除（改 `OpError`） |
| `archivePreprocessor` | 1 | `(w, encrypt bool, cipher, compression string) (transfer.Preprocessor, bool)` | `(transfer.Preprocessor, error)` |

## 4. 非 `h.*` 的包级入边（同样必须显式处置）

| 符号 | 次数 | 定义 | 处置 |
|---|---|---|---|
| `sendJSONResponse` | **102** | `pkg/server/response.go:39` | **留装配层**（设计契约「HTTP 面留在装配层」）；核心改返回 `(result, error)` |
| `ActorFrom` | 23 | `pkg/server/auth.go:60`（ctx key 是包私有） | 留装配层；`owner` 显式传进核心（**不要**把 ctx→actor 塞进领域包） |
| `drainAndVerifyBody` | 8 | `pkg/server/auth.go:308` | 留装配层（请求体 EOF 哈希校验属 HTTP 面） |
| `msgInvalidRequestBody` / `msgBadRequest` / `msgNotFound` / `msgGroupNotFound` / `msgArchiveDirFail` / `msgArchiveStatFail` / `tarGZExt` | 34 | `pkg/server/errors.go:44-56` | 留装配层；文案逐字保留（测试断言 message） |
| `UploadResponse` | 8 | `pkg/server/response.go:29` | 留装配层 |
| `AuditEvent` + `AuditResultSuccess/Error` | 17 | `pkg/server/audit.go:23,27,38` | 装配层适配器把 5 字段转成 `AuditEvent`；8 处调用点均未用 Actor/Owner/Volume 字段（逐条核对） |
| `CloudBatchTaskResult` | 6 | `pkg/server/response.go:72` | 留装配层（batch 逐项结果结构是 HTTP 响应模型） |
| `normalizeOwner` | 2 | `pkg/server/handlers.go:263` | 迁入包内（`archiveUsage` 键必须与 `TenantFor`/`Quota` 同归一语义） |
| `releaseArchiveReservation` / `commitArchiveSettlement` | 7 / 1 | `pkg/server/quota_settlement_wiring.go:23/39` | **随迁**（只依赖 `quota`/`capacity`，不含 `h`） |
| `filesPhysFootprinter{h: h}` | 2 | `pkg/server/files_phys.go:27` | **并入 `Volumes.PhysFootprint`**（否则是隐藏的第 9 条入边） |
| `rootfs.OpenUserVisibleRead/UserVisibleSize/WriteUserVisible` | 6 | `pkg/rootfs`（R25 唯一用户可见 IO 入口） | 直接 import（G0 能力，不是装配态） |
| `transfer.Execute` / `archive.AddFileToTar` / `compressx.Parse` / `files.LookupCipher` | 4 | 领域包 | 直接 import，不进 Deps |

## 5. 「19 → 8」映射表（接口草案）

```go
// pkg/server/cloudtasks/deps.go
type Deps struct {
	Tasks   TaskStore       // 1. 任务面（含 2 个创建策略 getter）
	Groups  GroupStore      // 2. 组面
	Tenants TenantResolver  // 3. 租户/源解析
	Volumes VolumeAccess    // 4. 卷 ACL / 目标 FS 解析 / 本地卷池 / 物理足迹
	Quota   QuotaSettler    // 5. 配额 Scope + 预留/回退 + 等待上限
	Targets TargetSink      // 6. staging gate + 写锁 + 派生状态 + 3 个策略 getter
	Audit   AuditSink       // 7. 审计
	Logger  *slog.Logger    // 8. 日志
}
```

### 5.1 `TaskStore`（9 方法，覆盖 `h.cloudMgr` 的任务面）

```go
type TaskStore interface {
	AllowPrivate() bool
	MaxBatchURLs() int
	SubmitAndStart(method, url, filename string, totalSize int64,
		syncCtx context.Context, owner string, params cloud.TaskParams) (*cloud.CloudTask, error)
	SnapshotTask(id, owner string) (*cloud.CloudTask, bool)
	SnapshotTasks(ids []string, owner string) []*cloud.CloudTask
	ListTasks(status string, offset, limit int, owner string) ([]*cloud.CloudTask, int)
	CancelTask(id, owner string) error
	DeleteTask(id, owner string) error
	ResumeTask(id string, force bool, owner string) error
}
```

- 覆盖：`h.cloudMgr` 的任务侧 9 个方法（其余引用属 `GroupStore`）。
- 残余：`AllowPrivate/MaxBatchURLs` 是 manager 上的**配置 getter**（命名上属策略）；为省字段放在
  这里，理由：它们只服务任务创建校验。若将来有 `Policy` 字段，应迁出（见 §9）。
- 编译期断言：`var _ cloudtasks.TaskStore = (*cloud.CloudDownloadManager)(nil)`（S1）。

### 5.2 `GroupStore`（8 方法，覆盖 `h.cloudMgr` 的组面）

```go
type GroupStore interface {
	SubmitAndStartGroup(name string, urls []cloudfilename.Entry, owner string, params cloud.TaskParams) (*cloud.CloudTaskGroup, error)
	GetGroup(id, owner string) (*cloud.CloudTaskGroup, bool)
	ListGroups(status string, offset, limit int, owner string) ([]*cloud.CloudTaskGroup, int)
	CancelGroup(groupID, owner string) error
	DeleteGroup(groupID, owner string) error
	ResumeGroup(groupID string, force bool, owner string) error
	SetGroupArchiveFile(groupID, archiveFile string)
	UpdateGroupStatus(groupID string)
}
```

- 残余：`UpdateGroupStatus` 有副作用（刷新），调用顺序是语义的一部分：
  `GetGroup → UpdateGroupStatus → GetGroup`（owner 可见性先判，防计时侧信道），
  `ListGroups("", -1, 0) → 逐个 UpdateGroupStatus → ListGroups(status, offset, limit)`。
  S2 必须保留这两段顺序，测试要锁「刚转状态的组能被 status 过滤命中」。

### 5.3 `TenantResolver`（1 方法）

```go
type TenantResolver interface {
	TenantFor(owner string) *storage.Tenant // 空 owner → anonymous；失败 nil（fail-closed）
}
```

- 覆盖 `h.tenantFor`（6 处）。用途：源（`cloud/<taskID>/<file>`）与 archive 桶（`archive/<name>`）
  的 `FeatureRel` + `Root()`。
- 残余：`*storage.Tenant` 是具体类型（`Root()`/`FeatureRel()` 不是接口方法）。下沉窄接口收益低
  （读仍必须走 `rootfs` 唯一入口，R25），保留具体类型。

### 5.4 `VolumeAccess`（5 方法）

```go
type VolumeAccess interface {
	Allowed(owner, volume string) bool                                   // volumeAllowedFor（fail-closed）
	TransferFS(volume string) (fs sync.FS, scheme string, vol volume.Volume, ok bool) // externalVolumeTransferFS
	IsLocal(volume string) bool                                          // volSet.HasLocalRoot
	LocalPool(volume string) *quota.Pool                                 // volSet.Pool
	PhysFootprint(volume, owner, userRel string) (int64, bool)           // filesPhysFootprinter
}
```

- 覆盖：`volumeAllowedFor`、`externalVolumeTransferFS`、`volSet`、`filesPhysFootprinter`。
- 残余：`volume.Volume`（`ResolveUserLocation`）与 `*quota.Pool` 是领域具体类型，保留；
  P1-a `volume.Router` 落地后 `Allowed` 可直接换成 `Router.Allowed`，`PhysFootprint` 可下沉
  `pkg/volume`（届时本接口只剩 3 方法）。

### 5.5 `QuotaSettler`（6 方法）

```go
type QuotaSettler interface {
	BucketScope(owner, bucket string) *quota.Scope // quotaBucketFor
	ScopeFor(owner, rel string) *quota.Scope       // quotaScopeFor
	WaitTimeout() time.Duration                    // quotaWaitTimeout（0 = 30s 默认）

	// capacity.StorageManager 回退（scope == nil 时才用；nil = 未装配）
	ReserveFallback(ctx context.Context, n int64, timeout time.Duration) error
	ReleaseFallback(n int64)
	TryReserveFallback(n int64) error
}
```

- 覆盖：`quotaBucketFor`、`quotaScopeFor`、`quotaWaitTimeout`、`storageMgr`。
- 残余：`quota.Settlement` 由包内直接用 `quota.NewSettlement(scope.Pool())` 构造（领域类型，
  不进 Deps）；三个 Fallback 方法在装配层适配器内**绑定 `capacity.CategoryCloud`**，避免
  `cloudtasks` 直连 `pkg/storage/capacity` 子包（R2 虽放行装配作用域，但窄接口更可测）。

### 5.6 `TargetSink`（7 方法）

```go
type TargetSink interface {
	StagingFS(owner string, fs sync.FS) sync.FS                    // stagingQuotaFS
	Lock(owner, fsKey string) (release func(), ok bool)            // transferWriteLock（键归一 + 排他锁）
	RecordBypassChecksum(owner, rel, checksum, volume string)      // files.Service.RecordBypassChecksum
	RecordBypassWrite(owner, rel string, size int64, checksum, volume string, root *storage.Root) // files.Service.RecordBypassWrite
	SkipVerify() bool                                              // verifySkipped
	MaxArchiveBytes() int64                                        // cfgPtr.Load().CloudArchiveMaxBytes
	ArchiveKey() []byte                                            // archiveKey（<32B → 调用方 400）
}
```

- 覆盖：`stagingQuotaFS`、`transferWriteLock`、`fileService`、`verifySkipped`、`cfgPtr`、`archiveKey`。
- 残余：`RecordBypassWrite` 需要 `*storage.Root`（取 modtime）；`Lock` 的 `fsKey` 归一
  （剥 `<owner>/` 前缀）仍由适配器完成，核心只传 `loc.FSPath()`。
- 设计债（显式记录）：3 个策略 getter 与 4 个写能力混在一个接口里，是「为 8 字段压缩」的
  折中；见 §9 的取舍。

### 5.7 `AuditSink`（1 方法）

```go
type AuditSink interface {
	Record(ctx context.Context, action, objectType, object, result, detail string)
}
```

- 覆盖 `h.RecordAudit` 的 8 处（均只用这 5 个字段）。与 `pkg/server/verify.Deps.Audit` 同形
  （`pkg/server/verify/service.go`），是既有模式。
- 残余：`AuditEvent.Actor/Mesh/Owner/Volume/RequestID/SourceIP` 的 ctx 自动填充保留在装配层
  适配器（`RecordAudit` 内部完成），核心不感知。

### 5.8 `Logger`

`*slog.Logger`（直接字段；`nil` → 装配层注入 `slog.Default()`，不要核心自己兜底）。

### 5.9 覆盖核对（19/19）

| 接口 | 覆盖的原依赖 |
|---|---|
| `Tasks` | `cloudMgr`（任务侧） |
| `Groups` | `cloudMgr`（组侧） |
| `Tenants` | `tenantFor` |
| `Volumes` | `volumeAllowedFor`、`externalVolumeTransferFS`、`volSet`、`filesPhysFootprinter` |
| `Quota` | `quotaBucketFor`、`quotaScopeFor`、`quotaWaitTimeout`、`storageMgr` |
| `Targets` | `stagingQuotaFS`、`transferWriteLock`、`fileService`、`verifySkipped`、`cfgPtr`、`archiveKey` |
| `Audit` | `RecordAudit` |
| `Logger` | `logger` |
| **包内状态（不入 Deps）** | `tenantMu`、`archiveUsage`（随迁自持锁） |

## 6. 无法覆盖的残余项（及原因）

1. **HTTP 响应/状态码（102 处）**：设计契约是「HTTP 面留在装配层」；若把 `sendJSONResponse`
   也注入，会多出第 9 字段并把 HTTP 语义塞进领域包。处置：核心方法返回 `(result, error)`，
   错误用语义 Kind（见 §7 的 S1），装配层单点映射状态码。
2. **`ActorFrom`（23 处）**：ctx key 是 `pkg/server` 私有（`auth.go:46`），领域包拿不到；
   处置：装配层取 owner 后显式传参（这也是「owner 显式化」的收益）。
3. **`drainAndVerifyBody`（8 处）**：请求体 EOF 哈希校验是 HTTP 中间件语义，留装配层。
4. **`msg*`/`tarGZExt`（34 处）与响应结构（`UploadResponse`/`CloudBatchTaskResult`）**：属 HTTP
   响应模型，留装配层；文案逐字保留（现有测试断言 message）。
5. **`filesPhysFootprinter`（2 处）**：它不是 `h.*` 形态但依赖 `Handlers`；**必须**并入
   `Volumes.PhysFootprint`，否则 Deps 是 8、实际入边是 9（隐藏耦合）。
6. **`archiveUsage` + `tenantMu`（12 处）**：不是能力而是**包内状态**。随迁后由
   `usageMu sync.Mutex` 保护；与 `Quota.BucketScope` 的调用**不得嵌套加锁**（见 §8 的 R5）。
7. **`quota.Settlement` / `transfer.Execute` / `rootfs.*` / `archive.AddFileToTar`**：领域/基础
   能力，直接 import 比注入更清晰（不是装配态，也无测试替身需求）。
8. **`groupErrorStatus` 的字符串匹配**（`filename conflict`/`duplicate URL`/`语义空洞`/`未装配`/
   `协议未声明`/`是 secrets 密钥卷`）：现状是脆弱契约。S2 可保留字符串匹配（零行为变化），
   或换 typed error——**换必须逐条加测试锁住原状态码**，不得「顺手改语义」。

## 7. 实施拆分（S1–S4）

### S1：接口 + 适配器 + 编译期断言（零行为变化）

- 新增 `pkg/server/cloudtasks/deps.go`（8 字段 + 7 接口）
- 新增 `pkg/server/cloudtasks/errors.go`（`Kind` + `OpError{Kind, Message}`；Kind：`BadRequest`/
  `Forbidden`/`NotFound`/`Conflict`/`TooLarge`/`InsufficientStorage`/`Internal`/`Unsupported`）
- 新增 `pkg/server/cloudtasks_adapter.go`（装配层）：把 `h` 的 19 项能力包成 8 个接口；
  `newCloudTasks(h) *cloudtasks.Orchestrator`
- 修改 `internal/archcheck/layers.go`：`Levels["pkg/server/cloudtasks"] = 2` +
  `ParentDomain[...] = "pkg/server"`（R6 强制 Levels；R2 强制 ParentDomain）
- 新增 `pkg/server/cloudtasks/deps_test.go`：`reflect.TypeOf(Deps{}).NumField() <= 8`（补 R52 的
  12 缺口）+ 编译期断言 `var _ TaskStore = (*cloud.CloudDownloadManager)(nil)` 等
- **验收**：`go build ./...`；`go test ./pkg/server/cloudtasks`；
  `go test ./internal/archcheck -run 'TestR52|TestR6|TestSubpackageVisibility|TestLayeringDirection'`；
  `make lint`

### S2：迁移 download 路径（任务 + 组）

- 迁入 `pkg/server/cloudtasks/download.go`、`group.go`：16 个入口中 14 个（download 单条/批量/
  列表/详情/取消/删除/恢复 + 组 CRUD）的**核心逻辑**
- 随迁纯函数：`saveOrDefault`、`isStorageFull`、`validateCloudDownloadURL`、`normalizeGroupURLs`、
  `groupErrorStatus`、`withShareTarget`、`parseOffsetLimit`
- 留在装配层：14 个 HTTP 壳（decode/`drainAndVerifyBody`/`ActorFrom`/状态映射/JSON）+ `msg*` 常量
- 修改：`pkg/server/cloud_download_handler.go`（入口瘦身）、`cloudtasks_adapter.go`
- **验收**：既有 `TestCloudHandler_*`（33）、`TestCloudOwner_*`（12）、`cloud_url_validation_test.go`
  全绿且**断言不改**；新增 `cloudtasks/download_test.go`/`group_test.go`（fake store，纯标准库，
  `t.Parallel`）锁：owner 过滤参数逐字透传、ACL 403、507、409、**batch 传 `nil` syncCtx**、
  快照而非指针。

### S3：迁移 archive 路径（单任务 / 批量 / 组 / 转存）

- 迁入 `pkg/server/cloudtasks/archive.go`、`transfer.go`、`tar.go`：`cloudArchiveTask/Batch/Group`、
  `cloudArchiveToVolume` 共享核心、reserve/settle、usage 登记、preprocessor、tar 打包
- 随迁包级函数：`createTarGz`、`createMultiFileTarGz`、`openArchiveOutput`、`archiveEntries`、
  `closeEntries`、`archiveTotalSize`、`archiveTargetRel`、`relOf`、`userRelFromFSKey`、
  `probeArchiveQuota`、`settleArchiveCharge`、`releaseArchiveReservation`、
  `commitArchiveSettlement`
- 状态随迁：`archiveUsage` + 包内 `usageMu`；删除 `deps.go:245-248` 字段与
  `routes_setup.go:194` 初始化
- 修改：`cloud_archive_handler.go`、`cloud_download_handler.go`（归档段）、`pkg/server/deps.go`、
  `pkg/server/routes_setup.go`
- **验收**：既有 `TestCloudArchive_*`（23）、`cloud_archive_local_test.go`（2）、
  `cloud_archive_quota_settle_test.go`（4）、`cloud_transfer_settle_test.go`（1）、
  `cloud_archive_download_test.go`（12）全绿；新增 `cloudtasks/archive_test.go` 覆盖四条主路径
  （单任务/批量/组/到卷）+ 失败回滚（O_EXCL 冲突、tar 失败、预留释放、storageMgr 三分支）+
  覆盖写差分（before/after）+ 写前探针 507
- 门禁账本：`internal/archcheck/quota_acquire_sites.tsv` 增 `pkg/server/cloudtasks/... 1 no probe`
  （`ProbeReserve`）；`audit_calls.tsv` 按新文件登记（注意 §10 的 R35-a 扫描面观察）

### S4：删除旧胶水 + 文档 + 门禁

- 删除：`deps.go` 的 `archiveUsage`、`routes_setup.go` 初始化、装配层已无引用的旧 helper
- 文档：`docs/architecture.md` 能力矩阵增 `pkg/server/cloudtasks` 行；`docs/designs/2026-10-31-policy-object-extraction.md`
  的 P1-c 状态标注（AGENTS.md 能力矩阵由主线决定，本片不改 AGENTS.md）
- **验收**：`git grep -n 'h\.archiveUsage'` 空；`go test ./pkg/server/... ./pkg/cloud/...`；
  `make deadcode-check`；`go test ./internal/archcheck/`；`make check-ci`；e2e
  （`test/e2e_restart_cloud_test.go` 等 cloud 相关）

### 7.1 预计删除/新增行数

- 两个 handler 文件：2165 行 → 估计 900–1100 行（仅剩 HTTP 适配 + 请求/响应模型）
- `pkg/server/cloudtasks`：估计 1800–2200 行（含 8 接口/错误/4 个编排文件/tar 助手）

## 8. 必须逐字等价的风险点

> 判定标准：迁移后上述路径的**调用顺序、锁窗口、账本差分、错误文案/状态码**任一变化，
> 都必须先有测试证明是「有意变更」，否则视为回归。

### R1 配额结算顺序（5 条路径，最易错）

1. **预留**（`cloudArchiveReserveQuota`，d:892）：
   - `pre = totalSourceSize + 100MiB`（`cloudArchiveReservePlaceholder`，a:75）
   - `scope != nil` → `quota.NewSettlement(scope.Pool())` + `ReserveWait(ctx, pre, h.quotaWaitTimeout)`
   - `scope == nil && storageMgr != nil` → `WaitReserve(ctx, pre, CategoryCloud, capacity.DefaultWaitTimeout)`
     （**注意这里用的是 `DefaultWaitTimeout`，不是 `h.quotaWaitTimeout`**——不对称是现状，不得「顺手统一」）
   - `scope == nil && storageMgr == nil` → 不预留（`pre` 仍返回）
2. **成功对账**（`cloudArchiveSettleQuota`，a:456）：
   - scope 路径：`Stat` 失败 → `res.Release()` + 返回 `(0, true)`（文件已落盘，账本交给周期扫描）
     ——**不是** `Commit`
   - 正常：`Reconcile(actual)`（失败仅 `logger.Error`，**仍 `Commit`**）→ `Commit()`
   - 回退路径：`Release(pre, CategoryCloud)` → `Stat` → `TryReserve(actual, CategoryCloud)`
     失败 → 删除归档 + 507
3. **失败回滚**（`releaseArchiveReservation`，`quota_settlement_wiring.go:23`）：
   - O_EXCL 同名冲突（`!created`）与 tar/打包失败两条分支都必须释放预留，且**只释放一次**
     （`Settlement.Release` 幂等；回退路径按 `pre` 释放）
4. **转存路径不占 archive 桶**（`cloudArchiveToVolume`，d:1001）：
   - `ProbeReserve(scope, t.TotalSize)`（写前探针：预留后立即释放、不排队；`size<=0`/`scope==nil` 跳过）
   - 写成功：`chargeAfter = archiveTargetCharge(...)` → `settleArchiveCharge(scope, before, after)`
     （**差分 `Adjust`，不是按 after 全额**）→ `settleVolumePoolCharge`（仅本地卷 `pool.Adjust`）
5. **不变量**：每个预留句柄必须以 `Commit` **或** `Release` 收尾；任何「一个分支既 Commit 又
   Release」或「stat 失败仍 Commit」都是回归（现有 `cloud_archive_quota_settle_test.go` 已锁
   部分语义，S3 必须补全 5 条路径）。

### R2 `storageMgr` 回退

- 只在 `scope == nil` 时使用（生产 scope 恒非 nil）；**禁止**恢复历史「双轨同时预留」
  （d:898-899 注释明确记录该收敛）。
- `storageMgr == nil` + `scope == nil`：无预留、无释放（`releaseArchiveReservation(nil,nil,pre)` 空操作）。
- `CloudArchive` 分类固定 `capacity.CategoryCloud`（适配器内绑定，不暴露给核心）。

### R3 卷路由 / ACL

- 4 个 ACL 点（d:54、d:707、a:400、a:433）逐字：`volSet == nil || !ByName || !Authorize` → 403，
  文案「目标卷对当前用户不可用（ACL 拒绝）」/「转存目标卷 %q 对当前用户不可用（ACL 拒绝）」。
- `externalVolumeTransferFS` 的返回语义：`volSet == nil || VolumeView 缺失` → `ok=false`；
  `ByName` 缺失但视图存在 → `fs` 非 nil、`scheme == ""`、`ok == false`（调用方按 `ok` 判 404）；
  `scheme == ""` → 归一 `"local"`；`scheme == "secrets"` → 400。
- 顺序：`validateArchiveExtras` → ACL → `normalizeArchiveName` → 打开源 → 目标解析 → 写门。
  ACL 必须**早于**任何源文件打开（否则会泄漏 403 之外的错误面）。

### R4 写锁（`transferWriteLock`）

- 键空间：`lockRelFor(owner, fsKey)` 剥 `<owner>/` 前缀，与 HTTP 上传/删除/rename/DAV/S3 同键
  （`uploading_lock.go:78-101`）；核心只传 `loc.FSPath()`。
- 顺序：`ReserveSpace`（可选能力，`ErrUnsupported` 跳过）→ **取锁**；锁失败 → 409
  「目标正被其他写者占用，请稍后重试」。
- 持锁窗口：`defer release()` 覆盖 `transfer.Execute` + 审计 + 配额结算 + 派生状态登记
  ——**不得**在写成功后提前释放（否则同 rel 并发写可插入未记账窗口）。

### R5 `tenantMu` / `archiveUsage` 并发语义

- 现状：`archiveUsage` 由 `h.tenantMu` 保护（与 quota lazy map 同一把锁）；`recordArchiveUsage`
  （a:711）全程持锁写 map。
- `deleteCloudArchive`（a:732）的加锁顺序：stat/remove **不持锁** → 持锁读登记 →
  **释放锁** → `quotaBucketFor`（内部再取 `tenantMu`）→ 持锁删登记。**不能**在持锁状态调用
  `Quota.BucketScope`（Go 的 `sync.Mutex` 不可重入 → 自死锁）。
- 迁移后：包内 `usageMu` 与 `Quota` 的锁是两把独立锁；保持「先 usageMu 取值 → 释放 → 调
  Quota → 再 usageMu 删」的顺序，不得嵌套。
- `TenantFor` 自身不取 `tenantMu`（`TenantCache` 有独立锁），可安全在包内任意调用。

### R6 派生状态登记 / 审计顺序

- 写成功后的固定顺序（d:1108-1120）：
  `chargeAfter` → `settleArchiveCharge` → `settleVolumePoolCharge` → `recordArchiveBypassState`
  → `onSuccess(url)`（组归档写 `SetGroupArchiveFile`）→ 响应。
- `recordArchiveBypassState` 只在**本地卷**执行，且 checksum 从 `meta.Provider.FileMeta().TotalSHA256`
  读（fs 可能是装饰链，必须实现 `meta.Provider`）。
- `recordArchiveTransferAudit` 在**错误处理之前**调用（成功/失败各记一次，含 `scheme/key/files/encrypt`）。
- `recordBypassWrite` 分流（d:1131）：本地卷 → 只 `RecordBypassChecksum`；外部卷 →
  `RecordBypassWrite`（checksum + 索引）。分流判据是 `HasLocalRoot`，不得改成「卷类型枚举」。

### R7 HTTP 状态码映射（必须逐条保留的「非直觉」项）

| 场景 | 状态码 | 证据 |
|---|---|---|
| 删除任务失败（任何错误） | **404**（不是 400/500） | d:315-333 |
| 取消/恢复任务错误含 `not found` | 404，其它 400 | d:293-357 |
| 组创建：`filename conflict` / `duplicate URL` | 409 | d:450-465 |
| 组创建：存储不足 | 507 | 同上 |
| 组创建：`语义空洞`/`未装配`/`协议未声明`/`是 secrets 密钥卷` | 400 | 同上 |
| 组创建：其它 | 500 | 同上 |
| 归档：目标卷未装配 | 404 | d:1017-1021 |
| 归档：目标卷是 secrets | 400 | d:1023-1028 |
| 归档：转存 `transfer.ErrExists` | 409 | d:1096-1100 |
| 归档：O_EXCL 同名 | 409 | a:160-166、d:936-942 |
| 归档：`max_bytes` 超限 | 400 | d:877-890 |
| 归档：预留/追加预留/探针失败 | 507 | d:892-927、a:466-487 |
| 归档：写锁占用 | 409 | d:1207-1222 |

### R8 其它逐字等价

- **batch 的 `nil` syncCtx**（d:220 注释与调用）：批量恒异步；改成 `r.Context()` 会让客户端
  断连取消批量任务（语义回归）。
- **默认归档名**三种格式：`cloud-task-%s-%d.tar.gz`、`cloud-batch-%d.tar.gz`、
  `%s-%d.tar.gz`（组）；`normalizeArchiveName` 对它们幂等（已带 `.tar.gz`）。
- **`cloudArchiveTask` 的 `Size` 用重新 `Stat` 的结果**（a:186-197），而批量/组用 `actual`
  ——现状不一致；迁移保持原样，若要统一必须单独测试 + 说明。
- **`cloudArchiveMaxBytes`/`verifySkipped`/`archiveKey` 每次调用都读配置/文件**（支持热更新，
  且 `archiveKey` 读盘失败返回 nil → 400）；不得在构造期缓存成字段。
- **`normalizeArchiveName`**：`filepath.Base` → 拒 `.`/`..`/空 → 补 `.tar.gz` → `len > 255` 拒绝。
- **`recordArchiveUsage` 的 `actual <= 0` 不登记**；`deleteCloudArchive` 在文件已不存在时
  按登记值释放（`TestCloudArchive_Delete_FileAlreadyGoneReleasesFromRegistry` 已锁）。
- **owner 归一**：`TenantFor`/`Quota.*` 内部 `normalizeOwner`；包内 `archiveUsage` 键也必须
  归一（现状 a:715 显式 `normalizeOwner`），空 owner 与 `anonymous` 必须同键。

## 9. 若 8 个装不下：取舍顺序

1. **合并 `Tasks` + `Groups` → `TaskStore`（17 方法，−1 字段）**：语义可接受（都是 cloud manager
   的任务面），代价是测试替身变宽、单测更容易写成「大而全」。**首选取舍**。
2. **把 3 个策略 getter（`SkipVerify`/`MaxArchiveBytes`/`ArchiveKey`）从 `Targets` 挪到
   `TaskStore`**：不推荐（任务面与归档策略无关），且只是把债换位置。
3. **`Volumes.PhysFootprint` 下沉 `pkg/volume`**（P1-a 完成后）：能从接口移除 1 方法，
   但不能减少字段数（`Volumes` 仍在）。
4. **把 `Tenants` 并入 `Volumes`（+1 方法）**：可省 1 字段，但「租户解析」与「卷路由」是
   两个语义；若必须压到 7 字段可选此项（比合并 Tasks/Groups 更伤可读性）。
5. **允许留在装配层的**（不进 Deps）：HTTP 面全部（`sendJSONResponse`/`ActorFrom`/body 校验/
   `msg*`/响应结构）、`archiveUsage` 状态、`filesPhysFootprinter`（并入 `Volumes`）、
   `releaseArchiveReservation`/`commitArchiveSettlement`（随迁）。
6. **禁止的做法**：把 `Logger` 改成 `New(d Deps, logger *slog.Logger)` 绕开字段计数、上调
   `depsFieldCap`、或新增「适配器包级变量」注入。R52 的语义是「Deps 字段 = 入边宽度」，
   绕过去等于自欺。真需要第 9 项时，**拆包**（如 `cloudtasks/archive`）而不是放宽门禁。

## 10. 遗留观察（与本次迁移相关，非本片修复）

- **R52 上限是 12**（`deps_field_cap_test.go:34`），设计目标是 8 ⇒ S1 的包内断言是唯一守卫。
- **R35-a（审计调用点棘轮）扫描面疑似漏掉 `pkg/**`**：`audit_calls_gate_test.go:69-70` 以
  `filepath.WalkDir("..")` 为根，而 `go test` 的 cwd 是 `internal/archcheck` ⇒ 实际扫描
  `internal/`。证据：`pkg/server/cloud_download_handler.go` 现有 8 处 `.RecordAudit(`，
  基线 `audit_calls.tsv:17` 写 7，但 `go test ./internal/archcheck -run TestAuditCallSites_Ratchet`
  PASS。迁移后**不能**依赖该棘轮守 `cloudtasks` 的审计调用点（需人工核对 + 单测锁）。
- `serial_budgets.tsv` 已登记两个测试文件的串行预算（`cloud_archive_handler_test.go 13`、
  `cloud_download_handler_test.go 26`）：若 S2/S3 把测试也搬进新包，必须同步 TSV 并保持
  全仓总数只减不增。

## 10.1 交付结果（2026-10-31，S1–S4 全部合并）

| 阶段 | PR | 结果 |
|---|---|---|
| S1 接口契约 | #968 | 7 接口 / 37 方法 / `Deps` 8 字段（`Volumes.PhysFootprint` 收口隐藏入边） |
| S2a 任务 CRUD | #970 | `cloud_download_handler.go` 1357 → 1027 |
| S2b 组 | #971 | 1027 → 787（新增 `Groups.DeleteGroupArchive` 过渡方法） |
| S3a 归档 task/batch | #972 | `cloud_archive_handler.go` 839 → 411 |
| S3b 组归档 | #973 | `pkg/server` 31,959 |
| S3c 转存核心 | #974 | `cloud_download_handler.go` 470 → 68；接口 48 → 42；删除 6 个过渡 seam |
| S4 tar 助手收尾 | #975 | `cloud_archive_handler.go` 240 → 106；接口 42 → **40**；`Targets` 9 → 7 |

**最终形态**：`Deps` 恒为 **8 字段**（7 窄接口 + Logger），接口方法合计 **40**；
`pkg/server` 非测试行数 **42,568 → 31,221（−26.6%）**，新增 `pkg/server/cloudtasks` 约 2,100 行。

**保留在装配层的部分（有意为之）**：
- `archiveReserveQuota` / `archiveSettleQuota`：失败面是 HTTP 状态码 + 中文文案，属装配语义；
- `cloudTaskDeps` / `cloudTaskService` / `cloudTaskHandler`：依赖装配与 `ActorFrom` 适配（owner 显式传入核心）。

**已删除的过渡 seam**（不再存在于 `Deps` 接口）：`Targets.ArchiveToVolume`、`Targets.ValidateArchiveExtras`、
`Targets.ArchiveEntries`、`Targets.ArchiveTotalSize`、`Targets.CreateArchiveTar`、`Targets.CreateMultiArchiveTar`、
`Quota.RecordArchiveUsage`、`Groups.DeleteGroupArchive`。

**教训**：`Deps` 字段数是装配耦合的度量（守卫 ≤8）；能力增长应体现为**接口方法**增加，而不是字段增加。

## 11. 关键引用（file:line）

- `pkg/server/cloud_download_handler.go:50/589/631/642/661/759/846/892/1001/1131/1183/1207/1224`
- `pkg/server/cloud_archive_handler.go:85/296/345/377/456/711/732/787/808`
- `pkg/server/handlers.go:197/281/568/614`；`pkg/server/deps.go:245-249`
- `pkg/server/uploading_lock.go:92/101`；`pkg/server/volumes.go:733/763/800`
- `pkg/server/volumes_api.go:428`；`pkg/server/audit.go:119`；`pkg/server/archive.go:49`
- `pkg/server/files_phys.go:27/75`；`pkg/server/quota_settlement_wiring.go:23/39`
- `pkg/quota/settlement.go:93/130/174/201/224`；`pkg/storage/capacity/wait.go:100`
- `internal/archcheck/deps_field_cap_test.go:34`；`internal/archcheck/layers.go`（Levels/ParentDomain）
