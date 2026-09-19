// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// manager.go 是云下载管理器的**核心定义**：领域类型（任务/组/配置）、依赖解析器接口、
// 管理器结构体与其构造、路径与配额接缝、以及装配层需要的只读访问器。
//
// D3 拆分（2026-09-14）：本文件原为 2328 行单文件，按职责切为 6 个同包文件（**零 API 变更**，
// 纯代码搬迁）：manager.go（本文件：类型/构造/接缝/访问器）、manager_task.go（任务创建与执行）、
// manager_query.go（任务查询与取消/删除）、manager_persist.go（持久化/恢复/清理/ID）、
// manager_group.go（任务组）、manager_lifecycle.go（等待停止/续传/脏刷/关闭）。

package cloud

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/internal/slogutil"
	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// CloudTask 表示一个云端下载任务。
// Owner 是任务级多租户隔离字段（阶段 6 工作项 C）：创建时由请求 AK 派生
// （SproxySig → AK；api_keys → key 名；未认证 → 空串）。过滤规则见 ownerVisible：
// 空 owner（全局/旧任务/未认证创建）对所有人可见；非空 owner 只对匹配用户
// （或空 owner 的管理员/未认证）可见。List/Get/Cancel/Delete/Resume 均按 owner 过滤，
// 跨 owner 视为不存在（404 防枚举）。
type CloudTask struct {
	ID           string    `json:"id"`
	Owner        string    `json:"owner,omitempty"` // 任务归属（创建者 AK / API key 名；空 = 全局兼容）
	URL          string    `json:"url"`
	Method       string    `json:"method"`     // "url" | "upload"
	Filename     string    `json:"filename"`   // 云端存储文件名
	Status       string    `json:"status"`     // pending | downloading | completed | failed | cancelled
	TotalSize    int64     `json:"total_size"` // -1 表示未知
	Downloaded   int64     `json:"downloaded"`
	Checksum     string    `json:"checksum"`
	ETag         string    `json:"etag,omitempty"`       // 服务端 ETag，用于版本标识与二次校验（可能为空）
	FileMTime    int64     `json:"file_mtime,omitempty"` // 原始文件修改时间（UnixNano），从 URL 的 Last-Modified 提取
	Error        string    `json:"error"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	ReservedSize int64     `json:"-"`                  // 实际预留量（storageMgr + Scope），不持久化
	GroupID      string    `json:"group_id,omitempty"` // 所属组 ID（可选）

	// 以下为 P4 租户配额（Scope）运行时状态，不持久化（json:"-"）。
	// account 是本任务在 Scope 中的配额唯一所有权（TaskAccount：reserved+committed），
	// 由 downloadSinkFactory 创建/复用（跨重试/续传保留同一 account），终态/删除时
	// releaseTaskScope 收敛为 account.Release（幂等归零）。nil = scope 未装配（仅全局账本）。
	// 不持久化：重启恢复的任务由磁盘扫描校准（Restored 语义），不再重建 account。
	account *quota.TaskAccount `json:"-"`
}

// CloudTaskGroup 表示一个云端下载任务组。
// 每个子任务仍是独立的 CloudTask（文件保存在租户 cloud 桶 <root>/<tenant>/cloud/<taskID>/ 下），
// 组只负责聚合元数据与组级操作（归档/取消/恢复）。
// Owner 与子任务一致（创建组的请求 AK 派生），组级 List/Get/Cancel/Delete 按 owner 过滤。
type CloudTaskGroup struct {
	ID          string    `json:"id"`
	Owner       string    `json:"owner,omitempty"` // 组归属（创建者 AK / API key 名；空 = 全局兼容）
	Name        string    `json:"name"`
	Status      string    `json:"status"` // downloading | completed | failed | cancelled
	TaskIDs     []string  `json:"task_ids"`
	TotalTasks  int       `json:"total_tasks"`
	Completed   int       `json:"completed"`
	Failed      int       `json:"failed"`
	Cancelled   int       `json:"cancelled"`
	Error       string    `json:"error,omitempty"`
	ArchiveFile string    `json:"archive_file,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// CloudDownloadConfig 云端下载配置。
type CloudDownloadConfig struct {
	SyncThreshold   int64         // 同步模式阈值（字节），默认 20 MiB
	MaxConcurrent   int           // 最大并发下载数，默认 3
	MaxBatchURLs    int           // 批量/组下载单次最大 URL 数，默认 100；0 使用默认值
	TaskTTL         time.Duration // 完成任务保留时间，默认 24h
	FailedTaskTTL   time.Duration // 失败任务保留时间，默认 1h
	AllowPrivate    bool          // 允许私有 IP 下载（仅测试用）
	DownloadTimeout time.Duration // 单次下载尝试整体超时，默认 30m；0 表示不限制
	IdleTimeout     time.Duration // 响应体读取空闲超时，默认 60s；0 表示不限制
	MaxRetries      int           // 失败重试次数，默认 10
	RetryDelay      time.Duration // 重试间隔，默认 10s
	Downloader      string        // 下载器名称，默认 "http"（配置 cloud_downloader 后生效）
}

// cloudReservePlaceholder 未知大小任务的存储占位大小（1 GiB）。
const cloudReservePlaceholder = int64(1024 * 1024 * 1024)

// ownerVisible 判定任务 owner 对请求者 owner 是否可见（多租户隔离规则，与
// syncmgr.ownerVisible 一致）：
//   - 请求者 owner 为空（管理员/未认证）→ 可见全部；
//   - 请求者 owner 非空 → 任务 owner 为空（全局/旧任务，兼容所有用户）或与请求者一致才可见。
//
// 空 owner 任务对所有人可见是刻意设计：不破坏现有未认证/单用户部署（旧任务与
// 未认证创建的任务无归属），且让空 owner 请求者承担管理员语义。
func ownerVisible(taskOwner, reqOwner string) bool {
	if reqOwner == "" {
		return true
	}
	return taskOwner == "" || taskOwner == reqOwner
}

// applyCloudConfigDefaults 为 CloudDownloadConfig 的零值字段填充默认值。
func applyCloudConfigDefaults(cfg *CloudDownloadConfig) {
	if cfg.SyncThreshold <= 0 {
		cfg.SyncThreshold = 20 * 1024 * 1024
	}
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 3
	}
	if cfg.MaxBatchURLs == 0 {
		cfg.MaxBatchURLs = 100
	}
	if cfg.TaskTTL <= 0 {
		cfg.TaskTTL = 24 * time.Hour
	}
	if cfg.FailedTaskTTL <= 0 {
		cfg.FailedTaskTTL = 1 * time.Hour
	}
	if cfg.DownloadTimeout <= 0 {
		cfg.DownloadTimeout = 30 * time.Minute
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 1 * time.Minute
	}
	if cfg.MaxRetries < 1 {
		cfg.MaxRetries = 10
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 10 * time.Second
	}
	if cfg.Downloader == "" {
		cfg.Downloader = "http"
	}
}

