// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/downloader"
)

// downloadsDirName 是云端下载持久化目录名。
const downloadsDirName = ".__downloads__"

// CloudTask 表示一个云端下载任务。
type CloudTask struct {
	ID           string    `json:"id"`
	URL          string    `json:"url"`
	Method       string    `json:"method"`     // "url" | "upload"
	Filename     string    `json:"filename"`   // 云端存储文件名
	Status       string    `json:"status"`     // pending | downloading | completed | failed | cancelled
	TotalSize    int64     `json:"total_size"` // -1 表示未知
	Downloaded   int64     `json:"downloaded"`
	Checksum     string    `json:"checksum"`
	FileMTime    int64     `json:"file_mtime,omitempty"` // 原始文件修改时间（UnixNano），从 URL 的 Last-Modified 提取
	Error        string    `json:"error"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	ReservedSize int64     `json:"-"`                  // 实际预留量，不持久化
	GroupID      string    `json:"group_id,omitempty"` // 所属组 ID（可选）
}

// CloudTaskGroup 表示一个云端下载任务组。
// 组内所有子任务文件下载到同一目录 .__cloud__/<groupID>/ 下。
type CloudTaskGroup struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"` // pending | downloading | completed | partial | failed | cancelled
	TaskIDs     []string  `json:"task_ids"`
	TotalTasks  int       `json:"total_tasks"`
	Completed   int       `json:"completed"`
	Failed      int       `json:"failed"`
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
	TaskTTL         time.Duration // 完成任务保留时间，默认 24h
	FailedTaskTTL   time.Duration // 失败任务保留时间，默认 1h
	AllowPrivate    bool          // 允许私有 IP 下载（仅测试用）
	DownloadTimeout time.Duration // 单次下载超时，默认 0（不限制）
	MaxRetries      int           // 失败重试次数，默认 10
	RetryDelay      time.Duration // 重试间隔，默认 10s
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

	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 1 // 防止死锁：MaxConcurrent=0 时 semaphore 永远阻塞
	}

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
		dl:            downloader.NewFromConfig("http"),
		cancelFuncs:   make(map[string]context.CancelFunc),
		metrics:       &CloudMetrics{},
		dirtyTasks:    make(map[string]struct{}),
		flushNow:      make(chan struct{}, 1),
		stopFlush:     make(chan struct{}),
		stopCleanup:   make(chan struct{}),
		groups:        make(map[string]*CloudTaskGroup),
	}

	mgr.logger.Info("cloud download manager initialized",
		"max_concurrent", cfg.MaxConcurrent,
		"sync_threshold", cfg.SyncThreshold,
		"task_ttl", cfg.TaskTTL,
		"failed_task_ttl", cfg.FailedTaskTTL,
	)

	// 允许私有 IP 时跳过 SSRF 后验证（仅测试用）
	// 注意：必须创建副本而非修改共享注册表的下载器，避免 data race
	if cfg.AllowPrivate {
		if hd, ok := mgr.dl.(*downloader.HTTPDownloader); ok {
			// 创建副本并清空 ValidateURLAfterDo
			clone := *hd
			clone.ValidateURLAfterDo = nil
			mgr.dl = &clone
		}
	}

	// 传递超时配置到 HTTPDownloader
	if cfg.DownloadTimeout > 0 {
		if hd, ok := mgr.dl.(*downloader.HTTPDownloader); ok {
			clone := *hd
			clone.Timeout = cfg.DownloadTimeout
			mgr.dl = &clone
		}
	}

	// 恢复持久化的任务
	mgr.recoverTasks()

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
// 自动去重：相同 URL 的 pending/downloading 任务返回已有任务。
func (m *CloudDownloadManager) CreateTask(method, url, filename string, totalSize int64) (*CloudTask, error) {
	// URL 去重：检查是否存在相同 URL 的活跃任务
	if existing := m.findByURL(url); existing != nil {
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
		reserved = 1 * 1024 * 1024 * 1024 // 1 GiB 保底
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

	m.saveTask(task)
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
// 小文件（< syncThreshold）同步执行，大文件异步执行。
// syncCtx 为 nil 时始终异步。
func (m *CloudDownloadManager) SubmitAndStart(method, url, filename string, totalSize int64, syncCtx context.Context) (*CloudTask, error) {
	task, err := m.CreateTask(method, url, filename, totalSize)
	if err != nil {
		return nil, err
	}

	// 如果返回的是已有任务（去重命中），检查是否需要启动
	if task.Status != "pending" {
		return task, nil
	}

	useSync := syncCtx != nil && totalSize > 0 && totalSize < m.config.SyncThreshold

	if useSync {
		m.logger.Info("starting sync cloud download", "task_id", task.ID, "url", url, "size", totalSize)
		// 同步下载：直接在当前 goroutine 执行，wg.Add(1) 在 go 之前确保不竞态
		m.wg.Add(1)
		m.executeDownload(syncCtx, task)
		return task, nil
	}

	m.logger.Info("starting async cloud download", "task_id", task.ID, "url", url, "size", totalSize)
	// 异步下载：goroutine 执行，wg.Add(1) 在 go 之前确保不竞态
	//nolint:gosec
	m.wg.Add(1)
	go m.executeDownload(context.Background(), task) //nolint:gosec
	return task, nil
}

// executeDownload 执行实际下载逻辑。
// 注意：调用者必须保证在调用前已调 m.wg.Add(1)，函数退出时自动 m.wg.Done()。
func (m *CloudDownloadManager) executeDownload(ctx context.Context, task *CloudTask) {
	defer m.wg.Done()
	defer m.metrics.ActiveDownloads.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("panic in download", "task_id", task.ID, "panic", r)
			m.failTask(task, fmt.Sprintf("panic: %v", r))
		}
	}()

	// 获取信号量
	select {
	case m.semaphore <- struct{}{}:
		defer func() { <-m.semaphore }()
	case <-ctx.Done():
		m.failTask(task, "cancelled before start")
		return
	}

	// 创建可取消的 context（从 Background 派生，使客户端断连后下载可继续异步重试）
	dlCtx, cancel := context.WithCancel(context.Background())
	defer cancel() // 确保 cancel 在函数返回时被调用（linter: G118）

	// 应用超时配置
	if m.config.DownloadTimeout > 0 {
		dlCtx, cancel = context.WithTimeout(dlCtx, m.config.DownloadTimeout)
		defer cancel()
	}

	m.mu.Lock()
	m.cancelFuncs[task.ID] = cancel
	task.Status = "downloading"
	task.UpdatedAt = time.Now()
	m.mu.Unlock()
	m.saveTask(task)

	m.logger.Info("download started", "task_id", task.ID, "url", task.URL, "filename", task.Filename)
	m.metrics.ActiveDownloads.Add(1)

	// 构建目标文件路径
	taskDir := filepath.Join(m.cloudDir, task.ID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		m.logger.Warn("创建任务目录失败", "task_id", task.ID, "dir", taskDir, "error", err)
		m.failTask(task, fmt.Sprintf("create task dir: %v", err))
		return
	}
	destPath := filepath.Join(taskDir, task.Filename)

	// 执行下载（带重试）
	maxRetries := max(m.config.MaxRetries,
		// 至少执行一次
		1)
	var result *downloader.Result
	var downloadErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			// 重试等待
			m.metrics.TasksRetried.Add(1)
			delay := m.config.RetryDelay
			if delay <= 0 {
				delay = 10 * time.Second
			}
			select {
			case <-time.After(delay):
			case <-dlCtx.Done():
				downloadErr = dlCtx.Err()
				goto downloadDone
			}
			m.logger.Info("retrying download", "task_id", task.ID, "url", task.URL, "attempt", attempt+1, "max", maxRetries)
		}

		result, downloadErr = m.dl.Download(dlCtx, task.URL, destPath, func(downloaded, total int64) {
			m.mu.Lock()
			defer m.mu.Unlock()
			task.Downloaded = downloaded
			if total > 0 {
				task.TotalSize = total
			}
			// 标记为脏，由 flushLoop 每 30 秒批量持久化
			m.markDirty(task.ID)
		})

		if downloadErr == nil {
			break
		}

		// 检查是否应该重试
		if dlCtx.Err() != nil {
			// 上下文取消或超时，不再重试
			break
		}
		// 最后一次尝试失败，不再重试
		if attempt >= maxRetries-1 {
			break
		}
	}

