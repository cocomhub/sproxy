// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/server/downloader"
)

// downloadsDirName 是云端下载持久化目录名。
const downloadsDirName = ".__downloads__"

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
	ReservedSize int64     `json:"-"`                  // 实际预留量，不持久化
	GroupID      string    `json:"group_id,omitempty"` // 所属组 ID（可选）
}

// CloudTaskGroup 表示一个云端下载任务组。
// 每个子任务仍是独立的 CloudTask（文件保存在 .__cloud__/<taskID>/ 下），
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

// CloudDownloadManager 管理云端下载任务。
type CloudDownloadManager struct {
	tasks         map[string]*CloudTask
	mu            sync.RWMutex
	uploadsDir    string
	cloudDir      string // uploadsDir/.__cloud__/
	persistDir    string // uploadsDir/.__downloads__/
	storage       *StorageManager
	checksumStore ChecksumStoreIface
	logger        *slog.Logger
	semaphore     chan struct{}
	config        *CloudDownloadConfig
	dl            downloader.Downloader
	cancelFuncs   map[string]context.CancelFunc // 任务取消函数
	running       map[string]bool               // 任务是否有执行中的下载 goroutine（含排队）
	metrics       *CloudMetrics

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
func NewCloudDownloadManager(uploadsDir string, sm *StorageManager, cs ChecksumStoreIface, logger *slog.Logger, cfg *CloudDownloadConfig) *CloudDownloadManager {
	cloudDir := filepath.Join(uploadsDir, cloudDirName)
	persistDir := filepath.Join(uploadsDir, downloadsDirName)

	if err := os.MkdirAll(cloudDir, 0755); err != nil {
		defaultLogger(logger).Warn("创建 cloud 目录失败", "dir", cloudDir, "error", err)
	}
	if err := os.MkdirAll(persistDir, 0755); err != nil {
		defaultLogger(logger).Warn("创建 persist 目录失败", "dir", persistDir, "error", err)
	}

	// 零值字段填充默认值（超时/重试等必须在这里生效，不依赖调用方接线）
	applyCloudConfigDefaults(cfg)

	mgr := &CloudDownloadManager{
		tasks:         make(map[string]*CloudTask),
		uploadsDir:    uploadsDir,
		cloudDir:      cloudDir,
		persistDir:    persistDir,
		storage:       sm,
		checksumStore: cs,
		logger:        defaultLogger(logger),
		semaphore:     make(chan struct{}, cfg.MaxConcurrent),
		config:        cfg,
		dl:            downloader.NewFromConfig(cfg.Downloader),
		cancelFuncs:   make(map[string]context.CancelFunc),
		running:       make(map[string]bool),
		metrics:       &CloudMetrics{},
		dirtyTasks:    make(map[string]struct{}),
		flushNow:      make(chan struct{}, 1),
		stopFlush:     make(chan struct{}),
		stopCleanup:   make(chan struct{}),
		groups:        make(map[string]*CloudTaskGroup),
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

	// 恢复持久化的任务与任务组
	mgr.recoverTasks()
	mgr.recoverGroups()

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

// CreateTask 创建云端下载任务（不启动下载）。
// owner 是请求认证派生的归属（SproxySig→AK / api_keys→key 名 / 未认证→空串），
// 由 handler 传入，服务端不信任客户端输入。
// 自动去重：相同 URL 且**对请求者可见**（同 owner 或全局空 owner）的活跃任务返回已有任务。
func (m *CloudDownloadManager) CreateTask(method, url, filename string, totalSize int64, owner string) (*CloudTask, error) {
	// URL 去重：仅对请求者可见的任务去重（跨 owner 的同 URL 任务不吸收，各自独立下载）
	if existing := m.findByURL(url, owner); existing != nil {
		m.logger.Info("duplicate cloud download request, reusing existing task",
			"url", url,
			"existing_id", existing.ID,
			"existing_status", existing.Status,
		)
		return existing, nil
	}

	// 预留存储空间
	reserved := totalSize
	if reserved <= 0 {
		reserved = cloudReservePlaceholder // 1 GiB 保底
	}
	if err := m.storage.TryReserve(reserved, CategoryCloud); err != nil {
		m.logger.Warn("storage full, cloud download rejected",
			"url", url,
			"requested_size", totalSize,
			"current_usage", m.storage.Usage(),
			"max_bytes", m.storage.MaxBytes(),
		)
		return nil, err
	}

	task := &CloudTask{
		ID:           newTaskID(),
		Owner:        owner,
		URL:          url,
		Method:       method,
		Filename:     filename,
		Status:       "pending",
		TotalSize:    totalSize,
		ReservedSize: reserved,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(m.config.TaskTTL),
	}

	m.mu.Lock()
	m.tasks[task.ID] = task
	m.mu.Unlock()

	_ = m.saveTask(task)
	m.logger.Info("cloud download task created",
		"task_id", task.ID,
		"url", url,
		"filename", filename,
		"reserved_size", totalSize,
	)
	m.metrics.TasksCreated.Add(1)
	return task, nil
}

// SubmitAndStart 创建任务并立即启动下载。
// 仅当调用方已知 totalSize > 0 且 < syncThreshold 且 syncCtx 非 nil 时才同步执行
// （在调用方 goroutine 内完成，便于小文件请求同步返回）；否则始终异步。
// 注意：服务端 handler 提交时大小未知（传 -1），因此实际请求恒异步；
// 同步路径主要供调用方在已知小文件大小时使用。
func (m *CloudDownloadManager) SubmitAndStart(method, url, filename string, totalSize int64, syncCtx context.Context, owner string) (*CloudTask, error) {
	task, err := m.CreateTask(method, url, filename, totalSize, owner)
	if err != nil {
		return nil, err
	}

	// 启动必须使用 m.tasks 中的真实对象，而非 CreateTask 返回的对象：
	// CreateTask 去重命中时返回 findByURL 的快照副本，对副本启动 goroutine 会
	// 导致 executeDownload 全程只写副本、真实对象永远停在 pending（findByURL
	// 持续命中使同 URL 无法再下载、任务卡死，直到进程重启自愈）。
	// 在写锁内检查 Status 并同步置位 running，闭合"检查→启动"竞态窗口，避免
	// 对同一 URL 并发启动两个 goroutine 写同一 .partial（Critical 修复）。
	m.mu.Lock()
	realTask, ok := m.tasks[task.ID]
	if !ok || realTask.Status != "pending" || m.running[realTask.ID] {
		m.mu.Unlock()
		return task, nil
	}
	m.running[realTask.ID] = true
	m.mu.Unlock()

	useSync := syncCtx != nil && totalSize > 0 && totalSize < m.config.SyncThreshold

	if useSync {
		m.logger.Info("starting sync cloud download", "task_id", realTask.ID, "url", url, "size", totalSize)
		// 同步下载：直接在当前 goroutine 执行，wg.Add(1) 在 go 之前确保不竞态
		m.wg.Add(1)
		m.executeDownload(syncCtx, realTask)
		return task, nil
	}

	m.logger.Info("starting async cloud download", "task_id", realTask.ID, "url", url, "size", totalSize)
	// 异步下载：goroutine 执行，wg.Add(1) 在 go 之前确保不竞态
	//nolint:gosec
	m.wg.Add(1)
	go m.executeDownload(context.Background(), realTask) //nolint:gosec
	return task, nil
}

// executeDownload 执行实际下载逻辑。
// 注意：调用者必须保证在调用前已调 m.wg.Add(1)，函数退出时自动 m.wg.Done()。
func (m *CloudDownloadManager) executeDownload(ctx context.Context, task *CloudTask) {
	defer m.wg.Done()
	// handedOff 标记"同步下载断连后已把下载转交给新 goroutine"：
	// 转交时 running 标记由新 goroutine 接管，旧 goroutine 的 defer 不得清除，
	// 否则新 goroutine 会短暂丢失 running，导致 ResumeTask 并发启动。
	var handedOff bool
	// 创建可取消的 context（从 Background 派生，使客户端断连后下载可继续异步重试）。
	// 必须在等待信号量之前注册 cancelFuncs：排队中的任务也能被取消/删除。
	dlCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 标记运行中：ResumeTask/Cancel 竞争保护依赖该标记区分"goroutine 仍存活"
	// 与"已退出"。必须在任何可能的写盘操作前设置。
	m.mu.Lock()
	m.cancelFuncs[task.ID] = cancel
	m.running[task.ID] = true
	m.mu.Unlock()

	// cleanupRunning 清理 running/cancelFuncs 标记。
	// 拆为独立函数，让 panic recovery 可先调用 failTask 再清理。
	cleanupRunning := func() {
		if handedOff {
			return
		}
		m.mu.Lock()
		delete(m.cancelFuncs, task.ID)
		delete(m.running, task.ID)
		m.mu.Unlock()
	}
	defer cleanupRunning()

	// 任务进入终态后刷新所属组状态，保证持久化的组状态不滞后于子任务实际进展
	defer m.refreshTaskGroup(task)

	// panic recovery 放在最后（最外层 defer，最先执行），
	// 确保 panic 时先调用 failTask 再清理 running（避免 ResumeTask 在 failTask
	// 完成前误判 goroutine 已退出并发起重写 .partial）。
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("panic in download", "task_id", task.ID, "panic", r)
			m.failTask(task, fmt.Sprintf("panic: %v", r))
			// failTask 之后刷新组状态（panic 时 refreshTaskGroup 已先执行，但那时状态是
			// downloading 非终态，这里再调一次确保用 failed 状态更新组）。
			m.refreshTaskGroup(task)
		}
	}()

	// 竞态守卫：goroutine 启动前/排队期间任务可能已被取消（CancelTask 已置
	// cancelled 并释放存储）。此时直接退出，不启动下载、不覆盖终态。
	m.mu.RLock()
	cancelled := task.Status == "cancelled"
	m.mu.RUnlock()
	if cancelled {
		m.logger.Info("download skipped, task was cancelled before start", "task_id", task.ID)
		return
	}

	// 排队等待信号量（排队期间可取消）
	select {
	case m.semaphore <- struct{}{}:
		defer func() { <-m.semaphore }()
	case <-dlCtx.Done():
		// 排队期间被取消：CancelTask 已（或即将）置 cancelled 并释放存储。
		// 这里不 failTask——把"已取消"标成 failed 会与 CancelTask 的终态打架，
		// 让用户看到错误的失败状态；直接退出，终态由 CancelTask 写入。
		m.logger.Info("queued download cancelled", "task_id", task.ID)
		return
	}

	// 取得信号量后复查一次：CancelTask 可能恰在 acquire 与这里之间执行
	m.mu.RLock()
	cancelled = task.Status == "cancelled"
	m.mu.RUnlock()
	if cancelled {
		m.logger.Info("download skipped, task cancelled while acquiring slot", "task_id", task.ID)
		return
	}

	m.metrics.ActiveDownloads.Add(1)
	defer m.metrics.ActiveDownloads.Add(-1)

	m.mu.Lock()
	// 复查任务状态与存在性：CancelTask/DeleteTask 可能在信号量获取与此处之间
	// 执行，若状态已非 pending 或任务已从 map 中删除，则放弃下载。
	if stored, ok := m.tasks[task.ID]; !ok || (stored.Status != "pending" && stored.Status != "downloading") {
		m.mu.Unlock()
		if ok {
			m.logger.Info("download skipped, task status changed while acquiring slot",
				"task_id", task.ID, "status", stored.Status)
		} else {
			m.logger.Info("download skipped, task deleted while acquiring slot", "task_id", task.ID)
		}
		return
	}
	task.Status = "downloading"
	task.UpdatedAt = time.Now()
	m.mu.Unlock()
	_ = m.saveTask(task)

	m.logger.Info("download started", "task_id", task.ID, "url", task.URL, "filename", task.Filename)

	// 构建目标文件路径
	taskDir := filepath.Join(m.cloudDir, task.ID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		m.logger.Warn("创建任务目录失败", "task_id", task.ID, "dir", taskDir, "error", err)
		m.failTask(task, fmt.Sprintf("create task dir: %v", err))
		return
	}
	destPath := filepath.Join(taskDir, task.Filename)

	// 执行下载（带重试）
	maxRetries := m.config.MaxRetries
	var result *downloader.Result
	var downloadErr error
	for attempt := range maxRetries {
		// 外层 ctx（同步下载的请求 ctx）已取消而内层未取消 = 客户端断连：
		// 立即交还，由 downloadDone 处转入异步继续，避免阻塞 handler 至重试耗尽。
		if ctx.Err() != nil && dlCtx.Err() == nil {
			downloadErr = ctx.Err()
			goto downloadDone
		}
		if attempt > 0 {
			// 重试等待（等待期间用户取消/客户端断连则立即停止）
			m.metrics.TasksRetried.Add(1)
			select {
			case <-time.After(m.config.RetryDelay):
			case <-dlCtx.Done():
				downloadErr = dlCtx.Err()
				goto downloadDone
			case <-ctx.Done():
				downloadErr = ctx.Err()
				goto downloadDone
			}
			m.logger.Info("retrying download", "task_id", task.ID, "url", task.URL, "attempt", attempt+1, "max", maxRetries)
		}

		// 每次尝试独立超时：超时可重试续传，用户取消不可重试
		attemptCtx := dlCtx
		var attemptCancel context.CancelFunc
		if m.config.DownloadTimeout > 0 {
			attemptCtx, attemptCancel = context.WithTimeout(dlCtx, m.config.DownloadTimeout)
		}

		result, downloadErr = m.dl.Download(attemptCtx, task.URL, destPath, func(downloaded, total int64) {
			m.mu.Lock()
			task.Downloaded = downloaded
			if total > 0 {
				task.TotalSize = total
			}
			id := task.ID
			m.mu.Unlock()
			// 在 m.mu 外调用 markDirty，避免与 flushDirty 的 dirtyMu → m.mu 形成 ABBA 死锁。
			// flushDirty 顺序：dirtyMu.Lock → saveTask(内部 m.mu.RLock)；
			// progress 回调顺序：m.mu.Lock → markDirty(dirtyMu.Lock)。
			// 将 markDirty 移出 m.mu 范围后锁序不再反转。
			m.markDirty(id)
		})

		// 及时释放本次尝试的定时器，避免累积到函数退出
		if attemptCancel != nil {
			attemptCancel()
		}

		if downloadErr == nil {
			// 立即记录 ETag 到 task：续传重试时可通过 task.ETag 做二次校验，
			// 完成后客户端也可通过 API 读取 ETag 确认版本。
			if result.ETag != "" {
				m.mu.Lock()
				task.ETag = result.ETag
				m.mu.Unlock()
			}
			break
		}

		// 用户取消/任务删除：停止重试
		if dlCtx.Err() != nil {
			downloadErr = dlCtx.Err()
			break
		}
		// 仅重试可重试错误（网络/5xx）或本次尝试超时
		var retryable *downloader.RetryableError
		if !errors.As(downloadErr, &retryable) && attemptCtx.Err() != context.DeadlineExceeded {
			break
		}
		// 最后一次尝试失败，不再重试
		if attempt >= maxRetries-1 {
			break
		}
	}

downloadDone:
	// 任务删除竞态守卫：删除后完成/失败的下载不再触碰存储与状态。
	// 注意：真正的终态提交在下方锁内统一复查，这里仅避免无谓的 failTask/对账。
	m.mu.RLock()
	_, exists := m.tasks[task.ID]
	m.mu.RUnlock()
	if !exists {
		m.logger.Info("download finished after task deletion, skipping completion", "task_id", task.ID)
		_ = os.RemoveAll(filepath.Join(m.cloudDir, task.ID))
		return
	}

	if downloadErr != nil {
		// 客户端断开（只有外层 ctx 取消，内层 dlCtx 未取消），转为异步继续。
		// running/cancelFuncs 由新 goroutine 接管：同步置位并标记 handedOff，使旧
		// goroutine 的 defer 不清除二者，避免新 goroutine 短暂丢失运行标记与取消句柄。
		if ctx.Err() != nil && dlCtx.Err() == nil {
			m.logger.Info("sync download client disconnected, switching to async",
				"task_id", task.ID, "url", task.URL)
			//nolint:gosec // G118: 断线后异步继续需要独立 context
			m.mu.Lock()
			m.running[task.ID] = true
			handedOff = true
			m.mu.Unlock()
			m.wg.Add(1)
			go m.executeDownload(context.Background(), task) //nolint:gosec
			return
		}
		// 用户取消：CancelTask 已更新状态并释放存储，这里不重复处理，仅兜底清理
		// 可能残留的任务文件（CancelTask 的 RemoveAll 在 Windows 下可能因文件被占用失败）。
		if dlCtx.Err() == context.Canceled {
			m.logger.Info("download cancelled", "task_id", task.ID)
			_ = os.RemoveAll(filepath.Join(m.cloudDir, task.ID))
			return
		}
		m.failTask(task, downloadErr.Error())
		m.logger.Error("download failed", "task_id", task.ID, "url", task.URL, "error", downloadErr)
		return
	}

	// 终态提交：在 m.mu 写锁内原子完成"复查存在/未取消 → 账本对账 → 置 completed"，
	// 与 CancelTask/DeleteTask 的写锁互斥，消除完成路径 TOCTOU：
	//   - 任务恰在此前被取消/删除时不会覆盖终态（取消后任务仍会残留文件的场景被移除）；
	//   - 不再出现对已删除任务写状态或对 m.tasks[id]（可能为 nil）解引用 panic；
	//   - 账本对账与置位同临界区，避免"CancelTask 释放预留后完成路径又补预留"的二次记账。
	m.mu.Lock()
	stored, ok := m.tasks[task.ID]
	if !ok {
		m.mu.Unlock()
		m.logger.Info("download finished after task deletion, skipping completion", "task_id", task.ID)
		_ = os.RemoveAll(filepath.Join(m.cloudDir, task.ID))
		return
	}
	if stored.Status == "cancelled" {
		m.mu.Unlock()
		m.logger.Info("download finished after cancel, discarding result", "task_id", task.ID)
		// 取消即放弃：连同 .partial/.partial.etag 一并清理（CancelTask 已释放存储
		// 并尝试删除，这里兜底），保持磁盘与已归零账本一致。
		_ = os.RemoveAll(filepath.Join(m.cloudDir, task.ID))
		return
	}

	// 恢复原始文件 mtime（先记录，锁外执行 Chtimes）
	var fileMTime int64
	if result.ModTime != (time.Time{}) {
		fileMTime = result.ModTime.UnixNano()
		stored.FileMTime = fileMTime
	}

	// 存储账本补偿：以 ReservedSize 为基准对齐到实际大小。TryReserve/Release 均为
	// 内存计数，锁内调用与 failTask/CancelTask 的锁内存储操作保持一致锁序。
	reserved := stored.ReservedSize
	sizeDelta := result.Size - reserved
	if sizeDelta > 0 {
		// 实际更大，需要追加预留
		if err := m.storage.TryReserve(sizeDelta, CategoryCloud); err != nil {
			// 存储已满无法容纳实际大小：释放旧占位并删文件，避免账本虚高/磁盘残留。
			if reserved > 0 {
				m.storage.Release(reserved, CategoryCloud)
			}
			stored.ReservedSize = 0
			stored.Status = "failed"
			stored.Error = "storage full after download"
			stored.UpdatedAt = time.Now()
			stored.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)
			m.mu.Unlock()
			_ = os.Remove(destPath)
			m.logger.Error("storage full after download, cannot fit actual size",
				"task_id", task.ID, "actual_size", result.Size, "reserved", reserved)
			_ = m.saveTask(stored)
			m.metrics.TasksFailed.Add(1)
			return
		}
		stored.ReservedSize = result.Size
	} else if sizeDelta < 0 {
		// 实际更小，释放多余空间
		m.storage.Release(-sizeDelta, CategoryCloud)
		stored.ReservedSize = result.Size
	}

	// 写入 ChecksumStore。key 与 download/stat/chunk 读取端一致：
	// 读端 resolveDownloadPath(kind=cloud_task) 返回的 filename 是 <taskID>/<file>（不含
	// .__cloud__ 内部段），故写端同样用 <owner>/<taskID>/<file>（审查 M1：加 owner 前缀时
	// 误保留 .__cloud__ 段导致写读 key 不一致、缓存永不命中、每请求全量重算）。
	// 审查 F2：写端 key 必须与读端（resolveDownloadPath 返回的 <taskID>/<file> 正斜杠
	// query 值）一致——filepath.Join 在 Windows 下产出反斜杠（ID\file），读端 key 恒正
	// 斜杠 → 缓存永不命中、每请求全量重算。用 ToSlash 归一为协议正斜杠。
	remotePath := filepath.ToSlash(filepath.Join(stored.ID, stored.Filename))
	if m.checksumStore != nil {
		m.checksumStore.Set(checksumStoreKey(stored.Owner, remotePath), result.Checksum)
	}

	// 更新任务状态
	stored.Status = "completed"
	stored.TotalSize = result.Size
	stored.Downloaded = result.Size
	stored.Checksum = result.Checksum
	stored.ETag = result.ETag
	stored.UpdatedAt = time.Now()
	stored.ExpiresAt = time.Now().Add(m.config.TaskTTL)
	m.mu.Unlock()

	// 锁外 I/O：恢复文件 mtime 与终态持久化
	if fileMTime != 0 {
		modTime := result.ModTime
		if err := os.Chtimes(destPath, modTime, modTime); err != nil {
			m.logger.Warn("设置文件修改时间失败", "task_id", task.ID, "error", err)
		}
	}
	// 终态持久化失败会丢失"已完成"状态（重启后任务回到 downloading 被重启下载），必须显式报错
	if err := m.saveTask(stored); err != nil {
		m.logger.Error("persist completed task failed, state may be lost on restart",
			"task_id", task.ID, "error", err)
	}
	m.logger.Info("download completed",
		"task_id", task.ID,
		"url", task.URL,
		"size", result.Size,
		"checksum", result.Checksum[:16]+"...",
	)
	m.metrics.TasksCompleted.Add(1)
	m.metrics.BytesDownloaded.Add(result.Size)
}