// TenantResolver 按 owner 返回租户（空 owner → anonymous；非法 owner 或存储根不可用返回 nil）。
// 与 Handlers.tenantFor 同签名，RegisterRoutes 装配时直接传 h.tenantFor。
type TenantResolver func(owner string) *storage.Tenant

// ChecksumResolver 按 owner 返回 per-tenant checksum 存储（不可用返回 nil）。
// 与 Handlers.checksumStoreFor 同签名，RegisterRoutes 装配时直接传 h.checksumStoreFor。
type ChecksumResolver func(owner string) *checksum.ChecksumStore

// QuotaResolver 按 owner 返回租户配额 Scope（未装配返回 nil）。
// 与 Handlers.quotaFor 同签名，RegisterRoutes 装配时直接传 h.quotaFor。
type QuotaResolver func(owner string) *quota.Scope

// StorageManager 是云下载域需要的**容量核算**能力（消费者定义接口）。
//
// 为什么是接口而不是直接 import `pkg/storage/capacity`：门禁 R2（子包可见性）规定
// `pkg/storage/capacity` 只允许 `pkg/storage` 子树与装配层导入——本包是**另一个领域**，
// 不得直接依赖它。装配层的适配器（pkg/server 的 cloudStorageManager）把类别固定为
// `capacity.CategoryCloud` 后注入，领域侧只表达「云下载桶的预留/归还」。
type StorageManager interface {
	TryReserveCloud(size int64) error
	ReleaseCloud(size int64)
	Usage() int64
	MaxBytes() int64
}

