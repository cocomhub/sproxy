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

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
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
	ChainID      string                `json:"chain_id"`
	CurrentPhase string                `json:"phase"`
	CurStatus    string                `json:"status"`
	URLs         []string              `json:"urls"`
	Entries      []cloudfilename.Entry `json:"entries,omitempty"` // URL→可选保存文件名；空则回退 URLs
	TaskIDs      []string              `json:"task_ids,omitempty"`
	ArchiveName  string                `json:"archive_name"`
	LocalDir     string                `json:"local_dir"`
	LocalPath    string                `json:"local_path,omitempty"`
	KeepFiles    bool                  `json:"keep_files"`
	Completed    int                   `json:"completed"`
	Failed       int                   `json:"failed"`
	Total        int                   `json:"total"`
	Error        string                `json:"error,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`

	// 持久化字段：恢复时自动恢复；同时是唯一数据源（SetOptions 从 chainOptions 桥接至此）
	PollInterval time.Duration `json:"poll_interval"` // 轮询间隔，恢复时保持
	Timeout      time.Duration `json:"timeout"`       // 超时时间，恢复时保持

	// 非持久化字段：恢复后需手动设置
	client   *FileClient   `json:"-"`
	chainMgr *ChainManager `json:"-"` // 链式操作管理器，用于阶段间持久化状态

	// backoffFn 存储超限重试的退避间隔（attempt 从 0 起）。nil 时用默认 10s*(1<<attempt)。
	// 测试注入小退避避免慢 CI（如 10ms）。
	backoffFn func(attempt int) time.Duration
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
	// Entries：显式指定（WithChainEntries / --url-file）优先；否则由 urls 构造
	// （filename 为空，提交时由服务端按 URL 自动生成）。submitTasks 统一走 Entries。
	entries := opts.entries
	if len(entries) == 0 {
		for _, u := range urls {
			entries = append(entries, cloudfilename.Entry{URL: u})
		}
	}
	return &CloudDownloadChain{
		ChainID:      chainID,
		CurrentPhase: "",
		CurStatus:    StatusRunning,
		URLs:         urls, // 兼容旧持久化状态；新状态以 Entries 为准
		Entries:      entries,
		ArchiveName:  archiveName,
		LocalDir:     localDir,
		KeepFiles:    opts.keepFiles,
		Total:        len(entries),
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
		"entries":       c.Entries,
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
			// 部分提交失败时清理已成功提交的任务：它们已在服务端开始下载，若本链
			// 中止且用户不再重试，会成为孤儿持续占用服务端存储直到 TTL。
			// 清理失败不影响主错误返回（主错误已足够用户了解失败原因）。
			_ = c.cleanupRemote(context.WithoutCancel(ctx))
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
// 任何条目提交失败（返回空 ID + error）都立即报错，不静默丢弃后继续——否则链式
// 下载会"完成"但缺少这些文件，用户毫不知情（禁止静默失败）。
func (c *CloudDownloadChain) submitTasks(ctx context.Context) error {
	// 幂等守卫：恢复时若 TaskIDs 已非空（submit 阶段完成、phase 尚未写入 waiting 前崩溃），
	// 跳过重复提交，避免同一批 URL 被再次提交导致双倍任务/轮询（C5）。
	if len(c.TaskIDs) > 0 {
		return nil
	}
	// 统一走带保存文件名的 Entries；为防御直接构造/旧持久化状态（Entries 为空），
	// 回退为从 URLs 生成条目（filename 为空，服务端自动生成）。
	entries := c.Entries
	if len(entries) == 0 {
		for _, u := range c.URLs {
			entries = append(entries, cloudfilename.Entry{URL: u})
		}
	}
	tasks, err := c.client.CloudDownloadBatchEntries(ctx, entries)
	if err != nil {
		return fmt.Errorf("批量提交云端下载失败: %w", err)
	}
	var submitFailed []string
	taskIDSeen := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		if t.ID != "" {
			if !taskIDSeen[t.ID] {
				c.TaskIDs = append(c.TaskIDs, t.ID)
				taskIDSeen[t.ID] = true
			}
			continue
		}
		if t.Error != "" {
			submitFailed = append(submitFailed, fmt.Sprintf("%s: %s", t.URL, t.Error))
		} else {
			submitFailed = append(submitFailed, t.URL)
		}
	}
	if len(submitFailed) > 0 {
		return fmt.Errorf("%d 个云端下载任务提交失败：%s", len(submitFailed), strings.Join(submitFailed, "; "))
	}
	c.Total = len(c.TaskIDs)
	return nil
}

// entryForURL 返回 URL 对应的条目（保留其保存文件名）；Entries 中未找到时返回
// 仅含 URL 的条目（filename 为空，服务端自动生成）。
// 用于存储超限重试提交：服务端返回的任务 URL 可能是规范化后的，与原始 Entries
// 不完全一致，匹配失败时按 URL 重新提交即可。
func (c *CloudDownloadChain) entryForURL(url string) cloudfilename.Entry {
	for _, e := range c.Entries {
		if e.URL == url {
			return e
		}
	}
	return cloudfilename.Entry{URL: url}
}

