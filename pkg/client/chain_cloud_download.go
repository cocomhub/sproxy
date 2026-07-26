// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// cloudArchiveDirName 是服务端云任务归档文件存储子目录，与服务端 cloudArchiveDirName 保持一致。
const cloudArchiveDirName = ".__cloud_archives__"

func init() {
	RegisterRunner("cloud_download", func() ChainRunner { return &CloudDownloadChain{} })
}

// CloudDownloadChain 云端下载链式操作，实现 ChainRunner 接口。
type CloudDownloadChain struct {
	ChainID      string    `json:"chain_id"`
	CurrentPhase string    `json:"phase"`
	CurStatus    string    `json:"status"`
	URLs         []string  `json:"urls"`
	TaskIDs      []string  `json:"task_ids,omitempty"`
	ArchiveName  string    `json:"archive_name"`
	LocalDir     string    `json:"local_dir"`
	LocalPath    string    `json:"local_path,omitempty"`
	KeepFiles    bool      `json:"keep_files"`
	Completed    int       `json:"completed"`
	Failed       int       `json:"failed"`
	Total        int       `json:"total"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	client *FileClient  `json:"-"`
	opts   chainOptions `json:"-"`
}

// NewCloudDownloadChain 创建云端下载链式操作。
func NewCloudDownloadChain(client *FileClient, urls []string, archiveName, localDir string, opts chainOptions) *CloudDownloadChain {
	now := time.Now()
	return &CloudDownloadChain{
		ChainID:      fmt.Sprintf("chain-%d", now.UnixNano()),
		CurrentPhase: "",
		CurStatus:    StatusRunning,
		URLs:         urls,
		ArchiveName:  archiveName,
		LocalDir:     localDir,
		KeepFiles:    opts.keepFiles,
		Total:        len(urls),
		CreatedAt:    now,
		UpdatedAt:    now,
		client:       client,
		opts:         opts,
	}
}

func (c *CloudDownloadChain) ID() string     { return c.ChainID }
func (c *CloudDownloadChain) Phase() string  { return c.CurrentPhase }
func (c *CloudDownloadChain) Status() string { return c.CurStatus }
func (c *CloudDownloadChain) State() map[string]any {
	return map[string]any{
		"type":         "cloud_download",
		"chain_id":     c.ChainID,
		"phase":        c.CurrentPhase,
		"status":       c.CurStatus,
		"urls":         c.URLs,
		"task_ids":     c.TaskIDs,
		"archive_name": c.ArchiveName,
		"local_dir":    c.LocalDir,
		"local_path":   c.LocalPath,
		"keep_files":   c.KeepFiles,
		"completed":    c.Completed,
		"failed":       c.Failed,
		"total":        c.Total,
		"error":        c.Error,
		"created_at":   c.CreatedAt,
		"updated_at":   c.UpdatedAt,
	}
}

func (c *CloudDownloadChain) Restore(state map[string]any) error {
	codec := StructCodec{}
	return codec.FromMap(state, c)
}

func (c *CloudDownloadChain) setClient(client *FileClient) {
	c.client = client
}

func (c *CloudDownloadChain) setOptions(opts chainOptions) {
	c.opts = opts
}

// Run 执行云端下载链式操作，按阶段推进：
// submitting → waiting → archiving → downloading → [cleaning] → completed。
func (c *CloudDownloadChain) Run(ctx context.Context, reportFn func(ctx context.Context, phase string, msg string, current, total int)) error {
	if c.client == nil {
		return fmt.Errorf("cloud download chain: client is nil, use setClient() before Run()")
	}

	// 统一错误处理：任何阶段失败都设置状态
	var runErr error
	defer func() {
		if runErr != nil {
			c.CurStatus = StatusFailed
			c.CurrentPhase = PhaseFailed
			c.Error = runErr.Error()
			c.UpdatedAt = time.Now()
		}
	}()

	switch c.CurrentPhase {
	case "":
		fallthrough
	case PhaseSubmitting:
		reportFn(ctx, PhaseSubmitting, "提交云端下载任务", 0, len(c.URLs))
		if err := c.submitTasks(ctx); err != nil {
			runErr = err
			return err
		}
		c.CurrentPhase = PhaseWaiting
		c.UpdatedAt = time.Now()
		reportFn(ctx, PhaseWaiting, "等待下载完成", c.Completed, c.Total)
		fallthrough

	case PhaseWaiting:
		if err := c.waitForTasks(ctx); err != nil {
			runErr = err
			return err
		}
		c.CurrentPhase = PhaseArchiving
		c.UpdatedAt = time.Now()
		reportFn(ctx, PhaseArchiving, "打包归档", 0, 1)
		fallthrough

	case PhaseArchiving:
		if err := c.archiveTasks(ctx); err != nil {
			runErr = err
			return err
		}
		c.CurrentPhase = PhaseDownloading
		c.UpdatedAt = time.Now()
		reportFn(ctx, PhaseDownloading, "下载到本地", 0, 1)
		fallthrough

	case PhaseDownloading:
		if err := c.downloadToLocal(ctx); err != nil {
			runErr = err
			return err
		}
		// 默认清理远端文件，keepFiles 时跳过
		if c.KeepFiles {
			break
		}
		c.CurrentPhase = PhaseCleaning
		c.UpdatedAt = time.Now()
		reportFn(ctx, PhaseCleaning, "清理远端文件", 0, len(c.TaskIDs)+1)
		fallthrough

	case PhaseCleaning:
		if c.KeepFiles {
			break
		}
		_ = c.cleanupRemote(ctx) // 清理失败不影响主流程成功
	}

	c.CurrentPhase = PhaseCompleted
	c.CurStatus = StatusCompleted
	c.UpdatedAt = time.Now()
	return nil
}

// submitTasks 批量提交云端下载任务。
func (c *CloudDownloadChain) submitTasks(ctx context.Context) error {
	tasks, err := c.client.CloudDownloadBatch(ctx, c.URLs)
	if err != nil {
		return fmt.Errorf("批量提交云端下载失败: %w", err)
	}
	for _, t := range tasks {
		c.TaskIDs = append(c.TaskIDs, t.ID)
	}
	c.Total = len(c.TaskIDs)
	return nil
}

// waitForTasks 轮询等待所有任务完成，支持存储超限重试。
func (c *CloudDownloadChain) waitForTasks(ctx context.Context) error {
	maxRetries := 3
	for attempt := 0; attempt <= maxRetries; attempt++ {
		results, err := c.pollAllTasks(ctx)
		if err != nil {
			return err
		}
		c.Completed = 0
		c.Failed = 0
		var storageFullURLs []string
		for _, r := range results {
			switch r.Status {
			case "completed":
				c.Completed++
			case "failed":
				if isStorageFullError(r.Error) {
					storageFullURLs = append(storageFullURLs, r.URL)
				} else {
					c.Failed++
				}
			}
		}
		if len(storageFullURLs) == 0 {
			return nil
		}
		if attempt < maxRetries {
			timer := time.NewTimer(30 * time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			timer.Stop()
			for _, url := range storageFullURLs {
				task, err := c.client.CloudDownload(ctx, url)
				if err != nil {
					return fmt.Errorf("重试提交失败: %w", err)
				}
				c.TaskIDs = append(c.TaskIDs, task.ID)
			}
		} else {
			c.Failed += len(storageFullURLs)
		}
	}
	return fmt.Errorf("存储空间不足，已重试 %d 次", maxRetries)
}

// pollAllTasks 轮询所有任务状态直到全部完成。
func (c *CloudDownloadChain) pollAllTasks(ctx context.Context) ([]*CloudTask, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, c.opts.timeout)
	defer cancel()

	for {
		select {
		case <-timeoutCtx.Done():
			return nil, timeoutCtx.Err()
		case <-time.After(c.opts.pollInterval):
			allDone := true
			var results []*CloudTask
			for _, taskID := range c.TaskIDs {
				status, err := c.client.GetCloudTask(timeoutCtx, taskID)
				if err != nil {
					return nil, fmt.Errorf("查询任务 %s 失败: %w", taskID, err)
				}
				results = append(results, status)
				if status.Status != "completed" && status.Status != "failed" && status.Status != "cancelled" {
					allDone = false
				}
			}
			if allDone {
				return results, nil
			}
		}
	}
}

// archiveTasks 打包归档所有已下载的文件。
func (c *CloudDownloadChain) archiveTasks(ctx context.Context) error {
	result, err := c.client.ArchiveCloudTasks(ctx, c.TaskIDs, c.ArchiveName)
	if err != nil {
		return fmt.Errorf("打包归档失败: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("打包归档失败: %s", result.Message)
	}
	return nil
}

// downloadToLocal 分块下载归档文件到本地。
func (c *CloudDownloadChain) downloadToLocal(ctx context.Context) error {
	archivePath := filepath.ToSlash(filepath.Join(cloudArchiveDirName, c.ArchiveName))
	if !strings.HasSuffix(c.ArchiveName, ".tar.gz") {
		archivePath += ".tar.gz"
	}
	localPath := filepath.Join(c.LocalDir, c.ArchiveName)
	if !strings.HasSuffix(localPath, ".tar.gz") {
		localPath += ".tar.gz"
	}
	c.LocalPath = localPath
	if err := c.client.ChunkedDownload(ctx, archivePath, localPath); err != nil {
		return fmt.Errorf("下载归档文件失败: %w", err)
	}
	return nil
}

// cleanupRemote 清理远端任务及关联文件。清理失败时继续处理剩余任务。
func (c *CloudDownloadChain) cleanupRemote(ctx context.Context) error {
	var firstErr error
	for _, taskID := range c.TaskIDs {
		if err := c.client.DeleteCloudTask(ctx, taskID); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("清理云端任务 %s 失败: %w", taskID, err)
			}
		}
	}
	return firstErr
}

// isStorageFullError 判断错误消息是否为存储空间不足（大小写不敏感子串匹配）。
func isStorageFullError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "storage full") ||
		strings.Contains(lower, "insufficient storage") ||
		strings.Contains(lower, "507") ||
		strings.Contains(lower, "disk quota") ||
		strings.Contains(lower, "no space left")
}
