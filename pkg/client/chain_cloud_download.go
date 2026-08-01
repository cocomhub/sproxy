// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// cloudArchiveDirName 是服务端云任务归档文件存储子目录，与服务端 cloudArchiveDirName 保持一致。
const cloudArchiveDirName = ".__cloud_archives__"

// TypeCloudDownload 是云端下载链式操作的类型标识。
const TypeCloudDownload = "cloud_download"

// Sentinel errors for CloudDownloadChain.
var (
	ErrClientNil     = errors.New("client is nil")
	ErrArchiveFailed = errors.New("archive failed")
	ErrStorageFull   = errors.New("storage full")
)

func init() {
	RegisterRunner(TypeCloudDownload, func() ChainRunner { return &CloudDownloadChain{} })
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

	// 持久化字段：恢复时自动恢复；同时是唯一数据源（SetOptions 从 chainOptions 桥接至此）
	PollInterval time.Duration `json:"poll_interval"` // 轮询间隔，恢复时保持
	Timeout      time.Duration `json:"timeout"`       // 超时时间，恢复时保持

	// 非持久化字段：恢复后需手动设置
	archiveServerPath string        `json:"-"` // 服务端返回的归档文件路径
	client            *FileClient   `json:"-"`
	chainMgr          *ChainManager `json:"-"` // 链式操作管理器，用于阶段间持久化状态
}

// NewCloudDownloadChain 创建云端下载链式操作。
func NewCloudDownloadChain(client *FileClient, urls []string, archiveName, localDir string, opts chainOptions) (*CloudDownloadChain, error) {
	if archiveName == "" {
		return nil, fmt.Errorf("archiveName 不能为空")
	}
	if localDir == "" {
		return nil, fmt.Errorf("localDir 不能为空")
	}
	now := time.Now()
	// 用纳秒 + 随机后缀避免同一纳秒内的冲突
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("生成随机数失败: %w", err)
	}
	chainID := fmt.Sprintf("chain-%d-%x", now.UnixNano(), buf)
	return &CloudDownloadChain{
		ChainID:      chainID,
		CurrentPhase: "",
		CurStatus:    StatusRunning,
		URLs:         urls,
		ArchiveName:  archiveName,
		LocalDir:     localDir,
		KeepFiles:    opts.keepFiles,
		Total:        len(urls),
		CreatedAt:    now,
		UpdatedAt:    now,
		PollInterval: fixPollInterval(opts.pollInterval),
		Timeout:      opts.timeout,
		client:       client,
	}, nil
}

func (c *CloudDownloadChain) ID() string     { return c.ChainID }
func (c *CloudDownloadChain) Phase() string  { return c.CurrentPhase }
func (c *CloudDownloadChain) Status() string { return c.CurStatus }
func (c *CloudDownloadChain) State() map[string]any {
	return map[string]any{
		"type":          TypeCloudDownload,
		"chain_id":      c.ChainID,
		"phase":         c.CurrentPhase,
		"status":        c.CurStatus,
		"urls":          c.URLs,
		"task_ids":      c.TaskIDs,
		"archive_name":  c.ArchiveName,
		"local_dir":     c.LocalDir,
		"local_path":    c.LocalPath,
		"keep_files":    c.KeepFiles,
		"completed":     c.Completed,
		"failed":        c.Failed,
		"total":         c.Total,
		"error":         c.Error,
		"created_at":    c.CreatedAt,
		"updated_at":    c.UpdatedAt,
		"poll_interval": c.PollInterval,
		"timeout":       c.Timeout,
	}
}

func (c *CloudDownloadChain) Restore(state map[string]any) error {
	codec := StructCodec{}
	return codec.FromMap(state, c)
}

func (c *CloudDownloadChain) SetClient(client *FileClient) {
	c.client = client
}

