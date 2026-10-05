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

// TypeCloudDownload 是云端下载链式操作的类型标识。
const TypeCloudDownload = "cloud_download"

// Sentinel errors for CloudDownloadChain.
var (
	ErrClientNil        = errors.New("client is nil")
	ErrArchiveFailed    = errors.New("archive failed")
	ErrStorageFull      = errors.New("storage full")
	ErrLocalNotVerified = errors.New("local file not verified; skipping remote cleanup")
)

func init() {
	RegisterRunner(TypeCloudDownload, func() ChainRunner { return &CloudDownloadChain{} })
}

// CloudDownloadChain 云端下载链式操作，实现 ChainRunner 接口。
type CloudDownloadChain struct {
	ChainID       string                `json:"chain_id"`
	CurrentPhase  string                `json:"phase"`
	CurStatus     string                `json:"status"`
	URLs          []string              `json:"urls"`
	Entries       []cloudfilename.Entry `json:"entries,omitempty"` // URL→可选保存文件名；空则回退 URLs
	TaskIDs       []string              `json:"task_ids,omitempty"`
	ArchiveName   string                `json:"archive_name"`
	LocalDir      string                `json:"local_dir"`
	LocalPath     string                `json:"local_path,omitempty"`
	LocalVerified bool                  `json:"local_verified,omitempty"` // 本地文件校验通过；cleanupRemote 仅当其 true 才删云端
	KeepFiles     bool                  `json:"keep_files"`
	// Transfer 转存目标（链式透传 submit 请求；nil = 不转存）。
	Transfer *TransferSpec `json:"transfer,omitempty"`
	// Save 保留 cloud 桶副本（nil = 默认 true）。
	Save *bool `json:"save,omitempty"`
	// DownloadLocal 客户端是否下载本地（链式拉取 cloud 桶文件）。
	DownloadLocal bool `json:"download_local,omitempty"`
	// ForceIntegrity 强制源文件完整性（语义校验失败阻断；透传服务端）。
	ForceIntegrity bool      `json:"force_integrity,omitempty"`
	Completed      int       `json:"completed"`
	Failed         int       `json:"failed"`
	Total          int       `json:"total"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

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
		ChainID:        chainID,
		CurrentPhase:   "",
		CurStatus:      StatusRunning,
		URLs:           urls, // 兼容旧持久化状态；新状态以 Entries 为准
		Entries:        entries,
		ArchiveName:    archiveName,
		LocalDir:       localDir,
		KeepFiles:      opts.keepFiles,
		Transfer:       opts.transfer,
		Save:           opts.save,
		DownloadLocal:  opts.downloadLocal,
		ForceIntegrity: opts.forceIntegrity,
		Total:          len(entries),
		CreatedAt:      now,
		UpdatedAt:      now,
		PollInterval:   fixPollInterval(opts.pollInterval),
		Timeout:        opts.timeout,
		client:         client,
	}, nil
}

func (c *CloudDownloadChain) ID() string     { return c.ChainID }
func (c *CloudDownloadChain) Phase() string  { return c.CurrentPhase }
func (c *CloudDownloadChain) Status() string { return c.CurStatus }
func (c *CloudDownloadChain) State() map[string]any {
	return map[string]any{
		"type":            TypeCloudDownload,
		"chain_id":        c.ChainID,
		"phase":           c.CurrentPhase,
		"status":          c.CurStatus,
		"urls":            c.URLs,
		"entries":         c.Entries,
		"task_ids":        c.TaskIDs,
		"archive_name":    c.ArchiveName,
		"local_dir":       c.LocalDir,
		"local_path":      c.LocalPath,
		"local_verified":  c.LocalVerified,
		"keep_files":      c.KeepFiles,
		"completed":       c.Completed,
		"failed":          c.Failed,
		"total":           c.Total,
		"error":           c.Error,
		"created_at":      c.CreatedAt,
		"updated_at":      c.UpdatedAt,
		"poll_interval":   c.PollInterval,
		"timeout":         c.Timeout,
		"transfer":        c.Transfer,
		"save":            c.Save,
		"download_local":  c.DownloadLocal,
		"force_integrity": c.ForceIntegrity,
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
	// 转存/保留/下载本地参数同样桥接（C1：此前遗漏导致 CLI 旗标空转）。
	c.Transfer = opts.transfer
	c.Save = opts.save
	c.DownloadLocal = opts.downloadLocal
	c.ForceIntegrity = opts.forceIntegrity
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
			c.markFailed(err)
		}
	}()

	for {
		done, stageErr := c.runStage(ctx, reportFn)
		if stageErr != nil {
			return stageErr
		}
		if done {
			return nil
		}
	}
}

// beginPhase 把链推入下一阶段并做统一的状态推进/持久化/日志/进度上报。
func (c *CloudDownloadChain) beginPhase(ctx context.Context, reportFn ProgressFunc, phase, msg string, current, total int) {
	c.CurrentPhase = phase
	c.UpdatedAt = time.Now()
	c.saveState(ctx)
	slog.Debug("cloud download chain", "chain_id", c.ChainID, "phase", phase)
	reportFn(ctx, ProgressInfo{Phase: phase, Message: msg, Current: current, Total: total})
}

// markFailed 在失败时统一写入失败状态。
func (c *CloudDownloadChain) markFailed(err error) {
	c.CurStatus = StatusFailed
	c.CurrentPhase = PhaseFailed
	c.Error = err.Error()
	c.UpdatedAt = time.Now()
}

// markCompleted 在链走完时统一置为完成态。
func (c *CloudDownloadChain) markCompleted() {
	c.CurrentPhase = PhaseCompleted
	c.CurStatus = StatusCompleted
	c.UpdatedAt = time.Now()
}

// runStage 执行当前阶段的一个状态推进，返回 done=true 表示链已完成、只需返回。
func (c *CloudDownloadChain) runStage(ctx context.Context, reportFn ProgressFunc) (bool, error) {
	switch c.CurrentPhase {
	case "", PhaseSubmitting:
		// 在提交任务前先持久化状态，确保崩溃恢复后不会重复提交
		c.beginPhase(ctx, reportFn, PhaseSubmitting, "submit cloud download tasks", 0, len(c.URLs))
		if err := c.submitTasks(ctx); err != nil {
			// 部分提交失败时清理已成功提交的任务：它们已在服务端开始下载，若本链
			// 中止且用户不再重试，会成为孤儿持续占用服务端存储直到 TTL。
			// 清理失败不影响主错误返回（主错误已足够用户了解失败原因）。
			_ = c.cleanupRemote(context.WithoutCancel(ctx))
			return false, err
		}
		c.CurrentPhase = PhaseWaiting
		return false, nil

	case PhaseWaiting:
		c.beginPhase(ctx, reportFn, PhaseWaiting, "waiting for downloads to complete", c.Completed, c.Total)
		if err := c.waitForTasks(ctx); err != nil {
			return false, err
		}
		if !c.DownloadLocal {
			// 客户端不下载本地（只转存/只保留）：等待完成后直接完成，跳过
			// archive/download/cleaning（H1b：否则仍拉取本地，正交性破坏）。
			c.markCompleted()
			return true, nil
		}
		c.CurrentPhase = PhaseArchiving
		return false, nil

	case PhaseArchiving:
		c.beginPhase(ctx, reportFn, PhaseArchiving, "packaging archive", 0, 1)
		if err := c.archiveTasks(ctx); err != nil {
			return false, err
		}
		c.CurrentPhase = PhaseDownloading
		return false, nil

	case PhaseDownloading:
		c.beginPhase(ctx, reportFn, PhaseDownloading, "downloading to local", 0, 1)
		if err := c.downloadToLocal(ctx); err != nil {
			return false, err
		}
		if c.KeepFiles {
			// 保留远端文件，不再清理
			c.markCompleted()
			return true, nil
		}
		c.CurrentPhase = PhaseCleaning
		return false, nil

	case PhaseCleaning:
		// KeepFiles=true 时不会进入此分支（下载阶段已提前完成）
		c.beginPhase(ctx, reportFn, PhaseCleaning, "cleaning remote files", 0, len(c.TaskIDs)+1)
		if err := c.cleanupRemote(ctx); err != nil {
			// 清理失败：保留云端（尤其未校验时不删云端），显式报错，禁止静默跳过。
			return false, fmt.Errorf("清理云端文件失败，已保留云端: %w", err)
		}
		c.markCompleted()
		return true, nil

	default:
		return false, fmt.Errorf("unknown phase: %s", c.CurrentPhase)
	}
}

// cloudDownloadEntries 返回链式下载条目（Entries 优先，回退 URLs 生成）。
func cloudDownloadEntries(c *CloudDownloadChain) []cloudfilename.Entry {
	if len(c.Entries) > 0 {
		return c.Entries
	}
	var out []cloudfilename.Entry
	for _, u := range c.URLs {
		out = append(out, cloudfilename.Entry{URL: u})
	}
	return out
}

// cloudDownloadTransferOpts 组装转存/保留选项（transfer/save 透传服务端）。
func cloudDownloadTransferOpts(c *CloudDownloadChain) []CloudDownloadOption {
	var opts []CloudDownloadOption
	if c.Transfer != nil {
		opts = append(opts, WithCloudDownloadTransfer(c.Transfer))
	}
	if c.Save != nil {
		opts = append(opts, WithCloudDownloadSave(*c.Save))
	}
	if c.DownloadLocal {
		opts = append(opts, WithCloudDownloadLocal(true))
	}
	if c.ForceIntegrity {
		opts = append(opts, WithCloudDownloadForceIntegrity(true))
	}
	return opts
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
	entries := cloudDownloadEntries(c)
	dlOpts := cloudDownloadTransferOpts(c)
	tasks, err := c.client.CloudDownloadBatchEntries(ctx, entries, dlOpts...)
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
		storageFullURLs, storageFullIDs, cancelled := c.tallyResults(results)
		if len(storageFullURLs) == 0 {
			// 无存储超限重试：若仍有失败/取消任务（含重试提交失败的 URL），链式操作不得
			// 声称成功（禁止静默失败）。cancelled 计入失败（用户确认 cancelled=失败）。
			if waitErr := c.waitFailure(cancelled, submitFailedCount); waitErr != nil {
				return waitErr
			}
			return nil
		}
		if attempt >= maxAttempts-1 {
			// 最后一次尝试仍存储超限：计入失败，循环结束返回 ErrStorageFull。
			c.Failed += len(storageFullURLs)
			continue
		}
		if retryErr := c.retryStorageFull(ctx, storageFullURLs, storageFullIDs, attempt); retryErr != nil {
			return retryErr
		}
	}
	return fmt.Errorf("storage full after %d attempts: %w", maxAttempts, ErrStorageFull)
}

// tallyResults 统计一轮轮询结果：完成/失败计数写入 c.Completed/c.Failed，
// 存储超限任务单独归集（用于重试提交）；返回存储超限的 URL/ID 列表与取消计数。
func (c *CloudDownloadChain) tallyResults(results []*CloudTask) (storageFullURLs, storageFullIDs []string, cancelled int) {
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
	return storageFullURLs, storageFullIDs, cancelled
}

// waitFailure 根据失败/取消计数构造链式操作失败错误（无失败返回 nil）。
func (c *CloudDownloadChain) waitFailure(cancelled, submitFailedCount int) error {
	if c.Failed+cancelled+submitFailedCount > 0 {
		if cancelled > 0 {
			return fmt.Errorf("%d 个云端下载任务失败（其中 %d 个被取消，共 %d 个）",
				c.Failed+cancelled+submitFailedCount, cancelled, c.Total+submitFailedCount)
		}
		return fmt.Errorf("%d 个云端下载任务失败（共 %d 个）", c.Failed+submitFailedCount, c.Total+submitFailedCount)
	}
	return nil
}

// retryStorageFull 处理存储超限重试：移除失败任务 ID、指数退避等待、用独立超时的
// context 重试提交（保留原 URL 在 Entries 中指定的保存文件名）。调用方保证
// storageFullURLs 非空；失败返回 error（含「所有重试提交都再次失败」的显式错误）。
func (c *CloudDownloadChain) retryStorageFull(ctx context.Context, storageFullURLs, storageFullIDs []string, attempt int) error {
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
	if err := c.waitBackoff(ctx, attempt); err != nil {
		return err
	}
	// 使用独立超时的 context 重试，避免原始 context 过期导致重试失败。
	// 重试条目保留原 URL 在 Entries 中指定的保存文件名（若指定过）。
	tasks, err := c.resubmitStorageFull(ctx, storageFullURLs)
	if err != nil {
		return err
	}
	// 新提交的任务添加回 TaskIDs；无 ID = 提交再次失败（计入 failedCount 用于显式报错，
	// 而非静默丢弃——否则该 URL 从 TaskIDs 消失、下一轮统计全完成后链式操作会错误报告
	// 成功）。注意：失败任务数在 waitForTasks 每轮循环开头归零，且「有失败任务」的最终
	// 判定在下一轮轮询结果后作出——故本计数与 waitFailure 读取值一致（保持既有行为逐字
	// 不变）；禁止静默失败语义由 TaskIDs 清空 + 下方显式报错兜底。
	c.TaskIDs = remaining
	var failedCount int
	for _, t := range tasks {
		if t.ID != "" {
			c.TaskIDs = append(c.TaskIDs, t.ID)
			continue
		}
		failedCount++
	}
	if len(c.TaskIDs) == 0 && failedCount > 0 {
		// 所有重试提交都再次失败、没有可轮询的任务：直接报错，
		// 避免下一轮空轮询返回误导性的"没有可轮询的任务"
		return fmt.Errorf("%d 个云端下载任务重试提交失败（存储空间不足）", failedCount)
	}
	// 更新 Total 为本次重试后的 TaskIDs 总数
	c.Total = len(c.TaskIDs)
	return nil
}

// waitBackoff 指数退避等待（默认 10s, 20s, 40s；测试可注入 backoffFn 缩短；受 ctx
// 剩余时间约束）。返回 nil 表示等待完成；ctx 提前取消返回其错误。
func (c *CloudDownloadChain) waitBackoff(ctx context.Context, attempt int) error {
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
		return nil
	case <-ctx.Done():
		// timer 可能已触发，drain channel 防阻塞
		if !timer.Stop() {
			<-timer.C
		}
		return ctx.Err()
	}
}

// resubmitStorageFull 用独立超时的 context 重试提交存储超限 URL（重试条目保留原 URL 在
// Entries 中指定的保存文件名）。
func (c *CloudDownloadChain) resubmitStorageFull(ctx context.Context, storageFullURLs []string) ([]CloudTask, error) {
	retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	retryEntries := make([]cloudfilename.Entry, 0, len(storageFullURLs))
	for _, u := range storageFullURLs {
		retryEntries = append(retryEntries, c.entryForURL(u))
	}
	// M5：storage-full 重试透传三参（transfer/save/download_local）——与 submitTasks
	// 同一收口 cloudDownloadTransferOpts(c)，避免重试条目丢失转存/保存语义（此前
	// 仅 URL+filename 重提交 → 转存后任务被当成纯下载，save=false 语义不落地）。
	tasks, err := c.client.CloudDownloadBatchEntries(retryCtx, retryEntries, cloudDownloadTransferOpts(c)...)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("重试批量提交失败: %w", err)
	}
	return tasks, nil
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
			// 并发查询所有任务状态；未全部到达终态时继续下一轮（results==nil）。
			results, err := c.pollRound(timeoutCtx)
			if err != nil {
				return nil, err
			}
			if results != nil {
				return results, nil
			}
		}
	}
}

// pollResult 是一轮并发查询中某任务的查询结果（index 回溯到 TaskIDs 下标）。
type pollResult struct {
	index int
	task  *CloudTask
	err   error
}

// pollRound 并发查询所有任务状态（一轮）：任一查询失败立即返回错误并消费剩余
// 结果（防 goroutine 泄漏）；全部到达终态（完成/失败/取消）返回 results，否则
// 返回 (nil, nil) 让外层继续轮询。
func (c *CloudDownloadChain) pollRound(ctx context.Context) ([]*CloudTask, error) {
	resultCh := make(chan pollResult, len(c.TaskIDs))
	cancelCtx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	c.startPollQueries(cancelCtx, resultCh)
	return c.collectPollResults(cancelCtx, resultCh, cancelAll)
}

// startPollQueries 启动全部任务的并发查询 goroutine，全部查询退出后关闭 resultCh。
func (c *CloudDownloadChain) startPollQueries(cancelCtx context.Context, resultCh chan<- pollResult) {
	var wg sync.WaitGroup
	for i, taskID := range c.TaskIDs {
		wg.Go(func() {
			select {
			case <-cancelCtx.Done():
				return
			default:
			}
			status, err := c.client.GetCloudTask(cancelCtx, taskID)
			select {
			case resultCh <- pollResult{index: i, task: status, err: err}:
			case <-cancelCtx.Done():
			}
		})
	}
	go func() {
		wg.Wait()
		close(resultCh)
	}()
}

// collectPollResults 汇总一轮并发查询：任一失败立即取消并消费剩余结果（防泄漏）；
// 全部到达终态返回 results，否则返回 (nil, nil) 让外层继续轮询。
func (c *CloudDownloadChain) collectPollResults(cancelCtx context.Context, resultCh <-chan pollResult, cancelAll context.CancelFunc) ([]*CloudTask, error) {
	results := make([]*CloudTask, len(c.TaskIDs))
	allDone := true
	for r := range resultCh {
		if r.err != nil {
			cancelAll()
			// 消费剩余结果，避免 goroutine 泄漏（错误已定位，剩余值丢弃）。
			for range resultCh { // NOSONAR: S108 — 排空通道防止发送方 goroutine 泄漏，循环体有意为空
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
	return nil, nil
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

	// 归档下载按用途传 kind=cloud_archive：服务端在租户 archive 桶内按 owner 拼接
	// 归档目录，客户端不接触内部路径，filename 只传归档名。
	localPath := filepath.Join(c.LocalDir, archiveName)
	c.LocalPath = localPath
	if err := c.client.ChunkedDownload(ctx, archiveName, localPath, WithChunkedKind(DownloadKindCloudArchive)); err != nil {
		// 下载/校验失败：LocalVerified 保持 false，禁止后续误删云端。
		c.LocalVerified = false
		return fmt.Errorf("下载归档文件失败: %w", err)
	}
	// 下载成功且 ChunkedDownload 已完成 SHA-256 校验（verifyDownloadChecksum），
	// 本地文件已确认与远端一致——以此为 cleanupRemote 删除云端的前置条件。
	c.LocalVerified = true
	return nil
}

// cleanupRemote 清理远端任务及关联文件。
// 硬性语义：**仅当本地文件校验通过（LocalVerified=true）才删除云端任务**。
// LocalVerified 为 false（下载/校验未成功，或 resume 恢复时未置位）时跳过清理并返回
// ErrLocalNotVerified——确保不会在本地校验和未经确认的情况下误删云端文件。
func (c *CloudDownloadChain) cleanupRemote(ctx context.Context) error {
	if !c.LocalVerified {
		return fmt.Errorf("本地文件尚未校验通过，跳过云端清理: %w", ErrLocalNotVerified)
	}
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
