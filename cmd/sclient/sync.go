// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdSync 创建 sync 命令（push/pull 子命令）。
// 在本地 sproxy 服务端创建同步任务，由 SyncManager 托管执行；--remote 是服务端
// sync_remotes 配置的远程节点名（HTTP 直连远程 sproxy 文件服务，非 mesh node）。
func NewCmdSync(factory clientfactory.Factory, ios cli.IOStreams, st *state.State, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync <push|pull>",
		Short: "节点间文件同步（push/pull）",
		Long: `在本地 sproxy 服务端创建同步任务，把文件/目录复制到远程节点（push）或从远程节点拉取（pull）。

同步任务由本地 sproxy 的 SyncManager 托管执行。--remote 是服务端 sync_remotes 配置的
远程节点名（HTTP 直连远程 sproxy 文件服务，非 mesh node）。--src/--dst 均为服务端 uploadsDir
相对路径（默认 "" = 整个根）。

创建后任务异步执行：加 --wait 等待完成并展示进度；--json 输出任务 JSON 供脚本消费。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newCmdSyncDirection(factory, ios, "push"))
	cmd.AddCommand(newCmdSyncDirection(factory, ios, "pull"))
	return cmd
}

// syncCmdOptions 是 sync push/pull 共用的 flag 集合。
type syncCmdOptions struct {
	remote         string
	src            string
	dst            string
	recursive      bool
	include        []string
	exclude        []string
	conflict       string
	syncEmptyDirs  bool
	followSymlinks bool
	wait           bool
	timeout        time.Duration
	pollInterval   time.Duration
}

// newCmdSyncDirection 创建 push 或 pull 子命令（direction 决定 SyncTaskRequest.Direction）。
func newCmdSyncDirection(factory clientfactory.Factory, ios cli.IOStreams, direction string) *cobra.Command {
	var o syncCmdOptions

	cmd := &cobra.Command{
		Use:   direction,
		Short: syncShort(direction),
		Long:  syncLong(direction),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 纯参数校验 fail fast（不依赖网络/配置），在 NewClient 之前（审查 M-2）
			conflict, err := normalizeConflictPolicy(o.conflict)
			if err != nil {
				return err
			}
			if o.remote == "" {
				return fmt.Errorf("--remote 必填（服务端 sync_remotes 配置的远程节点名）")
			}

			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			req := client.SyncTaskRequest{
				Direction:      direction,
				Remote:         o.remote,
				Src:            o.src,
				Dst:            o.dst,
				Recursive:      o.recursive,
				Include:        o.include,
				Exclude:        o.exclude,
				ConflictPolicy: conflict,
				SyncEmptyDirs:  o.syncEmptyDirs,
				FollowSymlinks: o.followSymlinks,
			}
			task, err := svc.CreateSyncTask(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("创建同步任务失败: %w", err)
			}

			// --json 是根命令持久化 flag（对齐 cloud-list/cloud-cancel 惯例），
			// 未定义本地 --json 避免遮蔽，使 `sclient --json sync push` 与
			// `sclient sync push --json` 行为一致。
			jsonOut, _ := cmd.Flags().GetBool("json")
			if o.wait {
				return waitSyncTask(cmd.Context(), ios, svc, task.ID, o.timeout, o.pollInterval, jsonOut)
			}
			return printSyncTaskResult(ios, task, jsonOut)
		},
	}

	cmd.Flags().StringVar(&o.remote, "remote", "", "远程节点名（服务端 sync_remotes 配置名，必填）")
	cmd.Flags().StringVar(&o.src, "src", "", "源路径（push=本地 uploadsDir 相对路径；pull=远程相对路径；默认 \"\" = 整个根）")
	cmd.Flags().StringVar(&o.dst, "dst", "", "目标路径（push=远程相对路径；pull=本地 uploadsDir 相对路径；默认 \"\" = 目标根）")
	cmd.Flags().BoolVar(&o.recursive, "recursive", false, "递归同步子目录")
	cmd.Flags().StringArrayVar(&o.include, "include", nil, "包含过滤器（glob 模式，可多次指定）")
	cmd.Flags().StringArrayVar(&o.exclude, "exclude", nil, "排除过滤器（glob 模式，可多次指定）")
	cmd.Flags().StringVar(&o.conflict, "conflict", "skip", "冲突处理策略（skip|overwrite|lww|conflict-rename）")
	cmd.Flags().BoolVar(&o.syncEmptyDirs, "sync-empty-dirs", false, "同步空目录（默认跳过）")
	cmd.Flags().BoolVar(&o.followSymlinks, "follow-symlinks", false, "跟随符号链接（默认跳过）")
	cmd.Flags().BoolVar(&o.wait, "wait", false, "等待任务完成并展示进度")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 5*time.Minute, "等待超时时间（--wait 时生效，0=不限时）")
	cmd.Flags().DurationVar(&o.pollInterval, "poll-interval", 2*time.Second, "轮询间隔")
	return cmd
}

// syncShort 返回 push/pull 子命令的简短描述。
func syncShort(direction string) string {
	switch direction {
	case "push":
		return "推送本地文件/目录到远程节点"
	case "pull":
		return "从远程节点拉取文件/目录到本地"
	default:
		return "同步文件"
	}
}

// syncLong 返回 push/pull 子命令的长描述。
func syncLong(direction string) string {
	switch direction {
	case "push":
		return `把本地 sproxy uploadsDir 中的文件/目录推送到远程节点。

在本地 sproxy 创建同步任务（方向 push），SyncManager 把 --src 指定路径复制到远程节点
uploadsDir 的 --dst 路径。--src/--dst 均为服务端相对路径，默认 "" 表示整个根。`
	case "pull":
		return `把远程节点 uploadsDir 中的文件/目录拉取到本地 sproxy。

在本地 sproxy 创建同步任务（方向 pull），SyncManager 把远程节点 --src 指定路径复制到
本地 uploadsDir 的 --dst 路径。--src/--dst 均为服务端相对路径，默认 "" 表示整个根。`
	default:
		return ""
	}
}

// normalizeConflictPolicy 把 CLI 的 --conflict 值映射为服务端 conflict_policy 枚举。
// CLI 用连字符（conflict-rename）对齐 flag 命名习惯；服务端用下划线（conflict_rename）。
func normalizeConflictPolicy(v string) (string, error) {
	switch v {
	case "", "skip":
		return "skip", nil
	case "overwrite":
		return "overwrite", nil
	case "lww":
		return "lww", nil
	case "conflict-rename", "conflict_rename":
		return "conflict_rename", nil
	default:
		return "", fmt.Errorf("无效的 --conflict 值 %q，仅支持 skip/overwrite/lww/conflict-rename", v)
	}
}

// printSyncTaskResult 打印同步任务创建/终态结果。
// jsonOut 输出全量 JSON，否则简洁行（id/status）。任务处于 failed/cancelled 时返回错误
// （命令非零退出，对齐云端下载 wait 的"cancelled 计入失败"语义）。
// 注意（审查 I-1）：状态错误先算好，--json 模式输出 JSON 后仍返回它——避免提前 return
// 跳过状态检查导致 failed/cancelled 退出码错误为 0。
func printSyncTaskResult(ios cli.IOStreams, task *client.SyncTask, jsonOut bool) error {
	var statusErr error
	switch task.Status {
	case client.SyncStatusFailed:
		msg := task.Error
		if msg == "" {
			msg = "未知错误"
		}
		statusErr = fmt.Errorf("同步任务 %s 失败: %s", task.ID, msg)
	case client.SyncStatusCancelled:
		statusErr = fmt.Errorf("同步任务 %s 已取消", task.ID)
	}
	if jsonOut {
		if err := printSyncTaskJSON(ios.Out, task); err != nil {
			return err
		}
		return statusErr
	}
	statusLine := fmt.Sprintf("同步任务 %s: %s", task.ID, task.Status)
	if task.Error != "" {
		statusLine += fmt.Sprintf(" - %s", task.Error)
	}
	ios.WriteOutLine(statusLine)
	return statusErr
}

// printSyncTaskJSON 把同步任务以缩进 JSON 输出到 w。
func printSyncTaskJSON(w io.Writer, task *client.SyncTask) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(task)
}

// waitSyncTask 轮询同步任务直到终态（completed/failed/cancelled），展示进度。
// timeout>0 时限制总等待时长；pollInterval 控制轮询间隔。
// jsonOut 时轮询期间不刷屏，仅终态输出一次 JSON。
func waitSyncTask(ctx context.Context, ios cli.IOStreams, svc *client.FileClient, id string, timeout, pollInterval time.Duration, jsonOut bool) error {
	// 审查 M-1：--poll-interval <= 0 会触发 time.NewTicker panic，守卫回落默认。
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	var pollCtx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		pollCtx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		pollCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// 初始查询：失败即报错，不伪造终态（对齐云端 wait：轮询/初始获取失败返回 error）
	task, err := svc.GetSyncTask(pollCtx, id)
	if err != nil {
		return fmt.Errorf("获取同步任务 %s 信息失败: %w", id, err)
	}
	if isSyncTerminal(task.Status) {
		return printSyncTaskResult(ios, task, jsonOut)
	}

	// --json 模式保持 stdout 纯净（脚本可整段 JSON 解析），不打印人工提示行
	if !jsonOut {
		ios.WriteOutLine("等待同步任务 %s 完成...", id)
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-pollCtx.Done():
			return pollCtx.Err()
		case <-ticker.C:
			task, err := svc.GetSyncTask(pollCtx, id)
			if err != nil {
				return fmt.Errorf("轮询同步任务 %s 状态失败: %w", id, err)
			}
			if isSyncTerminal(task.Status) {
				return printSyncTaskResult(ios, task, jsonOut)
			}
			if jsonOut {
				continue // --json 轮询时不刷屏，仅终态输出
			}
			pct := int64(0)
			if task.BytesTotal > 0 {
				pct = task.BytesDone * 100 / task.BytesTotal
			}
			ios.WriteOutLine("  ⟳ 同步中: %d%% (%d/%d bytes, %d/%d files)",
				pct, task.BytesDone, task.BytesTotal, task.FilesDone, task.FilesTotal)
		}
	}
}

// isSyncTerminal 报告任务状态是否为终态（completed/failed/cancelled）。
func isSyncTerminal(status string) bool {
	return status == client.SyncStatusCompleted || status == client.SyncStatusFailed || status == client.SyncStatusCancelled
}