type CloudDownloadManager struct {
	tasks            map[string]*CloudTask
	mu               sync.RWMutex
	uploadsDir       string
	tenantFor        TenantResolver   // 按任务 owner 解析租户（空 owner → anonymous）
	checksumStoreFor ChecksumResolver // 按任务 owner 解析 per-tenant checksum 存储
	quotaFor         QuotaResolver    // 按任务 owner 解析租户配额 Scope（nil = 未装配，仅全局账本）
	listTenants      func() []string  // 返回全部租户名（恢复扫描；磁盘扫描，非内存缓存）
	storage          StorageManager
	logger           *slog.Logger
	semaphore        chan struct{}
	config           *CloudDownloadConfig
	dl               downloader.Downloader
	cancelFuncs      map[string]context.CancelFunc // 任务取消函数
	running          map[string]bool               // 任务是否有执行中的下载 goroutine（含排队）
	metrics          *CloudMetrics

	// 批量持久化进度更新
	dirtyTasks  map[string]struct{}
	dirtyMu     sync.Mutex
	flushNow    chan struct{}
	stopFlush   chan struct{}
	stopCleanup chan struct{}  // 停止 cleanupExpired 后台 goroutine
	wg          sync.WaitGroup // 追踪所有执行中的 goroutine
	closeOnce   sync.Once      // 确保 Close 只执行一次

	// TaskGroup 支持
	groups  map[string]*CloudTaskGroup
	groupMu sync.RWMutex
	// groupSaveMu 串行化组持久化：saveGroup 的 marshal 与 write 必须原子（相对彼此），
	// 否则一个持有旧快照的保存可能在更新保存之后落盘，导致重启恢复出陈旧组状态
	// （Completed/Status 回退，TestCloudDownloadManager_GroupLifecycleAndPersistence 偶发 flake）。
	groupSaveMu sync.Mutex
}

// CloudMetrics 云端下载 Prometheus 指标。
type CloudMetrics struct {
	TasksCreated    atomic.Int64 // 创建的任务总数
	TasksCompleted  atomic.Int64 // 完成的任务数
	TasksFailed     atomic.Int64 // 失败的任务数
	TasksCancelled  atomic.Int64 // 取消的任务数
	TasksRetried    atomic.Int64 // 重试的任务数
	BytesDownloaded atomic.Int64 // 云端下载总字节数
	ActiveDownloads atomic.Int64 // 当前活跃下载数
}

// recoveryGuard 包装一个需要 panic recovery 的 goroutine 循环函数。
// 当 fn 发生 panic 时，记录日志并重新启动（除非 stopCh 已关闭）。
func recoveryGuard(name string, logger *slog.Logger, wg *sync.WaitGroup, stopCh <-chan struct{}, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("goroutine panicked, restarting", "name", name, "panic", r)
			select {
			case <-stopCh:
				return
			case <-time.After(10 * time.Second):
			}
			wg.Go(func() {
				recoveryGuard(name, logger, wg, stopCh, fn)
			})
		}
	}()
	fn()
}

