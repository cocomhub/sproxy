// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/spf13/cobra"
)

// NewCmdCloudDownloadGroup 创建云端组下载命令的工厂函数。
// 默认行为是完整链式操作：创建组 → 等待 → 打包 → 下载 → 清理。
func NewCmdCloudDownloadGroup(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud-download-group <name> <url> [url...]",
		Short: "从云端下载文件到组（链式操作：创建组→等待→打包→下载→清理）",
		Long: `通过 sproxy 服务端从外部 URL 下载文件到一组，自动执行完整链式操作：

  1. 创建云端下载任务组
  2. 等待组内全部下载完成
  3. 打包归档为 tar.gz
  4. 下载到本地
  5. 清理远端组

使用 --keep-files 跳过清理步骤。
使用子命令（submit, wait, archive, download, download-archive, list, cancel, delete, resume-chain, resume-download）执行单个步骤。`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			name := args[0]
			archiveName, _ := cmd.Flags().GetString("archive-name")
			if archiveName == "" {
				archiveName = fmt.Sprintf("cloud-download-group-%d.tar.gz", time.Now().Unix())
			}
			outputDir, _ := cmd.Flags().GetString("output-dir")
			keepFiles, _ := cmd.Flags().GetBool("keep-files")
			pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			urlFile, _ := cmd.Flags().GetString("url-file")

			if len(args) < 2 && urlFile == "" {
				return fmt.Errorf("请提供组名和至少一个 URL，或使用 --url-file 指定 URL 文件")
			}

			entries, collectErr := collectCloudEntries(args[1:], urlFile)
			if collectErr != nil {
				return collectErr
			}
			if preflightErr := preflightGroupEntries(ios, name, entries); preflightErr != nil {
				return preflightErr
			}

			ios.WriteOutLine("链式下载组 %q (%d 个条目)...", name, len(entries))
			opts := []client.ChainOption{
				client.WithChainPollInterval(pollInterval),
				client.WithChainTimeout(timeout),
			}
			if keepFiles {
				opts = append(opts, client.WithChainKeepFiles())
			}

			chainCtx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				chainCtx, cancel = context.WithTimeout(cmd.Context(), timeout)
				defer cancel()
			}
			result, err := svc.CloudDownloadGroupChain(chainCtx, name, entries, archiveName, outputDir, opts...)
			if err != nil {
				return fmt.Errorf("链式下载失败: %w", err)
			}

			ios.WriteOutLine("链式下载完成!")
			ios.WriteOutLine("  本地路径: %s", result.LocalPath())
			if !result.KeepFiles() {
				ios.WriteOutLine("  远端文件: 已清理")
			}
			return nil
		},
	}

	// 注册 flags
	cmd.Flags().String("archive-name", "", "归档文件名（默认自动生成）")
	cmd.Flags().String("output-dir", ".", "本地输出目录")
	cmd.Flags().Bool("keep-files", false, "下载到本地后不删除云端副本")
	cmd.Flags().Duration("poll-interval", 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "链式操作超时时间")
	cmd.Flags().String("url-file", "", "从文件读取 URL 条目（每行 URL 或 URL<TAB>FILENAME，FILENAME 为可选保存文件名）")

	// 注册子命令
	cmd.AddCommand(NewCmdCloudGroupSubmit(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupWait(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupArchive(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupDownload(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupDownloadArchive(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupList(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupCancel(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupResume(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupResumeChain(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupDelete(factory, ios, cfgSvc))

	return cmd
}

// preflightGroupEntries 客户端预校验：组内保存文件名必须唯一（与服务端 CreateGroup 规则一致）。
// 冲突在发送前拦截，同时展示每个 URL 的最终保存文件名。
func preflightGroupEntries(ios cli.IOStreams, name string, entries []cloudfilename.Entry) error {
	ios.WriteOutLine("创建下载组 %q (%d 个条目):", name, len(entries))
	filenameSeen := make(map[string]string, len(entries))
	var conflicts []string
	for _, e := range entries {
		fn, resolveErr := cloudfilename.ResolveFilename(e)
		if resolveErr != nil {
			return fmt.Errorf("条目 %s 文件名无效: %w", e.URL, resolveErr)
		}
		if prev, ok := filenameSeen[fn]; ok {
			conflicts = append(conflicts, fmt.Sprintf("文件名 %q (URL: %s 与 %s)", fn, prev, e.URL))
		} else {
			filenameSeen[fn] = e.URL
		}
		ios.WriteOutLine("  %s -> %s", e.URL, fn)
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("组内条目冲突，无法创建：%s；请在 --url-file 中为冲突条目指定不同的保存文件名（URL<TAB>FILENAME）",
			strings.Join(conflicts, ", "))
	}
	return nil
}

// NewCmdCloudGroupSubmit 创建 submit 子命令，仅创建下载组（不等待完成）。
func NewCmdCloudGroupSubmit(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "submit <name> <url> [url...]",
		Short: "创建云端下载任务组",
		Long:  `创建一组云端下载任务，文件下载到同一目录，支持组级打包。不等待任务完成。`,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			name := args[0]
			urlFile, _ := cmd.Flags().GetString("url-file")
			entries, collectErr := collectCloudEntries(args[1:], urlFile)
			if collectErr != nil {
				return collectErr
			}
			if preflightErr := preflightGroupEntries(ios, name, entries); preflightErr != nil {
				return preflightErr
			}

			group, err := svc.CloudCreateGroupEntries(cmd.Context(), name, entries)
			if err != nil {
				return fmt.Errorf("创建下载组失败: %w", err)
			}
			ios.WriteOutLine("  组 ID: %s", group.ID)
			ios.WriteOutLine("  状态: %s", group.Status)
			ios.WriteOutLine("  任务数: %d", group.TotalTasks)
			return nil
		},
	}
	cmd.Flags().String("url-file", "", "从文件读取 URL 条目（每行 URL 或 URL<TAB>FILENAME，FILENAME 为可选保存文件名）")
	return cmd
}

// NewCmdCloudGroupWait 创建 wait 子命令，等待组内全部任务完成。
func NewCmdCloudGroupWait(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wait <group-id>",
		Short: "等待云端下载任务组完成",
		Long:  `轮询等待指定下载组全部完成，显示组整体进度。`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			groupID := args[0]
			pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
			timeout, _ := cmd.Flags().GetDuration("timeout")

			// 初始查询确认组存在
			detail, err := svc.CloudGetGroup(cmd.Context(), groupID)
			if err != nil {
				return fmt.Errorf("获取下载组 %s 信息失败: %w", groupID, err)
			}
			if detail.Group == nil {
				return fmt.Errorf("下载组 %s 不存在", groupID)
			}

			ios.WriteOutLine("等待下载组 %s (%q) 完成...", groupID, detail.Group.Name)

			// timeout>0 才设超时；timeout=0 表示不限时（与链式入口一致）
			var pollCtx context.Context
			var cancel context.CancelFunc
			if timeout > 0 {
				pollCtx, cancel = context.WithTimeout(cmd.Context(), timeout)
			} else {
				pollCtx, cancel = context.WithCancel(cmd.Context())
			}
			defer cancel()
			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()

			for {
				select {
				case <-pollCtx.Done():
					return pollCtx.Err()
				case <-ticker.C:
					detail, err := svc.CloudGetGroup(pollCtx, groupID)
					if err != nil {
						return fmt.Errorf("轮询下载组状态失败: %w", err)
					}
					if detail.Group == nil {
						return fmt.Errorf("下载组 %s 不存在", groupID)
					}
					completed, failed, cancelled, active := 0, 0, 0, 0
					for _, t := range detail.Tasks {
						switch t.Status {
						case client.TaskStatusCompleted:
							completed++
						case client.TaskStatusFailed:
							failed++
						case client.TaskStatusCancelled:
							cancelled++
						default:
							active++
						}
					}
					ios.WriteOutLine("  %s: %s (%d/%d 完成, %d 失败, %d 取消, %d 进行中)",
						groupID, detail.Group.Status, completed, detail.Group.TotalTasks, failed, cancelled, active)

					if failed > 0 || cancelled > 0 {
						return fmt.Errorf("下载组 %s 有 %d 个失败, %d 个取消，无法完成", groupID, failed, cancelled)
					}
					if active == 0 {
						// 组状态为 failed/cancelled 且无活跃任务（空列表边界）→ 终态，视为异常报错，
						// 避免转圈到超时（C7）。
						if detail.Group.Status == "failed" || detail.Group.Status == "cancelled" {
							return fmt.Errorf("下载组 %s 已终止（状态 %s），无法完成", groupID, detail.Group.Status)
						}
						// 防御：服务端在极早期可能返回空 tasks，但组状态尚未到 completed。
						// 只有组状态为 completed（或 tasks 非空且无活跃）才视为完成。
						if detail.Group.Status == "completed" || len(detail.Tasks) > 0 {
							return nil
						}
						continue
					}
				}
			}
		},
	}
	cmd.Flags().Duration("poll-interval", 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "等待超时时间")
	return cmd
}

// NewCmdCloudGroupList 创建 list 子命令。
func NewCmdCloudGroupList(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出所有下载组",
		Long:  `列出所有云端下载任务组及其状态。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			status, _ := cmd.Flags().GetString("status")
			offset, _ := cmd.Flags().GetInt("offset")
			limit, _ := cmd.Flags().GetInt("limit")
			groups, total, err := svc.CloudListGroupsWithTotal(cmd.Context(), status, offset, limit)
			if err != nil {
				return fmt.Errorf("列举下载组失败: %w", err)
			}
			// 分页时展示总数（total=0 时不显示，避免每次都在空列表旁打印 0）
			if total > 0 && (offset > 0 || limit > 0) {
				ios.WriteOutLine("下载组总数: %d", total)
			}
			if len(groups) == 0 {
				ios.WriteOutLine("暂无下载组")
				return nil
			}
			for _, g := range groups {
				ios.WriteOutLine("  %s: %s (%s) %d/%d 完成, %d 失败, %d 取消", g.ID, g.Name, g.Status, g.Completed, g.TotalTasks, g.Failed, g.Cancelled)
			}
			return nil
		},
	}
	cmd.Flags().String("status", "", "按状态过滤 (pending|downloading|completed|failed|cancelled)")
	cmd.Flags().Int("offset", -1, "跳过前 N 条（默认 -1 不偏移）")
	cmd.Flags().Int("limit", 0, "返回条数上限（默认 0 返回全部）")
	return cmd
}

// NewCmdCloudGroupArchive 创建 archive 子命令。
func NewCmdCloudGroupArchive(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive <group-id> [archive-name]",
		Short: "打包下载组文件为 tar.gz",
		Long:  `将下载组内所有已完成的文件打包为单个 tar.gz 归档文件。`,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			groupID := args[0]
			archiveName := ""
			if len(args) > 1 {
				archiveName = args[1]
			}
			result, err := svc.CloudArchiveGroup(cmd.Context(), groupID, archiveName)
			if err != nil {
				return fmt.Errorf("打包下载组失败: %w", err)
			}
			if result.SkippedCount > 0 {
				ios.WriteOutLine("打包完成: %s (%d bytes, %d 个文件, 跳过 %d 个未完成任务)",
					result.File, result.Size, result.TaskCount, result.SkippedCount)
			} else {
				ios.WriteOutLine("打包完成: %s (%d bytes)", result.File, result.Size)
			}
			return nil
		},
	}
	return cmd
}

// NewCmdCloudGroupCancel 创建 cancel 子命令。
func NewCmdCloudGroupCancel(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <group-id>",
		Short: "取消下载组内所有任务",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if err := svc.CloudCancelGroup(cmd.Context(), args[0]); err != nil {
				// 与任务 cancel 一致：组不存在（404）视为已取消，幂等返回
				if errors.Is(err, client.ErrNotFound) {
					ios.WriteOutLine("下载组 %s 不存在（视为已取消）", args[0])
					return nil
				}
				return fmt.Errorf("取消下载组失败: %w", err)
			}
			ios.WriteOutLine("下载组已取消")
			return nil
		},
	}
	return cmd
}

// NewCmdCloudGroupResume 创建 resume-download 子命令。
func NewCmdCloudGroupResume(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume-download <group-id>",
		Short: "恢复组内失败下载任务",
		Long:  `恢复组内所有失败任务，支持续传或强制重新下载。`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			force, _ := cmd.Flags().GetBool("force")
			if err := svc.CloudResumeGroup(cmd.Context(), args[0], force); err != nil {
				return fmt.Errorf("恢复下载组失败: %w", err)
			}
			ios.WriteOutLine("下载组恢复成功")
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "强制删除后重新下载，不使用续传")
	return cmd
}

// NewCmdCloudGroupDownload 创建 download 子命令，下载组内指定子任务的原始文件。
func NewCmdCloudGroupDownload(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "download <group-id> <sub-task-id> [sub-task-id...]",
		Short: "下载组内指定子任务的原始文件",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			concurrency, _ := cmd.Flags().GetInt("concurrency")
			outputDir, _ := cmd.Flags().GetString("output-dir")

			groupID := args[0]
			detail, err := svc.CloudGetGroup(cmd.Context(), groupID)
			if err != nil {
				return fmt.Errorf("获取下载组 %s 信息失败: %w", groupID, err)
			}
			taskByID := make(map[string]client.CloudTask, len(detail.Tasks))
			for _, t := range detail.Tasks {
				taskByID[t.ID] = t
			}

			items := make([]client.DownloadItem, 0, len(args)-1)
			for _, subID := range args[1:] {
				task, ok := taskByID[subID]
				if !ok {
					return fmt.Errorf("子任务 %s 不在组 %s 中", subID, groupID)
				}
				if task.Status != client.TaskStatusCompleted {
					return fmt.Errorf("子任务 %s 未完成（当前 %s），无法下载原始文件", subID, task.Status)
				}
				cloudPath := ".__cloud__/" + subID + "/" + task.Filename
				items = append(items, client.DownloadItem{RemotePath: cloudPath, LocalPath: filepath.Join(outputDir, task.Filename)})
			}

			ios.WriteOutLine("下载组 %s 中 %d 个子任务原始文件...", groupID, len(items))
			opts := []client.DownloadOption{client.WithDownloadConcurrency(concurrency)}
			if err := svc.DownloadItems(cmd.Context(), items, opts...); err != nil {
				return fmt.Errorf("批量下载失败: %w", err)
			}
			ios.WriteOutLine("  ✓ 全部下载完成")
			return nil
		},
	}
	cmd.Flags().Int("concurrency", 2, "最大并发下载数（0=不限制，1=顺序，默认 2）")
	cmd.Flags().String("output-dir", ".", "本地输出目录（默认当前目录）")
	return cmd
}

// NewCmdCloudGroupDownloadArchive 创建 download-archive 子命令，下载归档文件。
func NewCmdCloudGroupDownloadArchive(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "download-archive <archive-file> [archive-file...]",
		Short: "下载归档文件到本地",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			concurrency, _ := cmd.Flags().GetInt("concurrency")
			outputDir, _ := cmd.Flags().GetString("output-dir")

			items := make([]client.DownloadItem, 0, len(args))
			for _, archiveFile := range args {
				local := filepath.Join(outputDir, filepath.Base(archiveFile))
				// 归档存服务端 uploadsDir/.__cloud_archives__/<name>；download-archive 传归档名
				// + kind=cloud_archive（.__ 内部路径不直接透传，ValidateFilePath 全拒）。
				items = append(items, client.DownloadItem{
					RemotePath: filepath.Base(archiveFile),
					LocalPath:  local,
					Kind:       client.DownloadKindCloudArchive,
				})
			}

			ios.WriteOutLine("下载 %d 个归档文件...", len(items))
			opts := []client.DownloadOption{client.WithDownloadConcurrency(concurrency)}
			if err := svc.DownloadItems(cmd.Context(), items, opts...); err != nil {
				return fmt.Errorf("批量下载归档文件失败: %w", err)
			}
			ios.WriteOutLine("  ✓ 全部下载完成")
			return nil
		},
	}
	cmd.Flags().Int("concurrency", 2, "最大并发下载数（0=不限制，1=顺序，默认 2）")
	cmd.Flags().String("output-dir", ".", "本地输出目录（默认当前目录）")
	return cmd
}

// NewCmdCloudGroupResumeChain 创建 resume-chain 子命令，恢复中断的链式操作。
func NewCmdCloudGroupResumeChain(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume-chain <chain-id>",
		Short: "恢复中断的组链式操作",
		Long: `从缓存恢复并继续执行中断的组链式操作。
需要客户端配置了 cache_dir 以启用链式操作持久化。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			chainID := args[0]
			ios.WriteOutLine("恢复组链式操作: %s", chainID)

			result, err := svc.ResumeChain(cmd.Context(), chainID)
			if err != nil {
				return fmt.Errorf("恢复组链式操作失败: %w", err)
			}

			ios.WriteOutLine("组链式操作完成!")
			ios.WriteOutLine("  本地路径: %s", result.LocalPath())
			if !result.KeepFiles() {
				ios.WriteOutLine("  远端文件: 已清理")
			}
			return nil
		},
	}

	return cmd
}

// NewCmdCloudGroupDelete 创建 delete 子命令。
func NewCmdCloudGroupDelete(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <group-id>",
		Short: "删除下载组及所有关联文件",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			yes, _ := cmd.Flags().GetBool("yes")
			if !yes {
				ios.WriteErrLine("将永久删除下载组 %s 及所有关联文件，请使用 --yes 确认", args[0])
				return fmt.Errorf("delete 需要 --yes 确认")
			}
			if err := svc.CloudDeleteGroup(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("删除下载组失败: %w", err)
			}
			ios.WriteOutLine("下载组已删除")
			return nil
		},
	}
	cmd.Flags().Bool("yes", false, "确认永久删除（组与所有关联文件）")
	return cmd
}