// failTask 将任务标记为失败，释放存储并保留 .partial 文件供续传。
// 已处于 failed/completed/cancelled 的任务直接返回（防止二次释放与状态回滚）。
func (m *CloudDownloadManager) failTask(task *CloudTask, errMsg string) {
	m.mu.Lock()
	if task.Status == "failed" || task.Status == "completed" || task.Status == "cancelled" {
		m.mu.Unlock()
		return
	}
	// 释放存储：以磁盘实际占用为基准，只释放占位与实际大小的差额。
	// .partial 保留供续传，账本与实际磁盘保持一致，避免配额窗口被累积突破。
	oldReserved := task.ReservedSize
	actual := m.diskUsageOfTask(task.ID)
	if actual < oldReserved {
		m.storage.Release(oldReserved-actual, CategoryCloud)
	} else if actual > oldReserved {
		// partial 超过占位（如大文件中途失败）：尝试补齐预留；失败则删文件防欠计
		if err := m.storage.TryReserve(actual-oldReserved, CategoryCloud); err != nil {
			m.logger.Warn("storage full, cannot keep partial for resume, removing task files",
				"task_id", task.ID, "actual", actual, "reserved", oldReserved, "error", err)
			// 文件将被整体删除：先释放旧占位，避免磁盘清空后账本仍虚高
			// （TryReserve 已失败、未增加任何预留）。
			if oldReserved > 0 {
				m.storage.Release(oldReserved, CategoryCloud)
			}
			task.ReservedSize = 0
			task.Status = "failed"
			task.Error = errMsg
			task.UpdatedAt = time.Now()
			task.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)
			m.mu.Unlock()
			_ = os.RemoveAll(filepath.Join(m.cloudDir, task.ID))
			if saveErr := m.saveTask(task); saveErr != nil {
				m.logger.Error("persist failed task after storage-full cleanup", "task_id", task.ID, "error", saveErr)
			}
			m.metrics.TasksFailed.Add(1)
			return
		}
	}
	task.ReservedSize = actual
	task.Status = "failed"
	task.Error = errMsg
	task.UpdatedAt = time.Now()
	task.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)
	m.mu.Unlock()
	// 终态持久化失败会丢失 failed 状态（重启后可能被当作 downloading 重启），必须显式报错
	if err := m.saveTask(task); err != nil {
		m.logger.Error("persist failed task state, state may be lost on restart",
			"task_id", task.ID, "error", err)
	}
	m.metrics.TasksFailed.Add(1)

	// 保留 .partial 供 ResumeTask 续传，仅清理临时文件与空目录
	taskDir := filepath.Join(m.cloudDir, task.ID)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return
	}
	hasPartial := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".partial") {
			hasPartial = true
			continue
		}
		if strings.Contains(e.Name(), ".tmp.") {
			_ = os.Remove(filepath.Join(taskDir, e.Name()))
		}
	}
	if !hasPartial {
		if err := os.RemoveAll(taskDir); err != nil {
			m.logger.Warn("failed to clean up task dir on fail", "task_id", task.ID, "error", err)
		}
	}
}