// NewCloudDownloadManager 创建云端下载管理器。
//
// 迁移后云任务文件与状态按任务 owner 落租户桶：
//   - 任务文件 → tenantFor(owner).Root() 下 cloud 桶（<root>/<tenant>/cloud/<taskID>/<file>）
//   - 任务状态 → meta/cloud 桶（<root>/<tenant>/meta/cloud/<taskID>.json）
//   - 组状态 → meta/cloud/groups（<root>/<tenant>/meta/cloud/groups/<gid>.json）
//
// tenantFor/checksumStoreFor/listTenants 由 RegisterRoutes 装配传入（h.tenantFor /
// h.checksumStoreFor / h.listTenantIDs）；任一为 nil 时回退为不可用（写路径 fail-closed，
// 不 panic）。空 owner 任务落 anonymous 租户。
func NewCloudDownloadManager(uploadsDir string, sm StorageManager, tenantFor TenantResolver, checksumStoreFor ChecksumResolver, listTenants func() []string, logger *slog.Logger, cfg *CloudDownloadConfig, quotaFor ...QuotaResolver) *CloudDownloadManager {
	if tenantFor == nil {
		tenantFor = func(string) *storage.Tenant { return nil }
	}
	if checksumStoreFor == nil {
		checksumStoreFor = func(string) *checksum.ChecksumStore { return nil }
	}
	if listTenants == nil {
		listTenants = func() []string { return nil }
	}
	var qf QuotaResolver
	if len(quotaFor) > 0 {
		qf = quotaFor[0]
	}

	// 零值字段填充默认值（超时/重试等必须在这里生效，不依赖调用方接线）
	applyCloudConfigDefaults(cfg)

	mgr := &CloudDownloadManager{
		tasks:            make(map[string]*CloudTask),
		uploadsDir:       uploadsDir,
		tenantFor:        tenantFor,
		checksumStoreFor: checksumStoreFor,
		quotaFor:         qf,
		listTenants:      listTenants,
		storage:          sm,
		logger:           slogutil.Default(logger),
		semaphore:        make(chan struct{}, cfg.MaxConcurrent),
		config:           cfg,
		dl:               downloader.NewFromConfig(cfg.Downloader),
		cancelFuncs:      make(map[string]context.CancelFunc),
		running:          make(map[string]bool),
		metrics:          &CloudMetrics{},
		dirtyTasks:       make(map[string]struct{}),
		flushNow:         make(chan struct{}, 1),
		stopFlush:        make(chan struct{}),
		stopCleanup:      make(chan struct{}),
		groups:           make(map[string]*CloudTaskGroup),
	}

	mgr.logger.Info("cloud download manager initialized",
		"max_concurrent", cfg.MaxConcurrent,
		"max_batch_urls", cfg.MaxBatchURLs,
		"sync_threshold", cfg.SyncThreshold,
		"task_ttl", cfg.TaskTTL,
		"failed_task_ttl", cfg.FailedTaskTTL,
		"download_timeout", cfg.DownloadTimeout,
		"idle_timeout", cfg.IdleTimeout,
		"max_retries", cfg.MaxRetries,
		"retry_delay", cfg.RetryDelay,
	)

	// 允许私有 IP 时跳过 SSRF 后验证（仅测试用）
	// 注意：必须创建副本而非修改共享注册表的下载器，避免 data race
	if hd, ok := mgr.dl.(*downloader.HTTPDownloader); ok {
		clone := *hd
		if cfg.AllowPrivate {
			clone.ValidateURLAfterDo = nil
		}
		if cfg.DownloadTimeout > 0 {
			clone.Timeout = cfg.DownloadTimeout
		}
		if cfg.IdleTimeout > 0 {
			clone.IdleTimeout = cfg.IdleTimeout
		}
		mgr.dl = &clone
	}

	// 恢复持久化的任务与任务组。
	// 顺序不能倒：recoverGroups 会按 m.tasks 修剪组内已不存在的任务引用，故必须先恢复任务；
	// 而孤儿 pending 所属的组记录要到 recoverGroups 之后才在内存里，故组状态刷新只能放在其后
	// （否则 UpdateGroupStatus 会因组不存在而空转，Web UI 会一直看到 downloading）。
	orphanPendingGroups := mgr.recoverTasks()
	mgr.recoverGroups()
	for _, gid := range orphanPendingGroups {
		mgr.UpdateGroupStatus(gid)
	}

	// 启动过期任务清理 (wg 跟踪)
	mgr.wg.Go(func() {
		recoveryGuard("cleanupExpired", mgr.logger, &mgr.wg, mgr.stopCleanup, mgr.cleanupExpired)
	})

	// 启动批量持久化 goroutine (wg 跟踪)
	mgr.wg.Go(func() {
		recoveryGuard("flushLoop", mgr.logger, &mgr.wg, mgr.stopFlush, mgr.flushLoop)
	})

	return mgr
}

// cloudDirFor 返回 owner 租户 cloud 桶的绝对路径（<root>/<tenant>/cloud）。
// 租户不可用（非法 owner / 存储根未装配）时返回 ""。
// CloudDirFor 返回 owner 租户的云任务根目录（`<租户根>/cloud`）。租户不可用（非法 owner /
// 存储根未装配）时返回 ""。**布局是领域契约**：任务文件落 `<CloudDirFor>/<taskID>/<filename>`
// （存档路径 pkg/server/cloud_archive_handler.go 亦按此布局经 FeatureRel 取文件）。
//
// 导出理由：装配层（含其测试）需要按该布局断言磁盘状态；把它留在包内会迫使调用方**复制**
// 一份布局知识（第二事实源）。
// UploadsDir 返回本管理器装配时的存储根（任务目录均在其租户子目录之下）。
// 供装配层与其测试定位落盘产物（导出理由同 CloudDirFor）。
func (m *CloudDownloadManager) UploadsDir() string { return m.uploadsDir }