downloadDone:
	m.mu.Lock()
	delete(m.cancelFuncs, task.ID)
	m.mu.Unlock()

	if downloadErr != nil {
		if ctx.Err() != nil && dlCtx.Err() == nil {
			// 客户端断开（只有外层 ctx 取消，内层 dlCtx 未取消），转为异步继续
			m.logger.Info("sync download client disconnected, switching to async",
				"task_id", task.ID, "url", task.URL)
			//nolint:gosec // G118: 断线后异步继续需要独立 context
			// wg.Add(1) 在 go 之前确保不竞态
			m.wg.Add(1)
			go m.executeDownload(context.Background(), task) //nolint:gosec
			return
		}
		if dlCtx.Err() != nil {
			m.failTask(task, "cancelled")
			m.logger.Info("download cancelled", "task_id", task.ID)
		} else {
			m.failTask(task, downloadErr.Error())
			m.logger.Error("download failed", "task_id", task.ID, "url", task.URL, "error", downloadErr)
		}
		return
	}

	// 恢复原始文件 mtime
	if result.ModTime != (time.Time{}) {
		modTime := result.ModTime
		if err := os.Chtimes(destPath, modTime, modTime); err != nil {
			m.logger.Warn("设置文件修改时间失败", "task_id", task.ID, "error", err)
		}
		m.mu.Lock()
		task.FileMTime = result.ModTime.UnixNano()
		m.mu.Unlock()
	}

	// 补偿存储空间（实际大小可能与预估值不同）
	m.mu.Lock()
	currentTotal := task.TotalSize
	m.mu.Unlock()
	sizeDelta := result.Size - currentTotal
	if sizeDelta > 0 {
		// 实际更大，需要追加预留
		if err := m.storage.TryReserve(sizeDelta, CategoryCloud); err != nil {
			m.failTask(task, "storage full after download")
			os.Remove(destPath)
			m.logger.Error("storage full after download, cannot fit actual size",
				"task_id", task.ID, "actual_size", result.Size, "reserved", currentTotal)
			return
		}
		m.mu.Lock()
		task.ReservedSize += sizeDelta
		m.mu.Unlock()
	} else if sizeDelta < 0 {
		// 实际更小，释放多余空间
		m.storage.Release(-sizeDelta, CategoryCloud)
		m.mu.Lock()
		task.ReservedSize += sizeDelta
		m.mu.Unlock()
	}

	// 写入 ChecksumStore
	remotePath := filepath.Join(cloudDirName, task.ID, task.Filename)
	if m.checksumStore != nil {
		m.checksumStore.Set(remotePath, result.Checksum)
	}

	// 更新任务状态
	m.mu.Lock()
	task.Status = "completed"
	task.TotalSize = result.Size
	task.Downloaded = result.Size
	task.Checksum = result.Checksum
	task.UpdatedAt = time.Now()
	task.ExpiresAt = time.Now().Add(m.config.TaskTTL)
	m.mu.Unlock()

	m.saveTask(task)
	m.logger.Info("download completed",
		"task_id", task.ID,
		"url", task.URL,
		"size", result.Size,
		"checksum", result.Checksum[:16]+"...",
	)
	m.metrics.TasksCompleted.Add(1)
	m.metrics.BytesDownloaded.Add(result.Size)
}