func (c *CloudDownloadChain) SetOptions(opts chainOptions) {
	// SetOptions 从 chainOptions 中读取 pollInterval/timeout/keepFiles，
	// 写入 CloudDownloadChain 的持久化字段（KeepFiles/PollInterval/Timeout），
	// 使 struct 字段成为唯一数据源，避免 chainOptions 与持久化字段的重复问题。
	//
	// chainOptions 保留 pollInterval/timeout/keepFiles 字段作为 WithChain* 函数式 API
	// 的桥接层。SetOptions 读取一次后即写入持久化字段，之后不再依赖 chainOptions。
	// 这使得外部调用方（sclient CLI、测试等）通过 WithChain* 设置的选项能正确生效，
	// 同时确保持久化/恢复时 CloudDownloadChain 的 struct 字段是唯一数据源。
	c.PollInterval = fixPollInterval(opts.pollInterval)
	c.Timeout = opts.timeout
	c.KeepFiles = opts.keepFiles
}

// fixPollInterval 确保轮询间隔不为零，零值时使用默认值（5s）。
func fixPollInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return 5 * time.Second
	}
	return d
}

// SetChainManager 设置链式操作管理器引用，用于阶段间持久化状态。
func (c *CloudDownloadChain) SetChainManager(mgr *ChainManager) {
	c.chainMgr = mgr
}

// saveState 通过 chainMgr 持久化当前状态到 KVStore。
// 使用 WithoutCancel 包装上下文，确保状态在上下文取消后仍可持久化。
func (c *CloudDownloadChain) saveState(ctx context.Context) {
	if c.chainMgr != nil {
		c.chainMgr.saveState(context.WithoutCancel(ctx), c)
	}
}

// Run 执行云端下载链式操作，按阶段推进：
// submitting -> waiting -> archiving -> downloading -> [cleaning] -> completed。
func (c *CloudDownloadChain) Run(ctx context.Context, reportFn ProgressFunc) (err error) {
	if c.client == nil {
		return fmt.Errorf("cloud download chain: %w", ErrClientNil)
	}

	// 统一错误处理：任何阶段失败都设置状态
	defer func() {
		if err != nil {
			c.CurStatus = StatusFailed
			c.CurrentPhase = PhaseFailed
			c.Error = err.Error()
			c.UpdatedAt = time.Now()
		}
	}()

	switch c.CurrentPhase {
	case "":
		fallthrough
	case PhaseSubmitting:
		// 在提交任务前先持久化状态，确保崩溃恢复后不会重复提交
		c.CurrentPhase = PhaseSubmitting
		c.UpdatedAt = time.Now()
		c.saveState(ctx)
		slog.Debug("cloud download chain", "chain_id", c.ChainID, "phase", PhaseSubmitting)
		reportFn(ctx, ProgressInfo{Phase: PhaseSubmitting, Message: "submit cloud download tasks", Current: 0, Total: len(c.URLs)})
		if err := c.submitTasks(ctx); err != nil {
			return err
		}
		c.CurrentPhase = PhaseWaiting
		c.UpdatedAt = time.Now()
		c.saveState(ctx)
		slog.Debug("cloud download chain", "chain_id", c.ChainID, "phase", PhaseWaiting, "completed", c.Completed, "total", c.Total)
		reportFn(ctx, ProgressInfo{Phase: PhaseWaiting, Message: "waiting for downloads to complete", Current: c.Completed, Total: c.Total})
		fallthrough

	case PhaseWaiting:
		slog.Debug("cloud download chain", "chain_id", c.ChainID, "phase", PhaseWaiting)
		if err := c.waitForTasks(ctx); err != nil {
			return err
		}
		c.CurrentPhase = PhaseArchiving
		c.UpdatedAt = time.Now()
		c.saveState(ctx)
		slog.Debug("cloud download chain", "chain_id", c.ChainID, "phase", PhaseArchiving)
		reportFn(ctx, ProgressInfo{Phase: PhaseArchiving, Message: "packaging archive", Current: 0, Total: 1})
		fallthrough

	case PhaseArchiving:
		slog.Debug("cloud download chain", "chain_id", c.ChainID, "phase", PhaseArchiving)
		if err := c.archiveTasks(ctx); err != nil {
			return err
		}
		c.CurrentPhase = PhaseDownloading
		c.UpdatedAt = time.Now()
		c.saveState(ctx)
		slog.Debug("cloud download chain", "chain_id", c.ChainID, "phase", PhaseDownloading)
		reportFn(ctx, ProgressInfo{Phase: PhaseDownloading, Message: "downloading to local", Current: 0, Total: 1})
		fallthrough

	case PhaseDownloading:
		slog.Debug("cloud download chain", "chain_id", c.ChainID, "phase", PhaseDownloading)
		if err := c.downloadToLocal(ctx); err != nil {
			return err
		}
		// 默认清理远端文件，keepFiles 时跳过
		if c.KeepFiles {
			break
		}
		c.CurrentPhase = PhaseCleaning
		c.UpdatedAt = time.Now()
		c.saveState(ctx)
		slog.Debug("cloud download chain", "chain_id", c.ChainID, "phase", PhaseCleaning)
		reportFn(ctx, ProgressInfo{Phase: PhaseCleaning, Message: "cleaning remote files", Current: 0, Total: len(c.TaskIDs) + 1})
		fallthrough

	case PhaseCleaning:
		// KeepFiles=true 时不会进入此分支（下载阶段已 break）
		slog.Debug("cloud download chain", "chain_id", c.ChainID, "phase", PhaseCleaning)
		_ = c.cleanupRemote(ctx) // 清理失败不影响主流程成功

	default:
		return fmt.Errorf("unknown phase: %s", c.CurrentPhase)
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
		if t.ID != "" {
			c.TaskIDs = append(c.TaskIDs, t.ID)
		}
	}
	c.Total = len(c.TaskIDs)
	return nil
}