// refreshTaskGroup 任务进入终态（completed/failed/cancelled）后刷新所属组状态。
// 无组或状态非终态时为空操作。仅读取内存字段，不持有调用方锁。
func (m *CloudDownloadManager) refreshTaskGroup(task *CloudTask) {
	m.mu.RLock()
	status := task.Status
	groupID := task.GroupID
	m.mu.RUnlock()
	if groupID == "" {
		return
	}
	switch status {
	case "completed", "failed", "cancelled":
		m.UpdateGroupStatus(groupID)
	}
}

// findByURL 查找相同 URL 且对请求者 owner 可见的活跃任务（去重）。
// 仅匹配 pending/downloading 状态（排除 completed/failed/cancelled）。
// owner 非空时只匹配同 owner 或空 owner（全局）任务——跨 owner 的同 URL 任务不吸收，
// 避免把 A 的任务泄露给 B（IDOR）或让 B 的请求复用 A 的下载。
// TODO: 如果 URL 数量增长到数百级别，考虑建立 url→ID 索引避免 O(n) 遍历。
func (m *CloudDownloadManager) findByURL(url, owner string) *CloudTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tasks {
		if t.URL == url && (t.Status == "pending" || t.Status == "downloading") && ownerVisible(t.Owner, owner) {
			c := *t
			return &c
		}
	}
	return nil
}