func (m *CloudDownloadManager) CloudDirFor(owner string) string {
	tnt := m.tenantFor(owner)
	if tnt == nil {
		return ""
	}
	abs, _ := tnt.Root().Abs("cloud")
	return abs
}

// persistDirFor 返回 owner 租户云任务状态目录（<root>/<tenant>/meta/cloud）。
// 租户不可用时返回 ""。
// PersistDirFor 返回 owner 租户的云任务**状态**目录（`<租户根>/meta/cloud`），
// 任务状态落 `<PersistDirFor>/<taskID>.json`、组状态落 `<PersistDirFor>/groups/<groupID>.json`。
// 导出理由同 CloudDirFor（装配层测试需按布局断言/构造磁盘状态）。
func (m *CloudDownloadManager) PersistDirFor(owner string) string {
	tnt := m.tenantFor(owner)
	if tnt == nil {
		return ""
	}
	abs, _ := tnt.Root().Abs("meta/cloud")
	return abs
}

// groupsDirFor 返回 owner 租户云任务组状态目录（<root>/<tenant>/meta/cloud/groups）。
// 租户不可用时返回 ""。
func (m *CloudDownloadManager) groupsDirFor(owner string) string {
	base := m.PersistDirFor(owner)
	if base == "" {
		return ""
	}
	return filepath.Join(base, "groups")
}

// taskDirFor 返回 owner 租户下任务文件目录（<root>/<tenant>/cloud/<taskID>）。
// 租户不可用时返回 ""。
// TaskDirFor 返回 owner 租户下某任务的落盘目录（`<CloudDirFor>/<taskID>`）。
func (m *CloudDownloadManager) TaskDirFor(owner, taskID string) string {
	base := m.CloudDirFor(owner)
	if base == "" {
		return ""
	}
	return filepath.Join(base, taskID)
}

// removeTaskDir 尽力删除 owner 租户下任务文件目录（租户不可用时为空操作）。
func (m *CloudDownloadManager) removeTaskDir(owner, taskID string) {
	dir := m.TaskDirFor(owner, taskID)
	if dir == "" {
		return
	}
	_ = os.RemoveAll(dir)
}

// removeTaskDirWithRetry 删除任务文件目录，并对瞬时占用做有界重试（见 removeWithRetry）。
// 返回最终错误（目录不存在/租户不可用视为成功）：调用方可据此判断「清理未完成」是否
// 需要对用户可观（并入 task.Error）。
func (m *CloudDownloadManager) removeTaskDirWithRetry(owner, taskID string) error {
	dir := m.TaskDirFor(owner, taskID)
	if dir == "" {
		return nil
	}
	return removeWithRetry(func() error { return os.RemoveAll(dir) })
}

// quotaScope 返回 owner 的租户配额 Scope（未装配 quotaFor 时返回 nil）。
func (m *CloudDownloadManager) quotaScope(owner string) *quota.Scope {
	if m.quotaFor == nil {
		return nil
	}
	return m.quotaFor(owner)
}

// quotaSinkAdapter 把 *quota.TaskAccount 适配为 downloader.QuotaSink（任务 7 收敛）。
// 写盘字节经下载器直接进 account（边写边记 + 自动补留）：
//   - Finish(true, oldSize)：释放未用 reserve + 覆盖写 ReleaseUsage(oldSize)；
//   - Finish(false, _)：只释放未用 reserve（保留已 commit 供续传，语义同前）。
type quotaSinkAdapter struct {
	acc   *quota.TaskAccount
	scope *quota.Scope
	w     io.Writer // 底层写盘目标（下载器传入；CommitUp 记账后写盘，与旧 QuotaWriter.Write 同构）
}

func (a *quotaSinkAdapter) Write(p []byte) (int, error) {
	if err := a.acc.CommitUp(int64(len(p))); err != nil {
		return 0, err
	}
	return a.w.Write(p)
}

