// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
)

// TypeCloudDownloadGroup 是云端组下载链式操作的类型标识。
const TypeCloudDownloadGroup = "cloud_download_group"

func init() {
	RegisterRunner(TypeCloudDownloadGroup, func() ChainRunner { return &CloudDownloadGroupChain{} })
}

// CloudDownloadGroupChain 云端组下载链式操作，实现 ChainRunner 接口。
// 与 CloudDownloadChain 的区别：
//   - 提交阶段调用 CloudCreateGroupEntries 创建组（而非 CloudDownloadBatchEntries）
//   - 等待阶段轮询组状态（CloudGetGroup 组详情，含子任务列表）
//   - 归档阶段调用 CloudArchiveGroup（而非 ArchiveCloudTasks）
//   - 清理阶段调用 CloudDeleteGroup（而非逐任务 DeleteCloudTask）
type CloudDownloadGroupChain struct {
	ChainID      string                `json:"chain_id"`
	CurrentPhase string                `json:"phase"`
	CurStatus    string                `json:"status"`
	GroupName    string                `json:"group_name"`
	GroupID      string                `json:"group_id,omitempty"` // 创建成功后设置
	Entries      []cloudfilename.Entry `json:"entries"`
	ArchiveName  string                `json:"archive_name"`
	LocalDir     string                `json:"local_dir"`
	LocalPath    string                `json:"local_path,omitempty"`
	KeepFiles    bool                  `json:"keep_files"`
	TotalTasks   int                   `json:"total_tasks"`
	Completed    int                   `json:"completed"`
	Failed       int                   `json:"failed"`
	Cancelled    int                   `json:"cancelled"`
	// Damaged 完整性损坏但放行完成的子任务数（R3-I3 组链延伸）。
	Damaged   int       `json:"damaged,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// M6：组下载四参（transfer/save/download_local/force_integrity）——与单条/batch 语义对齐。
	Transfer      *TransferSpec `json:"transfer,omitempty"`
	Save          *bool         `json:"save,omitempty"`
	DownloadLocal bool          `json:"download_local,omitempty"`
	// ForceIntegrity 强制源文件完整性（透传服务端；组内每个子任务）。
	ForceIntegrity bool `json:"force_integrity,omitempty"`

	// 持久化字段
	PollInterval time.Duration `json:"poll_interval"`
	Timeout      time.Duration `json:"timeout"`

	// 非持久化字段
	client   *FileClient   `json:"-"`
	chainMgr *ChainManager `json:"-"`
}

// NewCloudDownloadGroupChain 创建云端组下载链式操作。
func NewCloudDownloadGroupChain(client *FileClient, groupName string, entries []cloudfilename.Entry, archiveName, localDir string, opts chainOptions) (*CloudDownloadGroupChain, error) {
	if groupName == "" {
		return nil, fmt.Errorf("groupName 不能为空")
	}
	if archiveName == "" {
		return nil, fmt.Errorf("archiveName 不能为空")
	}
	if localDir == "" {
		return nil, fmt.Errorf("localDir 不能为空")
	}
	now := time.Now()
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("生成随机数失败: %w", err)
	}
	chainID := fmt.Sprintf("group-chain-%d-%x", now.UnixNano(), buf)

	return &CloudDownloadGroupChain{
		ChainID:      chainID,
		CurrentPhase: "",
		CurStatus:    StatusRunning,
		GroupName:    groupName,
		Entries:      entries,
		ArchiveName:  archiveName,
		LocalDir:     localDir,
		KeepFiles:    opts.keepFiles,
		TotalTasks:   len(entries),
		CreatedAt:    now,
		UpdatedAt:    now,
		PollInterval: fixPollInterval(opts.pollInterval),
		Timeout:      opts.timeout,
		// M6：三参从 opts 接入（与 CloudDownloadChain 同源函数式 API）。
		Transfer:       opts.transfer,
		Save:           opts.save,
		DownloadLocal:  opts.downloadLocal,
		ForceIntegrity: opts.forceIntegrity,
		client:         client,
	}, nil
}

func (c *CloudDownloadGroupChain) ID() string     { return c.ChainID }
func (c *CloudDownloadGroupChain) Phase() string  { return c.CurrentPhase }
func (c *CloudDownloadGroupChain) Status() string { return c.CurStatus }

func (c *CloudDownloadGroupChain) State() map[string]any {
	return map[string]any{
		"type":          TypeCloudDownloadGroup,
		"chain_id":      c.ChainID,
		"phase":         c.CurrentPhase,
		"status":        c.CurStatus,
		"group_name":    c.GroupName,
		"group_id":      c.GroupID,
		"entries":       c.Entries,
		"archive_name":  c.ArchiveName,
		"local_dir":     c.LocalDir,
		"local_path":    c.LocalPath,
		"keep_files":    c.KeepFiles,
		"total_tasks":   c.TotalTasks,
		"completed":     c.Completed,
		"failed":        c.Failed,
		"cancelled":     c.Cancelled,
		"error":         c.Error,
		"created_at":    c.CreatedAt,
		"updated_at":    c.UpdatedAt,
		"poll_interval": c.PollInterval,
		"timeout":       c.Timeout,
		// 四参（transfer/save/download_local/force_integrity）持久化：恢复/resume 后保持原语义
		// （F3：曾缺失导致 resume 重建为纯下载——组在服务端被重建成无 transfer/save）。
		"transfer":        c.Transfer,
		"save":            c.Save,
		"download_local":  c.DownloadLocal,
		"force_integrity": c.ForceIntegrity,
	}
}

func (c *CloudDownloadGroupChain) Restore(state map[string]any) error {
	codec := StructCodec{}
	if err := codec.FromMap(state, c); err != nil {
		return err
	}
	// G2：旧版持久化状态无 download_local 字段（三参是新增的）——恢复为 false 会被 F2
	// 守卫当「显式跳过」直接完成（静默丢本地下载）。历史语义是组链**总是**下载到本地，
	// 故 key 缺失时应视为 true（与旧状态升级兼容），只有新版显式持久化的 false 才跳过。
	if _, ok := state["download_local"]; !ok {
		c.DownloadLocal = true
	}
	return nil
}

func (c *CloudDownloadGroupChain) SetClient(client *FileClient) {
	c.client = client
}

func (c *CloudDownloadGroupChain) SetOptions(opts chainOptions) {
	c.PollInterval = fixPollInterval(opts.pollInterval)
	c.Timeout = opts.timeout
	c.KeepFiles = opts.keepFiles
	// 三参桥接（F3）：SetOptions 是 chainOptions → 持久化字段的唯一桥接层（与单链
	// chain_cloud_download.go 对齐），恢复后不再依赖 chainOptions。
	c.Transfer = opts.transfer
	c.Save = opts.save
	c.DownloadLocal = opts.downloadLocal
	c.ForceIntegrity = opts.forceIntegrity
}

func (c *CloudDownloadGroupChain) SetChainManager(mgr *ChainManager) {
	c.chainMgr = mgr
}

func (c *CloudDownloadGroupChain) saveState(ctx context.Context) {
	if c.chainMgr != nil {
		c.chainMgr.saveState(context.WithoutCancel(ctx), c)
	}
}

// Run 执行云端组下载链式操作，按阶段推进：
// submitting -> waiting -> archiving -> downloading -> [cleaning] -> completed。
func (c *CloudDownloadGroupChain) Run(ctx context.Context, reportFn ProgressFunc) (err error) {
	if c.client == nil {
		return fmt.Errorf("cloud group chain: %w", ErrClientNil)
	}

	defer func() {
		if err != nil {
			c.CurStatus = StatusFailed
			c.CurrentPhase = PhaseFailed
			c.Error = err.Error()
			c.UpdatedAt = time.Now()
		}
	}()

	for {
		done, stageErr := c.runGroupStage(ctx, reportFn)
		if stageErr != nil {
			return stageErr
		}
		if done {
			return nil
		}
	}
}

// runGroupStage 组链单阶段推进（switch 主体迁此控制 gocognit，与单链 runStage 同构）。
// 返回 done=true 表示链已完成（skipLocalDownload 直接完成 / 全部阶段走完）。
func (c *CloudDownloadGroupChain) runGroupStage(ctx context.Context, reportFn ProgressFunc) (bool, error) {
	switch c.CurrentPhase {
	case "", PhaseSubmitting:
		c.beginGroupPhase(ctx, reportFn, PhaseSubmitting, "create cloud download group", len(c.Entries))
		if err := c.submitGroup(ctx); err != nil {
			return false, err
		}
		c.beginGroupPhase(ctx, reportFn, PhaseWaiting, "waiting for group downloads to complete", c.TotalTasks)
		fallthrough

	case PhaseWaiting:
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseWaiting)
		if err := c.waitForGroup(ctx); err != nil {
			return false, err
		}
		// F2：download_local=false → 只转存/只保留，跳过 archive/download/cleaning。
		if c.skipLocalDownload(ctx) {
			return true, nil
		}
		c.beginGroupPhase(ctx, reportFn, PhaseArchiving, "packaging group archive", 1)
		fallthrough

	case PhaseArchiving:
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseArchiving)
		// 注意：此处不需要 skipLocalDownload 复查——DownloadLocal=false 的链在 Waiting
		// 阶段就已直接完成（永不进入 Archiving）；旧版持久化状态缺 download_local 键由
		// Restore 置为 true（历史语义总是下载本地，G2 规则），因此「false+Archiving」态
		// 不可达。若未来有人删除 Restore 的 G2 规则，须在此处补复查（防静默丢下载）。
		if err := c.archiveGroup(ctx); err != nil {
			return false, err
		}
		c.beginGroupPhase(ctx, reportFn, PhaseDownloading, "downloading to local", 1)
		fallthrough

	case PhaseDownloading:
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseDownloading)
		if err := c.downloadToLocal(ctx); err != nil {
			return false, err
		}
		if c.KeepFiles {
			break
		}
		c.beginGroupPhase(ctx, reportFn, PhaseCleaning, "cleaning remote group", 1)
		fallthrough

	case PhaseCleaning:
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseCleaning)
		if err := c.cleanupGroup(ctx); err != nil {
			// G1：清理失败显式报错（与单链一致，禁止静默吞错报「完成」）——
			// 否则云端组/桶残留却打印「组链式操作完成!」误导。
			return false, fmt.Errorf("清理云端组失败，已保留云端: %w", err)
		}

	default:
		return false, fmt.Errorf("unknown phase: %s", c.CurrentPhase)
	}

	c.CurrentPhase = PhaseCompleted
	c.CurStatus = StatusCompleted
	c.UpdatedAt = time.Now()
	return true, nil
}

// beginGroupPhase 阶段推进的统一收口（置阶段 + 时间戳 + saveState + 进度上报）。
// 抽离 Run 内重复块控制 gocognit（gocognit=15 门禁）。
func (c *CloudDownloadGroupChain) beginGroupPhase(ctx context.Context, reportFn ProgressFunc, phase, message string, total int) {
	c.CurrentPhase = phase
	c.UpdatedAt = time.Now()
	c.saveState(ctx)
	slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", phase)
	reportFn(ctx, ProgressInfo{Phase: phase, Message: message, Current: 0, Total: total})
}

// skipLocalDownload F2：download_local=false → 等待完成后直接完成（只转存/只保留），
// 跳过 archive/download/cleaning——与单链 H1b 语义对齐（正交性：不拉取本地）。
func (c *CloudDownloadGroupChain) skipLocalDownload(ctx context.Context) bool {
	if c.DownloadLocal {
		return false
	}
	c.CurrentPhase = PhaseCompleted
	c.CurStatus = StatusCompleted
	c.UpdatedAt = time.Now()
	c.saveState(ctx)
	return true
}

// submitGroup 创建云端下载任务组并记录组 ID。
// 组内去重可能吸收既有任务，TotalTasks 以服务端返回为准。
func (c *CloudDownloadGroupChain) submitGroup(ctx context.Context) error {
	// 幂等守卫：恢复时若 GroupID 已非空（submit 阶段完成、phase 尚未写入 waiting 前崩溃），
	// 跳过重复创建，避免同一组被再次创建导致双倍任务（C5）。
	if c.GroupID != "" {
		return nil
	}
	group, err := c.client.CloudCreateGroupEntries(ctx, c.GroupName, c.Entries, groupTransferOpts(c)...)
	if err != nil {
		return fmt.Errorf("创建下载组失败: %w", err)
	}
	c.GroupID = group.ID
	if group.TotalTasks > 0 {
		c.TotalTasks = group.TotalTasks
	}
	return nil
}

// waitForGroup 轮询组详情（含子任务列表）直到全部完成或有任务失败。
// 语义与 batch 链一致：任一子任务 failed/cancelled → 整体失败。
//
// 时序说明：batch 链与 group 链在 edef904 后语义统一——都不在发现 cancelled 时立即
// 失败，而是等所有活跃任务进入终态后整体报错（cancelled 计入失败，用户确认 cancelled=失败）。
// 组内仍有任务在下载时不提前中断（中断也无法阻止服务端已启动的下载）。
func (c *CloudDownloadGroupChain) waitForGroup(ctx context.Context) error {
	if c.GroupID == "" {
		return fmt.Errorf("组 ID 为空，请先创建组")
	}

	// c.Timeout>0 才设超时；0 表示不限时（与 batch 链 pollAllTasks 一致）
	timeoutCtx, cancel := groupTimeoutContext(ctx, c.Timeout)
	defer cancel()

	ticker := time.NewTicker(c.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return timeoutCtx.Err()
		case <-ticker.C:
			done, err := c.pollGroupRound(timeoutCtx)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// groupTimeoutContext 按 c.Timeout 生成带超时或纯取消的下文。
func groupTimeoutContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

// pollGroupRound 拉取一次组详情并更新状态。
// 返回 (true, nil) 表示组已成功完成；返回 (_, err) 表示终态异常；返回 (false, nil) 继续轮询。
func (c *CloudDownloadGroupChain) pollGroupRound(ctx context.Context) (bool, error) {
	detail, err := c.client.CloudGetGroup(ctx, c.GroupID)
	if err != nil {
		return false, fmt.Errorf("查询组状态失败: %w", err)
	}
	if detail.Group == nil {
		return false, fmt.Errorf("下载组 %s 不存在", c.GroupID)
	}
	// 以子任务列表实际状态为准计数（而非仅依赖 group.TotalTasks），
	// 防御服务端在极早期轮询返回空 tasks 的边界（此时按 pending 处理）。
	completed, failed, cancelled, active, damaged := countGroupTasks(detail.Tasks)
	c.TotalTasks = detail.Group.TotalTasks
	c.Completed = completed
	c.Failed = failed
	c.Cancelled = cancelled
	c.Damaged = damaged

	// 不提前中断：即使已有任务失败/取消，仍继续轮询等待所有活跃任务进入终态，
	// 与 batch 链 waitForTasks 语义一致（等全部终态后整体判定，不打包缺文件的归档）。
	// 只有活跃任务清零后才判定组终态。
	if active > 0 {
		return false, nil
	}
	return c.finishGroupRound(detail, completed, failed, cancelled, damaged)
}

// countGroupTasks 按任务状态统计 completed/failed/cancelled/active/damaged 计数。
// damaged（R3-I3 组链延伸）：completed 但 IntegrityStatus=="damaged" 的子任务——
// 默认放行语义，不应被当可靠成功（与 batch 链 Damaged 一致）。
func countGroupTasks(tasks []CloudTask) (completed, failed, cancelled, active, damaged int) {
	for _, t := range tasks {
		switch t.Status {
		case TaskStatusCompleted:
			completed++
			if t.IntegrityStatus == "damaged" {
				damaged++
			}
		case TaskStatusFailed:
			failed++
		case TaskStatusCancelled:
			cancelled++
		default:
			active++
		}
	}
	return completed, failed, cancelled, active, damaged
}

// finishGroupRound 在无活跃任务时判定组终态：成功返回 (true,nil)，异常返回错误，
// 其余（空 tasks 或未刷新到 completed）返回 (false,nil) 继续轮询。
func (c *CloudDownloadGroupChain) finishGroupRound(detail *CloudGroupDetail, completed, failed, cancelled, damaged int) (bool, error) {
	if detail.Group.Status == "completed" && failed+cancelled == 0 {
		// R3-I3 组链延伸：damaged 子任务明确提示（非失败，但不应被当可靠成功）。
		if damaged > 0 {
			return true, fmt.Errorf("下载组 %s 有 %d 个子任务完整性损坏（damaged，已放行）", c.GroupID, damaged)
		}
		return true, nil
	}
	// 组状态为 failed/cancelled 且无活跃任务 → 终态，视为异常报错，避免转圈到超时（C7）。
	if detail.Group.Status == "failed" || detail.Group.Status == "cancelled" || failed+cancelled > 0 {
		return false, fmt.Errorf("下载组 %s 有 %d 个任务失败/取消（%d/%d 完成），无法完成链式下载",
			c.GroupID, failed+cancelled, completed, c.TotalTasks)
	}
	// 空 tasks + 非 completed 组状态：继续轮询（避免误判完成）
	return false, nil
}

// archiveGroup 打包组内已完成文件。
func (c *CloudDownloadGroupChain) archiveGroup(ctx context.Context) error {
	_, err := c.client.CloudArchiveGroup(ctx, c.GroupID, c.ArchiveName)
	if err != nil {
		return fmt.Errorf("archive: %w: %v", ErrArchiveFailed, err)
	}
	return nil
}

// downloadToLocal 分块下载归档文件到本地。
func (c *CloudDownloadGroupChain) downloadToLocal(ctx context.Context) error {
	archiveName := filepath.Base(c.ArchiveName)
	if !strings.HasSuffix(archiveName, ".tar.gz") {
		archiveName += ".tar.gz"
	}

	// 归档下载按用途传 kind=cloud_archive：服务端在租户 archive 桶内按 owner 拼接，
	// 客户端不接触内部路径，filename 只传归档名。
	localPath := filepath.Join(c.LocalDir, archiveName)
	c.LocalPath = localPath
	if err := c.client.ChunkedDownload(ctx, archiveName, localPath, WithChunkedKind(DownloadKindCloudArchive)); err != nil {
		return fmt.Errorf("下载归档文件失败: %w", err)
	}
	return nil
}

// cleanupGroup 删除组及所有关联文件。
func (c *CloudDownloadGroupChain) cleanupGroup(ctx context.Context) error {
	if c.GroupID == "" {
		return nil
	}
	if err := c.client.CloudDeleteGroup(ctx, c.GroupID); err != nil {
		return fmt.Errorf("清理下载组失败: %w", err)
	}
	return nil
}

// groupTransferOpts 组装组下载四参透传选项（M6：与单条/batch 同语义收口）。
func groupTransferOpts(c *CloudDownloadGroupChain) []CloudDownloadOption {
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