// GetTask 返回任务的快照（副本），按请求者 owner 过滤（跨 owner 视为不存在）。
func (m *CloudDownloadManager) GetTask(id, owner string) (*CloudTask, bool) {
	return m.SnapshotTask(id, owner)
}

// SnapshotTask 返回任务的快照（副本），避免并发修改导致 data race。
// 按请求者 owner 过滤：跨 owner 任务返回 (nil, false)（404 防枚举，不泄露存在性）。
func (m *CloudDownloadManager) SnapshotTask(id, owner string) (*CloudTask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok || !ownerVisible(t.Owner, owner) {
		return nil, false
	}
	c := *t
	return &c, true
}

// ListTasks 列出任务，支持按 status 过滤与 offset/limit 分页。
// offset<0 时不偏移；limit<=0 时返回全部（兼容现有语义）。
// 排序：CreatedAt 降序 + ID 降序 tie-break。ID 含随机 4 字节 hex，CreatedAt 相等时按
// 随机值排序，但排序本身确定（同输入同输出），分页跨页仍稳定——仅同纳秒创建的任务
// 顺序不代表创建序，属可接受（创建时间戳通常唯一）。
// total 为按 status 过滤后的任务总数（不受分页影响）。
// owner 非空时只返回匹配 owner 与空 owner（全局兼容）的任务；空 owner（管理员/未认证）返回全部。
func (m *CloudDownloadManager) ListTasks(status string, offset, limit int, owner string) ([]*CloudTask, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var all []*CloudTask
	for _, t := range m.tasks {
		if (status == "" || t.Status == status) && ownerVisible(t.Owner, owner) {
			c := *t
			all = append(all, &c)
		}
	}
	// CreatedAt 降序，ID 降序 tie-break（保持稳定排序）
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].ID > all[j].ID
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	total := len(all)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		return all, total
	}
	if offset >= total {
		return nil, total
	}
	// 防止 offset+limit 溢出（limit 极大如 MaxInt64 时偏移相加可能回绕为负，导致
	// all[offset:end] slice bounds panic）。先钳制 limit 到剩余条数再相加。
	end := offset + min(limit, total-offset)
	return all[offset:end], total
}

// CancelTask 取消正在进行的任务。
// 按请求者 owner 过滤：跨 owner 任务返回 not found（404 防枚举，不泄露存在性）。
func (m *CloudDownloadManager) CancelTask(id, owner string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || !ownerVisible(t.Owner, owner) {
		m.mu.Unlock()
		return fmt.Errorf("task not found: %s", id)
	}
	if t.Status != "pending" && t.Status != "downloading" {
		m.mu.Unlock()
		return fmt.Errorf("cannot cancel task in status %q", t.Status)
	}
	t.Status = "cancelled"
	t.UpdatedAt = time.Now()
	t.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)

	// 释放实际预留的存储空间（ReservedSize 为准，释放后归零防二次释放）
	if t.ReservedSize > 0 {
		m.storage.Release(t.ReservedSize, CategoryCloud)
		t.ReservedSize = 0
	}

	// 触发下载取消（排队中任务也已在 cancelFuncs 注册，可立即生效）
	if cancel, ok := m.cancelFuncs[id]; ok {
		cancel()
		delete(m.cancelFuncs, id)
	}
	m.mu.Unlock()

	// 取消即放弃：清理任务文件（含 .partial/.partial.etag），使磁盘占用与已归零
	// 的存储账本一致——否则 partial 残留但账本释放，可累计突破 max_storage_bytes
	// 配额。goroutine 可能仍在响应 cancel 收尾（Windows 下删除被占用文件会失败），
	// 删除失败时由 executeDownload 的取消路径（RemoveAll）兜底。
	if err := os.RemoveAll(filepath.Join(m.cloudDir, id)); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to clean up task dir on cancel", "task_id", id, "error", err)
	}

	// 终态持久化失败会丢失 cancelled 状态（重启后可能被当作 downloading 重启），必须显式报错
	if err := m.saveTask(t); err != nil {
		m.logger.Error("persist cancelled task state, state may be lost on restart",
			"task_id", id, "error", err)
	}
	m.metrics.TasksCancelled.Add(1)
	m.logger.Info("cloud download task cancelled", "task_id", id)
	return nil
}

// DeleteTask 删除任务及其云端文件。
// 按请求者 owner 过滤：跨 owner 任务返回 not found（404 防枚举，不泄露存在性）。
func (m *CloudDownloadManager) DeleteTask(id, owner string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || !ownerVisible(t.Owner, owner) {
		m.mu.Unlock()
		return fmt.Errorf("task not found: %s", id)
	}

	// 如果正在下载，先取消
	if cancel, ok := m.cancelFuncs[id]; ok {
		cancel()
		delete(m.cancelFuncs, id)
	}

	delete(m.tasks, id)

	// 释放实际预留的存储空间（ReservedSize 为准，释放后归零防二次释放）。
	// 必须在锁内释放：failTask 在持有 m.mu 期间读取 ReservedSize 并执行 I/O，
	// 若 DeleteTask 在锁外释放，failTask 可能读到已释放的旧值并再次释放（double release）。
	reserved := t.ReservedSize
	if reserved > 0 {
		t.ReservedSize = 0
	}
	m.mu.Unlock()

	if reserved > 0 {
		m.storage.Release(reserved, CategoryCloud)
		m.logger.Debug("storage released", "task_id", id, "size", reserved)
	}

	m.logger.Info("deleting cloud download task", "task_id", id, "filename", t.Filename, "status", t.Status)

	// 删除云端文件
	taskDir := filepath.Join(m.cloudDir, t.ID)
	filePath := filepath.Join(taskDir, t.Filename)
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove cloud file", "task_id", id, "path", filePath, "error", err)
	}
	if err := os.Remove(taskDir); err != nil && !os.IsNotExist(err) {
		// 目录可能非空（有其他文件），使用 RemoveAll
		if err := os.RemoveAll(taskDir); err != nil {
			m.logger.Warn("failed to remove task dir", "task_id", id, "path", taskDir, "error", err)
		}
	}

	// 删除持久化文件
	persistFile := filepath.Join(m.persistDir, t.ID+".json")
	if err := os.Remove(persistFile); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove persist file", "task_id", id, "error", err)
	}

	// 清理 checksum（owner 作用域 key，与写入端一致，审查 M1）
	if m.checksumStore != nil {
		remotePath := filepath.Join(t.ID, t.Filename)
		m.checksumStore.Delete(checksumStoreKey(t.Owner, remotePath))
		m.logger.Debug("checksum deleted", "task_id", id, "remote_path", remotePath)
	}

	m.logger.Info("cloud download task deleted and cleaned up", "task_id", id)
	return nil
}

