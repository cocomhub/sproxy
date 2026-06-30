// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/downloader"
)

// CloudTask 表示一个云端下载任务。
type CloudTask struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	Method     string    `json:"method"`     // "url" | "upload"
	Filename   string    `json:"filename"`   // 云端存储文件名
	Status     string    `json:"status"`     // pending | downloading | completed | failed | cancelled
	TotalSize  int64     `json:"total_size"` // -1 表示未知
	Downloaded int64     `json:"downloaded"`
	Checksum   string    `json:"checksum"`
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// CloudDownloadConfig 云端下载配置。
type CloudDownloadConfig struct {
	SyncThreshold int64         // 同步模式阈值（字节），默认 20 MiB
	MaxConcurrent int           // 最大并发下载数，默认 3
	TaskTTL       time.Duration // 完成任务保留时间，默认 24h
	FailedTaskTTL time.Duration // 失败任务保留时间，默认 1h
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
}

// NewCloudDownloadManager 创建云端下载管理器。
func NewCloudDownloadManager(uploadsDir string, sm *StorageManager, cs ChecksumStoreIface, logger *slog.Logger, cfg *CloudDownloadConfig) *CloudDownloadManager {
	cloudDir := filepath.Join(uploadsDir, cloudDirName)
	persistDir := filepath.Join(uploadsDir, ".__downloads__")
	_ = os.MkdirAll(cloudDir, 0755)
	_ = os.MkdirAll(persistDir, 0755)

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
	}

	mgr.logger.Info("cloud download manager initialized",
		"max_concurrent", cfg.MaxConcurrent,
		"sync_threshold", cfg.SyncThreshold,
		"task_ttl", cfg.TaskTTL,
		"failed_task_ttl", cfg.FailedTaskTTL,
	)

	// 恢复持久化的任务
	mgr.recoverTasks()

	// 启动过期任务清理
	go mgr.cleanupExpired()

	return mgr
}

// CreateTask 创建云端下载任务（不启动下载）。
// 自动去重：相同 URL 的 pending/downloading/completed 任务返回已有任务。
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
	if totalSize <= 0 {
		totalSize = 1024 * 1024 // 默认预留 1 MiB
	}
	if err := m.storage.TryReserve(totalSize, CategoryCloud); err != nil {
		m.logger.Warn("storage full, cloud download rejected",
			"url", url,
			"requested_size", totalSize,
			"current_usage", m.storage.Usage(),
			"max_bytes", m.storage.MaxBytes(),
		)
		return nil, err
	}

	task := &CloudTask{
		ID:        newTaskID(),
		URL:       url,
		Method:    method,
		Filename:  filename,
		Status:    "pending",
		TotalSize: totalSize,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		ExpiresAt: time.Now().Add(m.config.TaskTTL),
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
		// 同步下载：直接在当前 goroutine 执行
		m.executeDownload(syncCtx, task)
		return task, nil
	}

	m.logger.Info("starting async cloud download", "task_id", task.ID, "url", url, "size", totalSize)
	// 异步下载：goroutine 执行
	//nolint:gosec // G118: 异步下载需要独立 context，不受请求生命周期限制
	go m.executeDownload(context.Background(), task)
	return task, nil
}