// failTask 将任务标记为失败，并清理任务目录。
func (m *CloudDownloadManager) failTask(task *CloudTask, errMsg string) {
	m.mu.Lock()
	if task.Status == "failed" || task.Status == "completed" {
		m.mu.Unlock()
		return
	}
	if task.ReservedSize > 0 {
		m.storage.Release(task.ReservedSize, CategoryCloud)
	}
	task.Status = "failed"
	task.Error = errMsg
	task.UpdatedAt = time.Now()
	task.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)
	m.mu.Unlock()
	m.saveTask(task)
	m.metrics.TasksFailed.Add(1)

	// 清理任务目录（下载失败后不残留垃圾文件）
	taskDir := filepath.Join(m.cloudDir, task.ID)
	if err := os.RemoveAll(taskDir); err != nil {
		m.logger.Warn("failed to clean up task dir on fail", "task_id", task.ID, "error", err)
	}
}

// findByURL 查找相同 URL 的活跃任务（去重）。
// 仅匹配 pending/downloading 状态（排除 completed/failed/cancelled）。
// TODO: 如果 URL 数量增长到数百级别，考虑建立 url→ID 索引避免 O(n) 遍历。
func (m *CloudDownloadManager) findByURL(url string) *CloudTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tasks {
		if t.URL == url && (t.Status == "pending" || t.Status == "downloading") {
			c := *t
			return &c
		}
	}
	return nil
}

// GetTask 按 ID 获取任务。
func (m *CloudDownloadManager) GetTask(id string) (*CloudTask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, false
	}
	c := *t
	return &c, true
}

// SnapshotTask 返回任务的快照（副本），避免并发修改导致 data race。
func (m *CloudDownloadManager) SnapshotTask(id string) (*CloudTask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, false
	}
	c := *t
	return &c, true
}

// ListTasks 列出所有任务，支持按 status 过滤。
func (m *CloudDownloadManager) ListTasks(status string) []*CloudTask {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*CloudTask
	for _, t := range m.tasks {
		if status == "" || t.Status == status {
			c := *t
			result = append(result, &c)
		}
	}
	return result
}

// CancelTask 取消正在进行的任务。
func (m *CloudDownloadManager) CancelTask(id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
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

	// 释放 TryReserve 预留的存储空间
	if t.TotalSize > 0 {
		m.storage.Release(t.TotalSize, CategoryCloud)
	}

	// 触发下载取消
	if cancel, ok := m.cancelFuncs[id]; ok {
		cancel()
		delete(m.cancelFuncs, id)
	}
	m.mu.Unlock()

	m.saveTask(t)
	m.metrics.TasksCancelled.Add(1)
	m.logger.Info("cloud download task cancelled", "task_id", id)
	return nil
}