// saveTask 持久化单个任务到磁盘，返回写盘错误（供终态调用方显式处理）。
// 进度类保存（dirty flush）失败可容忍（调用方忽略返回值）；终态（completed/
// failed/cancelled）保存失败意味着重启后状态回滚，调用方必须记 Error 日志。
func (m *CloudDownloadManager) saveTask(t *CloudTask) error {
	// 检查任务是否已被删除（避免被删除后仍持久化）
	m.mu.RLock()
	_, exists := m.tasks[t.ID]
	m.mu.RUnlock()
	if !exists {
		m.logger.Debug("skip persisting deleted task", "id", t.ID)
		return nil
	}

	// 快照关键字段避免 data race（json.Marshal 期间任务可能被并发修改）
	m.mu.RLock()
	data, err := json.Marshal(t)
	m.mu.RUnlock()
	if err != nil {
		m.logger.Warn("failed to marshal task", "id", t.ID, "error", err)
		return err
	}
	taskFile := filepath.Join(m.persistDir, t.ID+".json")
	if err := os.WriteFile(taskFile, data, 0644); err != nil {
		m.logger.Warn("failed to persist task", "id", t.ID, "error", err)
		return err
	}
	return nil
}

// recoverTasks 从磁盘恢复所有任务。
// 仅重启 downloading 状态的任务（崩溃前正在下载中）。
// pending 任务不自动启动——避免 CreateTask 创建但未 SubmitAndStart 的任务在崩溃后意外启动。
func (m *CloudDownloadManager) recoverTasks() {
	entries, err := os.ReadDir(m.persistDir)
	if err != nil {
		return
	}
	recovered := 0
	restarted := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.persistDir, e.Name()))
		if err != nil {
			m.logger.Warn("failed to read persisted task, skipping", "file", e.Name(), "error", err)
			continue
		}
		var task CloudTask
		if err := json.Unmarshal(data, &task); err != nil {
			m.logger.Warn("failed to unmarshal persisted task, skipping", "file", e.Name(), "error", err)
			continue
		}
		// 重启后以磁盘实际占用为基准重算 ReservedSize，
		// 与 StorageManager 启动扫描的计数器保持一致（不信任崩溃前持久化的占位值）
		m.reconcileReservedSize(&task)
		m.tasks[task.ID] = &task
		recovered++

		// 仅重启 downloading 状态的任务（崩溃前正在下载）。
		// pending 任务不自动启动——避免 CreateTask 创建但尚未 SubmitAndStart
		// 就崩溃导致意外启动的边界情况。
		if task.Status == "downloading" {
			m.logger.Info("restarting interrupted download", "task_id", task.ID, "url", task.URL)
			m.mu.Lock()
			m.running[task.ID] = true
			m.mu.Unlock()
			m.wg.Add(1)
			go m.executeDownload(context.Background(), &task)
			restarted++
		}
	}
	if recovered > 0 {
		m.logger.Info("cloud download tasks recovered", "count", recovered, "restarted", restarted)
	}
}

// diskUsageOfTask 返回任务目录中所有普通文件的实际字节占用。
func (m *CloudDownloadManager) diskUsageOfTask(taskID string) int64 {
	dir := filepath.Join(m.cloudDir, taskID)
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// reconcileReservedSize 以任务目录实际占用为基准重算 ReservedSize。
// 进程重启后 StorageManager 的计数器来自磁盘扫描，这里让每个任务的预留量
// 与扫描结果一致，避免后续删除/清理时多退或少退。
func (m *CloudDownloadManager) reconcileReservedSize(task *CloudTask) {
	task.ReservedSize = m.diskUsageOfTask(task.ID)
}

// recoverGroups 从磁盘恢复任务组，并修剪已不存在的任务引用。
func (m *CloudDownloadManager) recoverGroups() {
	groupsDir := filepath.Join(m.persistDir, "groups")
	entries, err := os.ReadDir(groupsDir)
	if err != nil {
		return
	}
	recovered := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(groupsDir, e.Name()))
		if err != nil {
			continue
		}
		var group CloudTaskGroup
		if err := json.Unmarshal(data, &group); err != nil {
			continue
		}
		group.TaskIDs = m.pruneGroupTaskIDs(group.TaskIDs)
		if len(group.TaskIDs) == 0 {
			_ = os.Remove(filepath.Join(groupsDir, e.Name()))
			continue
		}
		m.groupMu.Lock()
		m.groups[group.ID] = &group
		m.groupMu.Unlock()
		recovered++
	}

	// 兼容旧数据：为带 GroupID 但缺少组记录的孤儿任务重建最小组
	var orphanGroups []*CloudTaskGroup
	m.mu.RLock()
	for _, t := range m.tasks {
		if t.GroupID == "" {
			continue
		}
		if _, ok := m.groups[t.GroupID]; ok {
			continue
		}
		group := &CloudTaskGroup{
			ID:         t.GroupID,
			Owner:      t.Owner, // 继承子任务 owner，避免带 owner 的组被重建为全局可见（组级隔离漏洞）
			Name:       t.GroupID,
			Status:     "pending",
			TaskIDs:    []string{t.ID},
			TotalTasks: 1,
			CreatedAt:  t.CreatedAt,
			UpdatedAt:  t.UpdatedAt,
			ExpiresAt:  t.ExpiresAt,
		}
		orphanGroups = append(orphanGroups, group)
	}
	m.mu.RUnlock()
	for _, group := range orphanGroups {
		m.groupMu.Lock()
		m.groups[group.ID] = group
		m.groupMu.Unlock()
		_ = m.saveGroup(group)
		recovered++
	}

	if recovered > 0 {
		m.logger.Info("cloud download groups recovered", "count", recovered)
	}
}

// pruneGroupTaskIDs 返回仍存在于 tasks 中的任务 ID 列表。
func (m *CloudDownloadManager) pruneGroupTaskIDs(ids []string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var kept []string
	for _, id := range ids {
		if _, ok := m.tasks[id]; ok {
			kept = append(kept, id)
		}
	}
	return kept
}

// groupsDirPath 返回组持久化目录。
func (m *CloudDownloadManager) groupsDirPath() string {
	return filepath.Join(m.persistDir, "groups")
}

// saveGroup 持久化任务组到磁盘，返回写盘错误。
// 组状态（completed/partial/archive_file 等）持久化失败意味着重启后组元数据丢失，
// 调用方（组状态变更点）必须显式处理；清理类保存可忽略返回值。
func (m *CloudDownloadManager) saveGroup(g *CloudTaskGroup) error {
	// 串行化整个 marshal+write：并发调用 saveGroup 时，若某个持有旧快照的保存
	// 在更新的保存之后落盘，重启会恢复出陈旧组状态（进度/状态回退）。持锁期间
	// marshal 反映当时的在内存最新状态，写盘按获取锁的顺序落盘，最后写盘者必为
	// 最新状态触发的保存（所有组状态变更路径都调用 saveGroup）。
	m.groupSaveMu.Lock()
	defer m.groupSaveMu.Unlock()

	m.groupMu.RLock()
	data, err := json.Marshal(g)
	m.groupMu.RUnlock()
	if err != nil {
		m.logger.Warn("failed to marshal group", "id", g.ID, "error", err)
		return err
	}
	dir := m.groupsDirPath()
	if err := os.MkdirAll(dir, 0755); err != nil {
		m.logger.Warn("failed to create groups dir", "dir", dir, "error", err)
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, g.ID+".json"), data, 0644); err != nil {
		m.logger.Warn("failed to persist group", "id", g.ID, "error", err)
		return err
	}
	return nil
}