// executeDownload 执行实际下载逻辑。
func (m *CloudDownloadManager) executeDownload(ctx context.Context, task *CloudTask) {
	// 获取信号量
	select {
	case m.semaphore <- struct{}{}:
		defer func() { <-m.semaphore }()
	case <-ctx.Done():
		m.failTask(task, "cancelled before start")
		return
	}

	// 创建可取消的 context
	dlCtx, cancel := context.WithCancel(ctx)
	defer cancel() // 确保 cancel 在函数返回时被调用（linter: G118）
	m.mu.Lock()
	m.cancelFuncs[task.ID] = cancel
	task.Status = "downloading"
	task.UpdatedAt = time.Now()
	m.mu.Unlock()
	m.saveTask(task)

	m.logger.Info("download started", "task_id", task.ID, "url", task.URL, "filename", task.Filename)

	// 构建目标文件路径
	taskDir := filepath.Join(m.cloudDir, task.ID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		m.failTask(task, fmt.Sprintf("create task dir: %v", err))
		return
	}
	destPath := filepath.Join(taskDir, task.Filename)

	// 执行下载
	result, err := m.dl.Download(dlCtx, task.URL, destPath, func(downloaded, total int64) {
		m.mu.Lock()
		task.Downloaded = downloaded
		if total > 0 {
			task.TotalSize = total
		}
		m.mu.Unlock()
	})

	m.mu.Lock()
	delete(m.cancelFuncs, task.ID)
	m.mu.Unlock()

	if err != nil {
		if ctx.Err() != nil && dlCtx.Err() == nil {
			// 客户端断开（只有外层 ctx 取消，内层 dlCtx 未取消），转为异步继续
			m.logger.Info("sync download client disconnected, switching to async",
				"task_id", task.ID, "url", task.URL)
			//nolint:gosec // G118: 断线后异步继续需要独立 context
			go m.executeDownload(context.Background(), task)
			return
		}
		if dlCtx.Err() != nil {
			m.failTask(task, "cancelled")
			m.logger.Info("download cancelled", "task_id", task.ID)
		} else {
			m.failTask(task, err.Error())
			m.logger.Error("download failed", "task_id", task.ID, "url", task.URL, "error", err)
		}
		return
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
	} else if sizeDelta < 0 {
		// 实际更小，释放多余空间
		m.storage.Release(-sizeDelta, CategoryCloud)
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
}

// failTask 将任务标记为失败。
func (m *CloudDownloadManager) failTask(task *CloudTask, errMsg string) {
	m.mu.Lock()
	task.Status = "failed"
	task.Error = errMsg
	task.UpdatedAt = time.Now()
	task.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)
	m.mu.Unlock()
	m.saveTask(task)
}

// findByURL 查找相同 URL 的活跃任务（去重）。
// 仅匹配 pending/downloading/completed 状态。
func (m *CloudDownloadManager) findByURL(url string) *CloudTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tasks {
		if t.URL == url && (t.Status == "pending" || t.Status == "downloading" || t.Status == "completed") {
			return t
		}
	}
	return nil
}

// GetTask 按 ID 获取任务。
func (m *CloudDownloadManager) GetTask(id string) (*CloudTask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	return t, ok
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
			result = append(result, t)
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

	// 触发下载取消
	if cancel, ok := m.cancelFuncs[id]; ok {
		cancel()
		delete(m.cancelFuncs, id)
	}
	m.mu.Unlock()

	m.saveTask(t)
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
func (m *CloudDownloadManager) recoverTasks() {
	entries, err := os.ReadDir(m.persistDir)
	if err != nil {
		return
	}
	recovered := 0
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
	}
	if recovered > 0 {
		m.logger.Info("cloud download tasks recovered", "count", recovered)
	}
}

// cleanupExpired 定期清理过期任务。
func (m *CloudDownloadManager) cleanupExpired() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		m.mu.Lock()
		cleaned := 0
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
				delete(m.tasks, id)
				_ = os.Remove(filepath.Join(m.persistDir, id+".json"))
				_ = os.RemoveAll(filepath.Join(m.cloudDir, t.ID))
				if t.TotalSize > 0 {
					m.storage.Release(t.TotalSize, CategoryCloud)
				}
				if m.checksumStore != nil {
					remotePath := filepath.Join(cloudDirName, t.ID, t.Filename)
					m.checksumStore.Delete(remotePath)
				}
				cleaned++
			}
		}
		m.mu.Unlock()
		if cleaned > 0 {
			m.logger.Info("expired cloud download tasks cleaned up", "count", cleaned)
		}
	}
}

var taskIDCounter struct {
	mu sync.Mutex
	n  int64
}

func newTaskID() string {
	taskIDCounter.mu.Lock()
	taskIDCounter.n++
	n := taskIDCounter.n
	taskIDCounter.mu.Unlock()
	return fmt.Sprintf("cloud-%d-%d", time.Now().UnixNano(), n)
}