// Committed 委托 account 的已确认占用（测试与对账用）。
func (a *quotaSinkAdapter) Committed() int64 { return a.acc.Committed() }

// Finish 语义（对齐"下载失败保留 .partial 供续传"）：
//   - success=true：完成——释放未用 reserve（ReleaseReserve）+ 覆盖写 ReleaseUsage(oldSize)；
//   - success=false：失败/写超——只释放未用 reserve（ReleaseReserve），**保留已 commit 字节
//     继续占账**（.partial 在磁盘上，供 ResumeTask 续传复用；cancel/delete 时由
//     releaseTaskScope 收敛的 account.Release 回拨）。
func (a *quotaSinkAdapter) Finish(success bool, oldSize int64) {
	if success {
		a.acc.ReleaseReserve()
		a.scope.ReleaseUsage(oldSize)
	} else {
		a.acc.ReleaseReserve()
	}
}

// downloadSinkFactory 返回把写盘目标包装为 QuotaWriter 的 SinkFactory（每次写盘会话调用）。
// scope 为 nil（未装配）时返回 nil factory（直写，仅全局 storageMgr 账本）。
// 复用 task.account（首次会话 NewTaskAccount 预留，续传/重试保留同一 account）：
//   - 跨重试/续传不重复预留（防双计）；RunResult 完成后 account 已结算（committed/reserved=0），
//     再次 New 仅当 task.account==nil 且 scope 可用——重启恢复的任务 account==nil 且 Restored
//     由磁盘扫描校准，不重建。
func (m *CloudDownloadManager) downloadSinkFactory(task *CloudTask) downloader.SinkFactory {
	scope := m.quotaScope(task.Owner)
	if scope == nil {
		return nil
	}
	return func(w io.Writer, contentLength int64, resume bool) (downloader.QuotaSink, error) {
		// task.account 是被下载 goroutine 独占的使用者（创建/CommitUp/ReleaseReserve 均在
		// 下载执行 goroutine 内调用本闭包），但 SnapshotTask/ListTasks 的浅拷贝会以指针形式
		// 把 task.account 暴露给锁外读者 → 拷贝端置 nil 后，此处读写与下载 goroutine 串行即可
		// 无 race（同一 goroutine，无并发）。防御：仍持 m.mu 保护，防止未来下载路径
		// 分裂成多 goroutine（如 retry 重入）时引入竞态。
		m.mu.Lock()
		defer m.mu.Unlock()
		if task.account == nil {
			var estimate int64
			if contentLength > 0 {
				estimate = contentLength
			}
			// estimate<=0 → NewTaskAccount 内部占位 1 GiB。
			acc, err := quota.NewTaskAccount(scope, estimate)
			if err != nil {
				return nil, err
			}
			task.account = acc
			return &quotaSinkAdapter{acc: acc, scope: scope, w: w}, nil
		}
		// 续传/重试：复用同一 account（保留已 commit 用于增量补预留）。
		return &quotaSinkAdapter{acc: task.account, scope: scope, w: w}, nil
	}
}