// removeGroupFile 删除组的持久化文件。
func (m *CloudDownloadManager) removeGroupFile(groupID string) {
	_ = os.Remove(filepath.Join(m.groupsDirPath(), groupID+".json"))
}

// cleanupExpiredOnce 执行一次性的过期任务清理，返回清理的任务数量。
// 不包含循环，供测试直接调用。
//
// 注意：函数内部会先释放 m.mu 再执行 I/O 删除操作，调用者不应假设调用期间 mu 一直被持有。
func (m *CloudDownloadManager) cleanupExpiredOnce() int {
	now := time.Now()

	// 在锁内收集需要清理的 ID 及相关信息，避免锁内 I/O 阻塞
	type expiredItem struct {
		id           string
		taskID       string
		filename     string
		owner        string
		reservedSize int64
	}
	m.mu.Lock()
	var expired []expiredItem
	for id, t := range m.tasks {
		var ttl time.Duration
		switch t.Status {
		case "completed":
			ttl = m.config.TaskTTL
		case "failed", "cancelled":
			ttl = m.config.FailedTaskTTL
		default:
			continue
		}
		if now.After(t.UpdatedAt.Add(ttl)) {
			expired = append(expired, expiredItem{
				id:           id,
				taskID:       t.ID,
				filename:     t.Filename,
				owner:        t.Owner,
				reservedSize: t.ReservedSize,
			})
			t.ReservedSize = 0 // 释放后归零，防二次释放
			delete(m.tasks, id)
		}
	}
	m.mu.Unlock()

	if len(expired) == 0 {
		return 0
	}

	// 锁外执行 I/O 和 checksum 操作
	cleaned := 0
	for _, item := range expired {
		_ = os.Remove(filepath.Join(m.persistDir, item.id+".json"))
		_ = os.RemoveAll(filepath.Join(m.cloudDir, item.taskID))
		if item.reservedSize > 0 {
			m.storage.Release(item.reservedSize, CategoryCloud)
		}
		if m.checksumStore != nil {
			remotePath := filepath.Join(item.taskID, item.filename)
			m.checksumStore.Delete(checksumStoreKey(item.owner, remotePath))
		}
		cleaned++
	}

	// 清理引用已全部过期任务的空组（saveGroup 在锁外调用，避免 RLock 重入死锁）。
	// 锁序说明：此处 m.groupMu 下调用 pruneGroupTaskIDs（内部取 m.mu.RLock），
	// 嵌套顺序为 groupMu → mu；全代码库所有嵌套获取均遵循 groupMu → mu，
	// 无反向路径（m.mu → groupMu），因此不存在 ABBA 死锁。
	var toSave []*CloudTaskGroup
	m.groupMu.Lock()
	for gid, g := range m.groups {
		kept := m.pruneGroupTaskIDs(g.TaskIDs)
		if len(kept) == 0 {
			delete(m.groups, gid)
			m.removeGroupFile(gid)
		} else if len(kept) != len(g.TaskIDs) {
			g.TaskIDs = kept
			g.TotalTasks = len(kept)
			toSave = append(toSave, g)
		}
	}
	m.groupMu.Unlock()
	for _, g := range toSave {
		_ = m.saveGroup(g)
	}

	m.logger.Info("expired cloud download tasks cleaned up", "count", cleaned)
	return cleaned
}

// cleanupExpired 定期清理过期任务。
func (m *CloudDownloadManager) cleanupExpired() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.cleanupExpiredOnce()
		case <-m.stopCleanup:
			m.cleanupExpiredOnce() // 退出前清理一次
			return
		}
	}
}

func newGroupID() string {
	return newIDWithPrefix("group")
}

func newTaskID() string {
	return newIDWithPrefix("cloud")
}

func newIDWithPrefix(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	idCounter.mu.Lock()
	idCounter.n++
	n := idCounter.n
	idCounter.mu.Unlock()
	return fmt.Sprintf("%s-%s-%d", prefix, hex.EncodeToString(b), n)
}

var idCounter struct {
	mu sync.Mutex
	n  int64
}

// CreateGroup 创建下载任务组。
// owner 是请求认证派生的组归属，子任务写入同 owner（组级多租户隔离）。
// 校验文件名冲突，创建子任务。
func (m *CloudDownloadManager) CreateGroup(name string, urls []cloudfilename.Entry, owner string) (*CloudTaskGroup, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("at least one URL is required")
	}

	// 校验文件名冲突
	filenameSet := make(map[string]int)
	for _, entry := range urls {
		fn, err := cloudfilename.ResolveFilename(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid filename for %s: %w", entry.URL, err)
		}
		filenameSet[fn]++
	}
	var conflicts []string
	for fn, count := range filenameSet {
		if count > 1 {
			conflicts = append(conflicts, fn)
		}
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf("filename conflicts detected: %s; please specify unique filenames via request", strings.Join(conflicts, ", "))
	}

	groupID := newGroupID()
	now := time.Now()

	group := &CloudTaskGroup{
		ID:         groupID,
		Owner:      owner,
		Name:       name,
		Status:     "pending",
		TotalTasks: len(urls),
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(m.config.TaskTTL),
	}

	var taskIDs []string
	var newTaskIDs []string  // 本次新建的任务（回滚时删除）
	var absorbedIDs []string // 去重吸收的既有任务（回滚时清除组归属但不删除）
	seen := make(map[string]bool, len(urls))
	// rollback 在循环中途失败时清理"本次新建"的任务与存储预留，防止泄漏 pending 任务。
	// 去重吸收的既有独立任务不属于本组创建，回滚时不得删除（否则误删用户已有下载）。
	rollback := func() {
		// 先清除被吸收任务的组归属，避免悬挂引用到不存在的组
		for _, id := range absorbedIDs {
			var snap *CloudTask
			m.mu.Lock()
			t, ok := m.tasks[id]
			if ok {
				t.GroupID = ""
				c := *t
				snap = &c
			}
			m.mu.Unlock()
			if ok {
				_ = m.saveTask(snap)
			}
		}
		for i := len(newTaskIDs) - 1; i >= 0; i-- {
			if err := m.DeleteTask(newTaskIDs[i], owner); err != nil {
				m.logger.Warn("failed to rollback group task", "task_id", newTaskIDs[i], "error", err)
			}
		}
	}
	for _, entry := range urls {
		fn, err := cloudfilename.ResolveFilename(entry)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("invalid filename for %s: %w", entry.URL, err)
		}
		// 该 URL 已有**对请求者可见**的活跃任务 → 本次是去重吸收既有任务，回滚时不删除。
		// 跨 owner 的同 URL 任务不可见，不吸收（各自独立下载，防组归属性混乱）。
		absorbed := m.findByURL(entry.URL, owner) != nil

		task, err := m.CreateTask("url", entry.URL, fn, -1, owner)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("create task for %s: %w", entry.URL, err)
		}
		// 更新存储任务（而非 CreateTask 返回的副本）的 GroupID。
		// CreateTask 去重命中时返回的是快照副本，只改副本会导致内存与磁盘不一致。
		m.mu.Lock()
		stored, ok := m.tasks[task.ID]
		if !ok {
			m.mu.Unlock()
			rollback()
			return nil, fmt.Errorf("task disappeared during group creation: %s", task.ID)
		}
		// 去重命中：同组重复 URL 或已属其他组的活跃任务不允许重复入组
		if stored.GroupID != "" && stored.GroupID != groupID {
			m.mu.Unlock()
			rollback()
			return nil, fmt.Errorf("duplicate URL %s already belongs to group %s", entry.URL, stored.GroupID)
		}
		if seen[stored.ID] {
			m.mu.Unlock()
			rollback()
			return nil, fmt.Errorf("duplicate URL in group: %s", entry.URL)
		}
		stored.GroupID = groupID
		m.mu.Unlock()
		_ = m.saveTask(stored)
		taskIDs = append(taskIDs, stored.ID)
		if absorbed {
			absorbedIDs = append(absorbedIDs, stored.ID)
		} else {
			newTaskIDs = append(newTaskIDs, stored.ID)
		}
		seen[stored.ID] = true
	}

	group.TaskIDs = taskIDs

	m.groupMu.Lock()
	m.groups[groupID] = group
	m.groupMu.Unlock()
	if err := m.saveGroup(group); err != nil {
		m.logger.Error("persist new group failed, group may be lost on restart",
			"group_id", groupID, "error", err)
	}

	m.logger.Info("cloud download group created",
		"group_id", groupID,
		"name", name,
		"task_count", len(urls),
	)
	return group, nil
}