// waitForTasks 轮询等待所有任务完成，支持存储超限重试。
func (c *CloudDownloadChain) waitForTasks(ctx context.Context) error {
	maxAttempts := 3
	for attempt := range maxAttempts {
		// 每次重试前归零计数器，基于本次轮询结果重新统计
		c.Completed = 0
		c.Failed = 0

		results, err := c.pollAllTasks(ctx)
		if err != nil {
			return err
		}
		var storageFullURLs []string
		var storageFullIDs []string
		for _, r := range results {
			switch r.Status {
			case TaskStatusCompleted:
				c.Completed++
			case TaskStatusFailed:
				if isStorageFullError(r.Error) {
					storageFullURLs = append(storageFullURLs, r.URL)
					storageFullIDs = append(storageFullIDs, r.ID)
				} else {
					c.Failed++
				}
			}
		}
		if len(storageFullURLs) == 0 {
			return nil
		}
		if attempt < maxAttempts-1 {
			// 移除旧失败任务 ID，后续追加新提交的 ID
			failedSet := make(map[string]struct{}, len(storageFullIDs))
			for _, id := range storageFullIDs {
				failedSet[id] = struct{}{}
			}
			var remaining []string
			for _, id := range c.TaskIDs {
				if _, ok := failedSet[id]; !ok {
					remaining = append(remaining, id)
				}
			}
			c.TaskIDs = remaining

			// 指数退避等待：10s, 20s, 40s
			baseDelay := 10 * time.Second
			delay := baseDelay * (1 << attempt)
			// 检查上下文剩余时间，避免超时
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline)
				if remaining < delay {
					delay = remaining
				}
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			timer.Stop()
			if len(storageFullURLs) > 0 {
				// 使用独立超时的 context 重试，避免原始 context 过期导致重试失败
				retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				tasks, err := c.client.CloudDownloadBatch(retryCtx, storageFullURLs)
				cancel()
				if err != nil {
					return fmt.Errorf("重试批量提交失败: %w", err)
				}
				// 新提交的任务添加回 TaskIDs
				c.TaskIDs = remaining
				for _, t := range tasks {
					if t.ID != "" {
						c.TaskIDs = append(c.TaskIDs, t.ID)
					}
				}
			}
			// 更新 Total 为本次重试后的 TaskIDs 总数
			c.Total = len(c.TaskIDs)
		} else {
			c.Failed += len(storageFullURLs)
		}
	}
	return fmt.Errorf("storage full after %d attempts: %w", maxAttempts, ErrStorageFull)
}