// releaseTaskScope 释放任务在租户 Scope 中的全部占用（取消/删除/放弃路径）：
//   - account.Release()：幂等回拨 committed + reserved（TaskAccount 统一所有权，
//     见 pkg/quota/task_account.go）；归零后置 nil（防二次释放）。
//   - 下载中 account 边写边记的 committed 一并回拨（防取消时 Scope 虚高）；
//   - 未用 reserve 由 account.Release 内部 releaseUp 归还（不再单独 ReleaseReserve——
//     避免与 Release 叠加双释放）。
//
// 幂等：复调/任务无占用/Scope 未装配均为空操作（account nil 直接跳过）。
//
// 前置条件（审计 F5，2026-09-16）：**桶的 committed 可能低于本任务账本**——周期 reconcile 的
// 「读 Usage() → Adjust」两拍非原子（pkg/server/quota_reconcile.go 自陈），若读取后被并发
// commitUp 夹在中间，桶会被压到低于任务账本；而 QW 的 `reserved` 只在两个窗口为 0：
// ①「estimate 恰好用尽、下一次 Write 尚未补留」（pkg/quota/quota_writer.go 的 Write 先补留后入账）；
// ② `Finish` 之后（预留已全部结算，`reserved` 恒 0）——② 无害（此时磁盘==账本，reconcile 的
// Adjust 净差为 0）。⇒ reconcile 的 `Reserved()>0` 跳过保护**并非绝对覆盖**。
// 此时本函数按任务账本释放即属**超额释放**：#302 之后 releaseCommittedUp / adjustUp 只传播
// **本层实际扣减量**，即只保证「**不多扣**」——若本层同时还持有**其它任务**的字节，清零会连它们
// 一起清，并向父层传播**等量**扣减（这是维持「祖先 ≥ 其子树之和」所必需的）⇒ 祖先**会**按本层
// 实际扣减量同步下降，但**绝不被超额扣减**；残留影响是「本层 + 祖先少计，直到下次扫描自愈」
// （fail-closed，可接受）。
// 若要根治需在 pkg/server 侧把两拍改为单锁原子写（如给 Pool 加 SetCommittedTo(n)）——
// 超出本包边界，已作为后续项登记。
func (m *CloudDownloadManager) releaseTaskScope(task *CloudTask) {
	if task.account != nil {
		task.account.Release() // 幂等：回拨 committed + reserved，归零后空操作
		task.account = nil
	}
	task.ReservedSize = 0 // 释放后归零防二次释放（storageMgr 侧由调用方另行处理）
}

// releaseAbandonedTaskScope 在下载 goroutine 退出时释放「已放弃」任务的租户配额占用。
//
// 存在的理由（不变量：**goroutine 已停止 ⇒ 配额已归零**）：取消/删除时下载 goroutine
// 可能仍在写盘并 commit 字节，此时提前释放会被后续 commit 抬回（账本泄漏，CI run
// 34941359725 的 `cancel 后 cloud 桶 Usage()=200 want 0`）。故运行中的任务一律把释放
// 推迟到本 goroutine 退出（此时不可能再有 commit），由本函数统一回拨；releaseTaskScope
// 幂等，重复调用释放 0。
//
// 只释放「已放弃」的任务：取消（cancelled）或已被删除（不在 m.tasks）。failed 保留
// .partial 供续传、completed 的文件即真实占用，二者必须保留已记占用，不得回拨。
// 调用方需持 m.mu（状态与存在性判定在同一临界区内，并与清理 running 标记同锁，使
// waitTaskStopped 返回 true 时释放已完成）。
//
// 已知取舍（勿按「两账必须同步」写断言）：CancelTask/DeleteTask 里**同步**释放的是全局
// storageMgr 占用（API /api/stats 的 CategoryCloud 立即归零），而租户 Scope 的释放按上述
// 理由推迟到 goroutine 退出，两者之间存在短暂不一致窗口；若 goroutine 长时间不退出，
// 租户配额会被推迟归还。该不一致是为修正确性（防账本被后续 commit 抬回）而接受的取舍。
func (m *CloudDownloadManager) releaseAbandonedTaskScope(task *CloudTask) {
	if stored, ok := m.tasks[task.ID]; ok && stored.Status != "cancelled" {
		return
	}
	m.releaseTaskScope(task)
}

// CreateTask 创建云端下载任务（不启动下载）。
// owner 是请求认证派生的归属（SproxySig→AK / api_keys→key 名 / 未认证→空串），
// 由 handler 传入，服务端不信任客户端输入。
// Metrics 返回本管理器的运行指标（只读快照入口）。
//
// 导出理由：装配层的 Prometheus 暴露端（pkg/server/metrics.go）需要读取这些计数器，
// 而它不在本包内——原先靠「同包可访问未导出字段 m.metrics」实现，抽取后必须给出
// 显式访问器。返回的指针指向包内实例，调用方只应读取其原子计数器。
func (m *CloudDownloadManager) Metrics() *CloudMetrics { return m.metrics }

// AllowPrivate 报告 SSRF 策略是否允许下载私网地址（装配层做请求校验时需要）。
// 返回的是**构造期快照**（applyCloudConfigDefaults 之后的 cfg），与既有语义一致。
func (m *CloudDownloadManager) AllowPrivate() bool { return m.config.AllowPrivate }

// MaxBatchURLs 返回单次批量请求允许的最大 URL 数（装配层做请求校验时需要）。
func (m *CloudDownloadManager) MaxBatchURLs() int { return m.config.MaxBatchURLs }