// SubmitAndStartGroup 创建组并启动所有子任务下载。
func (m *CloudDownloadManager) SubmitAndStartGroup(name string, urls []cloudfilename.Entry, owner string) (*CloudTaskGroup, error) {
	group, err := m.CreateGroup(name, urls, owner)
	if err != nil {
		return nil, err
	}

	for _, taskID := range group.TaskIDs {
		// 在写锁内检查 Status + 同步置位 running，闭合"检查→启动"竞态窗口：
		// 并发 SubmitAndStartGroup 对同一任务会有一个拿到 running 后另一个跳过，
		// 避免两个 goroutine 并发写同一 .partial。已在 running 的任务（可能是
		// 去重命中的既有任务）跳过启动。
		m.mu.Lock()
		task, exists := m.tasks[taskID]
		if exists && task.Status == "pending" && !m.running[taskID] {
			m.running[taskID] = true
		} else {
			task = nil
		}
		m.mu.Unlock()
		if task == nil {
			continue
		}
		m.wg.Add(1)
		go m.executeDownload(context.Background(), task)
	}

	m.UpdateGroupStatus(group.ID)
	return group, nil
}

// GetGroup 获取组详情，按请求者 owner 过滤（跨 owner 组视为不存在，404 防枚举）。
func (m *CloudDownloadManager) GetGroup(id, owner string) (*CloudTaskGroup, bool) {
	m.groupMu.RLock()
	defer m.groupMu.RUnlock()
	g, ok := m.groups[id]
	if !ok || !ownerVisible(g.Owner, owner) {
		return nil, false
	}
	c := *g
	return &c, true
}

// ListGroups 列出组，支持按 status 过滤与 offset/limit 分页。
// offset<0 时不偏移；limit<=0 时返回全部（兼容现有语义）。
// 排序：CreatedAt 降序 + ID 降序 tie-break（同 ListTasks 注释，确定性排序）。
// total 为按 status 过滤后的组总数（不受分页影响）。
// owner 非空时只返回匹配 owner 与空 owner（全局兼容）的组；空 owner（管理员/未认证）返回全部。
func (m *CloudDownloadManager) ListGroups(status string, offset, limit int, owner string) ([]*CloudTaskGroup, int) {
	m.groupMu.RLock()
	defer m.groupMu.RUnlock()

	var all []*CloudTaskGroup
	for _, g := range m.groups {
		if (status == "" || g.Status == status) && ownerVisible(g.Owner, owner) {
			c := *g
			all = append(all, &c)
		}
	}
	// CreatedAt 降序，ID 降序 tie-break（保持稳定排序）
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].ID > all[j].ID
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	total := len(all)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		return all, total
	}
	if offset >= total {
		return nil, total
	}
	// 防止 offset+limit 溢出（同 ListTasks）
	end := offset + min(limit, total-offset)
	return all[offset:end], total
}

// CancelGroup 取消组内所有 pending/downloading 任务（已完成任务跳过）。
// 按请求者 owner 过滤：跨 owner 组返回 not found（404 防枚举）。
func (m *CloudDownloadManager) CancelGroup(groupID, owner string) error {
	m.groupMu.RLock()
	group, ok := m.groups[groupID]
	m.groupMu.RUnlock()
	if !ok || !ownerVisible(group.Owner, owner) {
		return fmt.Errorf("group not found: %s", groupID)
	}

	var errs []error
	for _, tid := range group.TaskIDs {
		if err := m.CancelTask(tid, owner); err != nil {
			// 已完成/已失败任务不可取消、任务已被单独删除，均不视为组取消失败
			if strings.Contains(err.Error(), "cannot cancel") || strings.Contains(err.Error(), "task not found") {
				continue
			}
			errs = append(errs, err)
		}
	}
	m.UpdateGroupStatus(groupID)
	return errors.Join(errs...)
}

// DeleteGroup 删除组记录及所有子任务。
// 按请求者 owner 过滤：跨 owner 组返回 not found（404 防枚举）。
func (m *CloudDownloadManager) DeleteGroup(groupID, owner string) error {
	m.groupMu.RLock()
	group, ok := m.groups[groupID]
	m.groupMu.RUnlock()
	if !ok || !ownerVisible(group.Owner, owner) {
		return fmt.Errorf("group not found: %s", groupID)
	}

	var errs []error
	for _, tid := range group.TaskIDs {
		if err := m.DeleteTask(tid, owner); err != nil {
			// 组内任务被单独删除后，组级删除不应因此报错
			if strings.Contains(err.Error(), "task not found") {
				continue
			}
			errs = append(errs, err)
		}
	}

	m.groupMu.Lock()
	delete(m.groups, groupID)
	m.groupMu.Unlock()
	m.removeGroupFile(groupID)

	return errors.Join(errs...)
}

