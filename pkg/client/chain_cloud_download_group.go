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
	Error        string                `json:"error,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`

	// 持久化字段
	PollInterval time.Duration `json:"poll_interval"`
	Timeout      time.Duration `json:"timeout"`

	// 非持久化字段
	archiveServerPath string        `json:"-"`
	client            *FileClient   `json:"-"`
	chainMgr          *ChainManager `json:"-"`
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
		client:       client,
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
	}
}

func (c *CloudDownloadGroupChain) Restore(state map[string]any) error {
	codec := StructCodec{}
	return codec.FromMap(state, c)
}

func (c *CloudDownloadGroupChain) SetClient(client *FileClient) {
	c.client = client
}

func (c *CloudDownloadGroupChain) SetOptions(opts chainOptions) {
	c.PollInterval = fixPollInterval(opts.pollInterval)
	c.Timeout = opts.timeout
	c.KeepFiles = opts.keepFiles
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

	switch c.CurrentPhase {
	case "":
		fallthrough
	case PhaseSubmitting:
		c.CurrentPhase = PhaseSubmitting
		c.UpdatedAt = time.Now()
		c.saveState(ctx)
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseSubmitting)
		reportFn(ctx, ProgressInfo{Phase: PhaseSubmitting, Message: "create cloud download group", Current: 0, Total: len(c.Entries)})
		if err := c.submitGroup(ctx); err != nil {
			return err
		}
		c.CurrentPhase = PhaseWaiting
		c.UpdatedAt = time.Now()
		c.saveState(ctx)
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseWaiting, "group_id", c.GroupID)
		reportFn(ctx, ProgressInfo{Phase: PhaseWaiting, Message: "waiting for group downloads to complete", Current: 0, Total: c.TotalTasks})
		fallthrough

	case PhaseWaiting:
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseWaiting)
		if err := c.waitForGroup(ctx); err != nil {
			return err
		}
		c.CurrentPhase = PhaseArchiving
		c.UpdatedAt = time.Now()
		c.saveState(ctx)
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseArchiving)
		reportFn(ctx, ProgressInfo{Phase: PhaseArchiving, Message: "packaging group archive", Current: 0, Total: 1})
		fallthrough

	case PhaseArchiving:
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseArchiving)
		if err := c.archiveGroup(ctx); err != nil {
			return err
		}
		c.CurrentPhase = PhaseDownloading
		c.UpdatedAt = time.Now()
		c.saveState(ctx)
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseDownloading)
		reportFn(ctx, ProgressInfo{Phase: PhaseDownloading, Message: "downloading to local", Current: 0, Total: 1})
		fallthrough

	case PhaseDownloading:
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseDownloading)
		if err := c.downloadToLocal(ctx); err != nil {
			return err
		}
		if c.KeepFiles {
			break
		}
		c.CurrentPhase = PhaseCleaning
		c.UpdatedAt = time.Now()
		c.saveState(ctx)
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseCleaning)
		reportFn(ctx, ProgressInfo{Phase: PhaseCleaning, Message: "cleaning remote group", Current: 0, Total: 1})
		fallthrough

	case PhaseCleaning:
		slog.Debug("cloud group chain", "chain_id", c.ChainID, "phase", PhaseCleaning)
		_ = c.cleanupGroup(ctx)

	default:
		return fmt.Errorf("unknown phase: %s", c.CurrentPhase)
	}

	c.CurrentPhase = PhaseCompleted
	c.CurStatus = StatusCompleted
	c.UpdatedAt = time.Now()
	return nil
}

// submitGroup 创建云端下载任务组并记录组 ID。
// 组内去重可能吸收既有任务，TotalTasks 以服务端返回为准。
func (c *CloudDownloadGroupChain) submitGroup(ctx context.Context) error {
	group, err := c.client.CloudCreateGroupEntries(ctx, c.GroupName, c.Entries)
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
// 时序差异说明（与 batch 链 pollAllTasks 的区别）：batch 链在发现 cancelled 任务时
// 立即返回错误并停止轮询；group 链这里继续轮询等待所有活跃任务进入终态后才整体失败。
// 这是有意的"组完整性"语义——组内仍有任务在下载时，不提前中断（中断也无法阻止
// 服务端已启动的下载）。两边的结果都是整体失败，仅失败时机不同。
func (c *CloudDownloadGroupChain) waitForGroup(ctx context.Context) error {
	if c.GroupID == "" {
		return fmt.Errorf("组 ID 为空，请先创建组")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	ticker := time.NewTicker(c.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return timeoutCtx.Err()
		case <-ticker.C:
			detail, err := c.client.CloudGetGroup(timeoutCtx, c.GroupID)
			if err != nil {
				return fmt.Errorf("查询组状态失败: %w", err)
			}
			if detail.Group == nil {
				return fmt.Errorf("下载组 %s 不存在", c.GroupID)
			}

			// 以子任务列表实际状态为准计数（而非仅依赖 group.TotalTasks），
			// 防御服务端在极早期轮询返回空 tasks 的边界（此时按 pending 处理）。
			completed, failed, cancelled, active := 0, 0, 0, 0
			for _, t := range detail.Tasks {
				switch t.Status {
				case TaskStatusCompleted:
					completed++
				case TaskStatusFailed:
					failed++
				case TaskStatusCancelled:
					cancelled++
				default:
					active++
				}
			}
			c.TotalTasks = detail.Group.TotalTasks
			c.Completed = completed
			c.Failed = failed
			c.Cancelled = cancelled

			// 任一子任务失败/取消 → 整体失败（与 batch 链 waitForTasks 语义一致，
			// 不打包下载缺文件的归档）
			if failed+cancelled > 0 {
				return fmt.Errorf("下载组 %s 有 %d 个任务失败/取消（%d/%d 完成），无法完成链式下载",
					c.GroupID, failed+cancelled, completed, c.TotalTasks)
			}
			// 防御：服务端在极早期返回空 tasks 但组状态非终态（如 downloading）时，
			// 空列表不应被误判为"全部完成"。只有组状态为 completed 或活跃计数为 0
			// 且已完成数 >= 组总任务数时才视为完成。
			if active == 0 {
				if detail.Group.Status == "completed" {
					return nil
				}
				// 空 tasks + 非 completed 组状态：继续轮询（避免误判完成）
				if len(detail.Tasks) == 0 {
					continue
				}
				// 有任务列表但都已完成（无活跃），组状态尚未刷新到 completed
				// （服务端 UpdateGroupStatus 是异步的）——再轮询一次等待组状态收敛。
				continue
			}
		}
	}
}

// archiveGroup 打包组内已完成文件。
func (c *CloudDownloadGroupChain) archiveGroup(ctx context.Context) error {
	result, err := c.client.CloudArchiveGroup(ctx, c.GroupID, c.ArchiveName)
	if err != nil {
		return fmt.Errorf("archive: %w: %v", ErrArchiveFailed, err)
	}
	c.archiveServerPath = result.File
	return nil
}

// downloadToLocal 分块下载归档文件到本地。
func (c *CloudDownloadGroupChain) downloadToLocal(ctx context.Context) error {
	archiveName := filepath.Base(c.ArchiveName)
	if !strings.HasSuffix(archiveName, ".tar.gz") {
		archiveName += ".tar.gz"
	}

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