// DeleteTask 删除任务及其云端文件。
func (m *CloudDownloadManager) DeleteTask(id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("task not found: %s", id)
	}

	// 如果正在下载，先取消
	if cancel, ok := m.cancelFuncs[id]; ok {
		cancel()
		delete(m.cancelFuncs, id)
	}

	delete(m.tasks, id)
	m.mu.Unlock()

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

	// 释放存储空间
	if t.TotalSize > 0 {
		m.storage.Release(t.TotalSize, CategoryCloud)
		m.logger.Debug("storage released", "task_id", id, "size", t.TotalSize)
	}

	// 清理 checksum
	if m.checksumStore != nil {
		remotePath := filepath.Join(cloudDirName, t.ID, t.Filename)
		m.checksumStore.Delete(remotePath)
		m.logger.Debug("checksum deleted", "task_id", id, "remote_path", remotePath)
	}

	m.logger.Info("cloud download task deleted and cleaned up", "task_id", id)
	return nil
}

// saveTask 持久化单个任务到磁盘。
func (m *CloudDownloadManager) saveTask(t *CloudTask) {
	// 检查任务是否已被删除（避免被删除后仍持久化）
	m.mu.RLock()
	_, exists := m.tasks[t.ID]
	m.mu.RUnlock()
	if !exists {
		m.logger.Debug("skip persisting deleted task", "id", t.ID)
		return
	}

	// 快照关键字段避免 data race（json.Marshal 期间任务可能被并发修改）
	m.mu.RLock()
	data, err := json.Marshal(t)
	m.mu.RUnlock()
	if err != nil {
		m.logger.Warn("failed to marshal task", "id", t.ID, "error", err)
		return
	}
	taskFile := filepath.Join(m.persistDir, t.ID+".json")
	if err := os.WriteFile(taskFile, data, 0644); err != nil {
		m.logger.Warn("failed to persist task", "id", t.ID, "error", err)
	}
}

// recoverTasks 从磁盘恢复所有任务。
// downloading 状态的任务自动重启下载。
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
			continue
		}
		var task CloudTask
		if err := json.Unmarshal(data, &task); err != nil {
			continue
		}
		m.tasks[task.ID] = &task
		recovered++

		// 重启正在下载的任务，wg.Add(1) 在 go 之前确保不竞态
		if task.Status == "downloading" {
			m.logger.Info("restarting interrupted download", "task_id", task.ID, "url", task.URL)
			m.wg.Add(1)
			go m.executeDownload(context.Background(), &task)
			restarted++
		}
	}
	if recovered > 0 {
		m.logger.Info("cloud download tasks recovered", "count", recovered, "restarted", restarted)
	}
}

// cleanupExpiredOnce 执行一次性的过期任务清理，返回清理的任务数量。
// 不包含循环，供测试直接调用。
//
// 注意：函数内部会先释放 m.mu 再执行 I/O 删除操作，调用者不应假设调用期间 mu 一直被持有。
func (m *CloudDownloadManager) cleanupExpiredOnce() int {
	now := time.Now()

	// 在锁内收集需要清理的 ID 及相关信息，避免锁内 I/O 阻塞
	type expiredItem struct {
		id       string
		taskID   string
		filename string
		totalSz  int64
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
				id:       id,
				taskID:   t.ID,
				filename: t.Filename,
				totalSz:  t.TotalSize,
			})
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
		if item.totalSz > 0 {
			m.storage.Release(item.totalSz, CategoryCloud)
		}
		if m.checksumStore != nil {
			remotePath := filepath.Join(cloudDirName, item.taskID, item.filename)
			m.checksumStore.Delete(remotePath)
		}
		cleaned++
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

var taskIDCounter struct {
	mu sync.Mutex
	n  int64
}

func newTaskID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	taskIDCounter.mu.Lock()
	taskIDCounter.n++
	n := taskIDCounter.n
	taskIDCounter.mu.Unlock()
	return fmt.Sprintf("cloud-%s-%d", hex.EncodeToString(b), n)
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
		m.saveTask(task)
	}
}

// Close 停止所有后台 goroutine（flushLoop 和 cleanupExpired）并执行一次清理。
// 在进程退出前应调用一次。多次调用安全。
func (m *CloudDownloadManager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		for id, cancel := range m.cancelFuncs {
			cancel()
			delete(m.cancelFuncs, id)
		}
		m.mu.Unlock()
		close(m.stopFlush)
		close(m.stopCleanup)
		m.wg.Wait()
	})
}

// FlushNow 立即触发一次批量持久化（测试用）。
func (m *CloudDownloadManager) FlushNow() {
	m.flushDirty()
}