// pollAllTasks 轮询所有任务状态直到全部完成。
// 使用并发查询减少多任务时的总等待时间。
func (c *CloudDownloadChain) pollAllTasks(ctx context.Context) ([]*CloudTask, error) {
	if len(c.TaskIDs) == 0 {
		return nil, fmt.Errorf("没有可轮询的任务")
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	ticker := time.NewTicker(c.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return nil, timeoutCtx.Err()
		case <-ticker.C:
			// 并发查询所有任务状态
			type taskResult struct {
				index int
				task  *CloudTask
				err   error
			}
			resultCh := make(chan taskResult, len(c.TaskIDs))
			var wg sync.WaitGroup
			cancelCtx, cancelAll := context.WithCancel(timeoutCtx)
			defer cancelAll()

			for i, taskID := range c.TaskIDs {
				wg.Go(func() {
					select {
					case <-cancelCtx.Done():
						return
					default:
					}
					status, err := c.client.GetCloudTask(cancelCtx, taskID)
					select {
					case resultCh <- taskResult{index: i, task: status, err: err}:
					case <-cancelCtx.Done():
					}
				})
			}
			go func() {
				wg.Wait()
				close(resultCh)
			}()

			results := make([]*CloudTask, len(c.TaskIDs))
			allDone := true
			for r := range resultCh {
				if r.err != nil {
					cancelAll()
					// 消费剩余结果，避免 goroutine 泄漏
					for range resultCh {
					}
					return nil, fmt.Errorf("查询任务 %s 失败: %w", c.TaskIDs[r.index], r.err)
				}
				results[r.index] = r.task
				switch r.task.Status {
				case TaskStatusCancelled:
					return nil, fmt.Errorf("cloud download task %s was cancelled", r.task.ID)
				case TaskStatusCompleted, TaskStatusFailed:
					// 已完成或失败，继续等待其他任务
				default:
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
		return fmt.Errorf("archive: %w: %v", ErrArchiveFailed, err)
	}
	// 保存服务端返回的归档文件路径，供 downloadToLocal 使用
	c.archiveServerPath = result.File
	return nil
}

// downloadToLocal 分块下载归档文件到本地。
func (c *CloudDownloadChain) downloadToLocal(ctx context.Context) error {
	// 路径穿越防护：使用 filepath.Base 确保 ArchiveName 不含路径分隔符
	archiveName := filepath.Base(c.ArchiveName)
	if !strings.HasSuffix(archiveName, ".tar.gz") {
		archiveName += ".tar.gz"
	}

	// 优先使用服务端返回的路径，兜底使用本地构造（保持与服务端一致的后缀逻辑）
	archivePath := c.archiveServerPath
	if archivePath == "" {
		archivePath = filepath.ToSlash(filepath.Join(cloudArchiveDirName, archiveName))
	}

	localPath := filepath.Join(c.LocalDir, archiveName)
	c.LocalPath = localPath
	if err := c.client.ChunkedDownload(ctx, archivePath, localPath); err != nil {
		return fmt.Errorf("下载归档文件失败: %w", err)
	}
	return nil
}

// cleanupRemote 清理远端任务及关联文件。清理失败时继续处理剩余任务。
func (c *CloudDownloadChain) cleanupRemote(ctx context.Context) error {
	var errs []error
	for _, taskID := range c.TaskIDs {
		if err := c.client.DeleteCloudTask(ctx, taskID); err != nil {
			errs = append(errs, fmt.Errorf("清理云端任务 %s 失败: %w", taskID, err))
		}
	}
	return errors.Join(errs...)
}

// isStorageFullError 判断错误消息是否为存储空间不足（大小写不敏感子串匹配）。
//
// 此函数作为后备方案，通过错误消息文本匹配判断存储超限。
// 未来应使用 HTTP 507 (Insufficient Storage) 状态码进行精确判断。
//
// 注意：此方法依赖服务端错误消息字符串，不同版本的服务端可能返回不同格式的
// 错误消息。建议未来使用结构化错误码（如 HTTP 507 状态码或 JSON 错误体中的
// error_code 字段）替代文本匹配，以提高健壮性和可维护性。
func isStorageFullError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "storage full") ||
		strings.Contains(lower, "insufficient storage") ||
		strings.Contains(lower, "disk quota") ||
		strings.Contains(lower, "no space left") ||
		strings.Contains(lower, "disk full") ||
		strings.Contains(lower, "out of disk space") ||
		(strings.Contains(lower, "quota") && strings.Contains(lower, "exceeded")) ||
		strings.Contains(lower, "存储空间") ||
		strings.Contains(lower, "存储已满") ||
		strings.Contains(lower, "超出配额") ||
		strings.Contains(lower, "磁盘空间")
}