// waitForTasks 轮询等待所有任务完成，支持存储超限重试。
func (c *CloudDownloadChain) waitForTasks(ctx context.Context) error {
	maxAttempts := 3
	// 重试提交再次失败（无 ID）的 URL 数。它在循环内被归零的 c.Failed 之外单独
	// 累积，保证任何一次重试提交失败都被计入最终结果，不被静默丢弃（禁止静默失败）。
	var submitFailedCount int
	for attempt := range maxAttempts {
		// 每次重试前归零计数器，基于本次轮询结果重新统计
		c.Completed = 0
		c.Failed = 0
		submitFailedCount = 0

		results, err := c.pollAllTasks(ctx)
		if err != nil {
			return err
		}
		var storageFullURLs []string
		var storageFullIDs []string
		cancelled := 0
		for _, r := range results {
			switch r.Status {
			case TaskStatusCompleted:
				c.Completed++
			case TaskStatusCancelled:
				cancelled++
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
			// 无存储超限重试：若仍有失败/取消任务（含重试提交失败的 URL），链式操作不得
			// 声称成功（禁止静默失败）。cancelled 计入失败（用户确认 cancelled=失败）。
			if c.Failed+cancelled+submitFailedCount > 0 {
				if cancelled > 0 {
					return fmt.Errorf("%d 个云端下载任务失败（其中 %d 个被取消，共 %d 个）",
						c.Failed+cancelled+submitFailedCount, cancelled, c.Total+submitFailedCount)
				}
				return fmt.Errorf("%d 个云端下载任务失败（共 %d 个）", c.Failed+submitFailedCount, c.Total+submitFailedCount)
			}
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

			// 指数退避等待：默认 10s, 20s, 40s；测试可注入 backoffFn 缩短
			delay := 10 * time.Second * (1 << attempt)
			if c.backoffFn != nil {
				delay = c.backoffFn(attempt)
			}
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
				// timer 可能已触发，drain channel 防阻塞
				if !timer.Stop() {
					<-timer.C
				}
				return ctx.Err()
			}
			if len(storageFullURLs) > 0 {
				// 使用独立超时的 context 重试，避免原始 context 过期导致重试失败。
				// 重试条目保留原 URL 在 Entries 中指定的保存文件名（若指定过）。
				retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				retryEntries := make([]cloudfilename.Entry, 0, len(storageFullURLs))
				for _, u := range storageFullURLs {
					retryEntries = append(retryEntries, c.entryForURL(u))
				}
				tasks, err := c.client.CloudDownloadBatchEntries(retryCtx, retryEntries)
				cancel()
				if err != nil {
					return fmt.Errorf("重试批量提交失败: %w", err)
				}
				// 新提交的任务添加回 TaskIDs；无 ID = 提交再次失败，计入 submitFailedCount
				// 而非静默丢弃（否则该 URL 从 TaskIDs 消失、下一轮统计全完成后链式操作
				// 会错误报告成功）
				c.TaskIDs = remaining
				for _, t := range tasks {
					if t.ID != "" {
						c.TaskIDs = append(c.TaskIDs, t.ID)
						continue
					}
					submitFailedCount++
				}
				if len(c.TaskIDs) == 0 && submitFailedCount > 0 {
					// 所有重试提交都再次失败、没有可轮询的任务：直接报错，
					// 避免下一轮空轮询返回误导性的"没有可轮询的任务"
					return fmt.Errorf("%d 个云端下载任务重试提交失败（存储空间不足）", submitFailedCount)
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
	// c.Timeout>0 才设超时；0 表示不限时（与 waitForTasks 的 ctx 约束保持一致）
	timeoutCtx := ctx
	var cancel context.CancelFunc
	if c.Timeout > 0 {
		timeoutCtx, cancel = context.WithTimeout(ctx, c.Timeout)
	} else {
		timeoutCtx, cancel = context.WithCancel(ctx)
	}
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
				case TaskStatusCompleted, TaskStatusFailed, TaskStatusCancelled:
					// 已完成/失败/取消均为终态：继续等待其他任务，不立即中止。
					// 取消不再立即失败整链——与组链 waitForGroup 的"等所有任务终态后
					// 整体报错"语义对齐（用户确认 cancelled=失败，但失败时机延后到终态收敛）。
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
	_, err := c.client.ArchiveCloudTasks(ctx, c.TaskIDs, c.ArchiveName)
	if err != nil {
		return fmt.Errorf("archive: %w: %v", ErrArchiveFailed, err)
	}
	// 服务端返回的 File 只含归档名（客户端不接触 .__ 内部路径），下载阶段直接用
	// 客户端自身构造的归档名（与服务端后缀规范化一致），无需保存服务端路径。
	return nil
}

// downloadToLocal 分块下载归档文件到本地。
func (c *CloudDownloadChain) downloadToLocal(ctx context.Context) error {
	// 路径穿越防护：使用 filepath.Base 确保 ArchiveName 不含路径分隔符
	archiveName := filepath.Base(c.ArchiveName)
	if !strings.HasSuffix(archiveName, ".tar.gz") {
		archiveName += ".tar.gz"
	}

	// 归档下载按用途传 kind=cloud_archive：服务端在 .__cloud_archives__/<owner>/ 下
	// 按 owner 拼接归档目录，客户端不接触内部路径，filename 只传归档名。
	localPath := filepath.Join(c.LocalDir, archiveName)
	c.LocalPath = localPath
	if err := c.client.ChunkedDownload(ctx, archiveName, localPath, WithChunkedKind(DownloadKindCloudArchive)); err != nil {
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

// isStorageFullError 判断任务错误消息是否为存储空间不足（大小写不敏感子串匹配）。
//
// 注意：创建阶段的存储满已由 doJSON 的 HTTP 507 映射为 ErrStorageFull（errors.Is 精确
// 判断，见 client.go doJSON）。本函数是轮询到的失败任务（r.Error 为任务状态字符串而非
// error 对象）的兜底判断，两者覆盖不同数据路径，均保留。
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