// waitTaskStopped 等待任务的下载 goroutine 完全退出（running 标记清除）。
// 最多等待 timeout。返回 false 表示超时仍未退出。
// 调用方不得持有 m.mu（本函数内部需取读锁）。
func (m *CloudDownloadManager) waitTaskStopped(taskID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		m.mu.RLock()
		running := m.running[taskID]
		m.mu.RUnlock()
		if !running {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ResumeTask 恢复失败的下载任务。
// force=true 时删除已有部分文件重新下载；force=false 时保留 .partial 由下载器
// 通过 Range 续传（不再改名成 destPath，避免续传退化为全量下载）。
// 按请求者 owner 过滤：跨 owner 任务返回 not found（404 防枚举）。
func (m *CloudDownloadManager) ResumeTask(taskID string, force bool, owner string) error {
	m.mu.Lock()
	task, ok := m.tasks[taskID]
	if !ok || !ownerVisible(task.Owner, owner) {
		m.mu.Unlock()
		return fmt.Errorf("task not found: %s", taskID)
	}
	if task.Status != "failed" && task.Status != "cancelled" {
		m.mu.Unlock()
		return fmt.Errorf("task %s is in status %q, only failed/cancelled tasks can be resumed", taskID, task.Status)
	}
	// 释放写锁再等待：waitTaskStopped 内部需取读锁，持有写锁会死锁。
	// 等待期间任务可能被删除或状态被并发修改，之后会重新校验。
	m.mu.Unlock()

	// cancelled 状态由 CancelTask 提前写入，不代表旧 goroutine 已停止写盘：
	// 等待其完全退出（running 标记清除），避免新旧 goroutine 并发 append 同一
	// .partial 文件导致损坏。failed 状态由旧 goroutine 自身在写盘结束后写入，
	// running 此时已清除，本等待立即返回。
	if !m.waitTaskStopped(taskID, m.config.IdleTimeout+5*time.Second) {
		return fmt.Errorf("task %s is still finishing previous download, try again later", taskID)
	}

	m.mu.Lock()
	task, ok = m.tasks[taskID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("task not found: %s", taskID)
	}
	if task.Status != "failed" && task.Status != "cancelled" {
		m.mu.Unlock()
		return fmt.Errorf("task %s is in status %q, only failed/cancelled tasks can be resumed", taskID, task.Status)
	}
	if m.running[taskID] {
		m.mu.Unlock()
		return fmt.Errorf("task %s is still running, cannot resume now", taskID)
	}

	// 状态先切 pending + running 同步置位：并发双 resume 中第二个会因 running 已置
	// 被上面的检查拦截，避免两个 goroutine 并发写同一 .partial（Critical 修复）。
	task.Status = "pending"
	m.running[taskID] = true
	task.Error = ""
	task.UpdatedAt = time.Now()
	task.ExpiresAt = time.Now().Add(m.config.TaskTTL)

	// 释放过存储的任务需要重新占位
	if task.ReservedSize == 0 {
		if err := m.storage.TryReserve(cloudReservePlaceholder, CategoryCloud); err != nil {
			// 占位失败：撤销 pending 切换并清除 running，避免 running 残留
			// 永久阻止后续 resume（goroutine 从未启动）。
			task.Status = "failed"
			task.Error = "storage full, cannot resume"
			delete(m.running, taskID)
			m.mu.Unlock()
			if saveErr := m.saveTask(task); saveErr != nil {
				m.logger.Error("persist resume-failure task state, state may be lost on restart",
					"task_id", taskID, "error", saveErr)
			}
			return err
		}
		task.ReservedSize = cloudReservePlaceholder
	}
	m.mu.Unlock()

	taskDir := filepath.Join(m.cloudDir, task.ID)
	destPath := filepath.Join(taskDir, task.Filename)
	if force {
		_ = os.Remove(destPath)
		_ = os.Remove(destPath + ".partial")
		_ = os.Remove(destPath + ".partial.etag")
	}

	if err := m.saveTask(task); err != nil {
		m.logger.Error("persist resumed task state, state may be lost on restart",
			"task_id", taskID, "error", err)
	}

	m.wg.Add(1)
	go m.executeDownload(context.Background(), task)
	return nil
}

// ResumeGroup 恢复组内所有失败/取消任务。
// 按请求者 owner 过滤：跨 owner 组返回 not found（404 防枚举）。
func (m *CloudDownloadManager) ResumeGroup(groupID string, force bool, owner string) error {
	m.groupMu.RLock()
	group, ok := m.groups[groupID]
	m.groupMu.RUnlock()
	if !ok || !ownerVisible(group.Owner, owner) {
		return fmt.Errorf("group not found: %s", groupID)
	}

	var errs []error
	for _, tid := range group.TaskIDs {
		if err := m.ResumeTask(tid, force, owner); err != nil {
			// 组内任务被单独删除后，组级恢复不应因此报错（与 CancelGroup/DeleteGroup 一致，
			// 否则整个组 resume 会误返回 404 让用户以为组不存在）。
			if strings.Contains(err.Error(), "task not found") {
				continue
			}
			errs = append(errs, err)
		}
	}
	m.UpdateGroupStatus(groupID)
	return errors.Join(errs...)
}

// SetGroupArchiveFile 记录组的归档文件路径并持久化。
func (m *CloudDownloadManager) SetGroupArchiveFile(groupID, archiveFile string) {
	m.groupMu.Lock()
	g, ok := m.groups[groupID]
	if !ok {
		m.groupMu.Unlock()
		return
	}
	g.ArchiveFile = archiveFile
	g.UpdatedAt = time.Now()
	m.groupMu.Unlock()
	if err := m.saveGroup(g); err != nil {
		m.logger.Error("persist group archive_file failed, group state may be lost on restart",
			"group_id", groupID, "error", err)
	}
}

// markDirty 将任务标记为"脏"（进度已更新），由 flushLoop 批量持久化。
func (m *CloudDownloadManager) markDirty(id string) {
	m.dirtyMu.Lock()
	m.dirtyTasks[id] = struct{}{}
	m.dirtyMu.Unlock()
}

// flushLoop 每 30 秒批量持久化脏任务的进度更新。
func (m *CloudDownloadManager) flushLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.flushDirty()
		case <-m.flushNow:
			m.flushDirty()
		case <-m.stopFlush:
			m.flushDirty()
			return
		}
	}
}

// flushDirty 将所有脏任务的当前状态持久化到磁盘。
func (m *CloudDownloadManager) flushDirty() {
	m.dirtyMu.Lock()
	ids := make([]string, 0, len(m.dirtyTasks))
	for id := range m.dirtyTasks {
		ids = append(ids, id)
	}
	m.dirtyTasks = make(map[string]struct{})
	m.dirtyMu.Unlock()

	if len(ids) == 0 {
		return
	}

	for _, id := range ids {
		m.mu.RLock()
		task, ok := m.tasks[id]
		m.mu.RUnlock()
		if !ok {
			continue
		}
		// 进度类批量持久化 best-effort（saveTask 内部已 Warn），失败不阻塞
		_ = m.saveTask(task)
	}
}

// UpdateGroupStatus 根据子任务状态更新组状态（导出方法，供 handler 调用）。
func (m *CloudDownloadManager) UpdateGroupStatus(groupID string) {
	// 在 groupMu 下取 TaskIDs 局部副本，避免与 cleanupExpiredOnce 的写入
	// 形成跨锁竞争（groupMu.Lock vs m.mu.RLock 无 happens-before）。
	m.groupMu.RLock()
	group, ok := m.groups[groupID]
	var taskIDs []string
	if ok {
		taskIDs = append([]string(nil), group.TaskIDs...)
	}
	m.groupMu.RUnlock()
	if !ok {
		return
	}

	m.mu.RLock()
	completed, failed, cancelled, active, pending := 0, 0, 0, 0, 0
	for _, tid := range taskIDs {
		task, exists := m.tasks[tid]
		if !exists {
			continue
		}
		switch task.Status {
		case "completed":
			completed++
		case "failed":
			failed++
		case "cancelled":
			cancelled++
		case "downloading":
			active++
		default:
			pending++
		}
	}
	total := completed + failed + cancelled + active + pending
	m.mu.RUnlock()

	m.groupMu.Lock()
	changed := group.Completed != completed ||
		group.Failed != failed ||
		group.Cancelled != cancelled ||
		group.TotalTasks != total
	var newStatus string
	// 只要还有未终止的任务（downloading 或 pending），组状态为 downloading。
	// 一旦所有子任务进入终态，按 failed/cancelled/completed 优先级判定。
	switch {
	case total == 0:
		// 所有子任务已删除，组已完成其生命周期
		newStatus = "completed"
	case active > 0 || pending > 0:
		newStatus = "downloading"
	case failed > 0:
		newStatus = "failed"
	case cancelled > 0:
		newStatus = "cancelled"
	default:
		newStatus = "completed"
	}
	if newStatus != group.Status {
		group.Status = newStatus
		changed = true
	}
	if !changed {
		// Web UI 轮询会高频调用本方法，状态与计数未变化时跳过落盘
		m.groupMu.Unlock()
		return
	}
	group.Completed = completed
	group.Failed = failed
	group.Cancelled = cancelled
	group.TotalTasks = total
	group.UpdatedAt = time.Now()
	m.groupMu.Unlock()
	// 组状态变更持久化失败会导致重启后组进度回退，必须显式报错
	if err := m.saveGroup(group); err != nil {
		m.logger.Error("persist group status failed, group state may be lost on restart",
			"group_id", groupID, "error", err)
	}
}

// Close 停止所有后台 goroutine（flushLoop 和 cleanupExpired）并等待下载完成。
// 在进程退出前应调用一次。多次调用安全。
// 注意：优雅关闭不取消进行中的下载任务——下载 goroutine 在进程退出时自然终止，
// .partial 文件保留，重启后通过 recoverTasks 恢复并通过 Range 续传继续。
// wg.Wait 最多等待 30 秒，超时后返回（防止下载 goroutine 卡在 I/O 上永久阻塞）。
func (m *CloudDownloadManager) Close() {
	m.closeOnce.Do(func() {
		close(m.stopFlush)
		close(m.stopCleanup)

		// 带超时的 Wait，防止下载 goroutine 卡在 I/O 上永久阻塞
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			m.logger.Warn("cloud download manager Close timed out waiting for goroutines")
		}
	})
}

// FlushNow 立即触发一次批量持久化（测试用）。
func (m *CloudDownloadManager) FlushNow() {
	m.flushDirty()
}
