// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
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
	}

	// 恢复持久化的任务
	mgr.recoverTasks()

	// 启动过期任务清理
	go mgr.cleanupExpired()

	return mgr
}

// CreateTask 创建云端下载任务。
func (m *CloudDownloadManager) CreateTask(method, url, filename string, totalSize int64) (*CloudTask, error) {
	// 预留存储空间
	if totalSize <= 0 {
		totalSize = 1024 * 1024 // 默认预留 1 MiB
	}
	if err := m.storage.TryReserve(totalSize, CategoryCloud); err != nil {
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
	return task, nil
}

// GetTask 按 ID 获取任务。
func (m *CloudDownloadManager) GetTask(id string) (*CloudTask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	return t, ok
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
	defer m.mu.Unlock()

	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task not found: %s", id)
	}
	if t.Status != "pending" && t.Status != "downloading" {
		return fmt.Errorf("cannot cancel task in status %q", t.Status)
	}
	t.Status = "cancelled"
	t.UpdatedAt = time.Now()
	t.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)
	m.saveTask(t)
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
	delete(m.tasks, id)
	m.mu.Unlock()

	// 删除云端文件
	filePath := filepath.Join(m.cloudDir, t.ID, t.Filename)
	_ = os.Remove(filePath)
	_ = os.Remove(filepath.Join(m.cloudDir, t.ID)) // 清理任务目录

	// 删除持久化文件
	_ = os.Remove(filepath.Join(m.persistDir, t.ID+".json"))

	// 释放存储空间
	if t.TotalSize > 0 {
		m.storage.Release(t.TotalSize, CategoryCloud)
	}

	// 清理 checksum
	if m.checksumStore != nil {
		remotePath := filepath.Join(cloudDirName, t.ID, t.Filename)
		m.checksumStore.Delete(remotePath)
	}

	return nil
}

// saveTask 持久化单个任务到磁盘。
func (m *CloudDownloadManager) saveTask(t *CloudTask) {
	data, err := json.Marshal(t)
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
	}
}

// cleanupExpired 定期清理过期任务。
func (m *CloudDownloadManager) cleanupExpired() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		m.mu.Lock()
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
			}
		}
		m.mu.Unlock()
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
