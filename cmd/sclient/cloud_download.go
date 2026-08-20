// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/spf13/cobra"
)

// readEntriesFromFile 从文件中读取云端下载条目（每行一个）。
// 每行格式为 "URL" 或 "URL<TAB>FILENAME"（Tab 分隔的可选保存文件名，
// 因为 URL 本身可能包含空格，文件名与 URL 之间必须用 Tab 分隔）。
// 若一行含多个 Tab，仅取前两列（URL 与 FILENAME），多余 Tab 忽略——FILENAME 本身
// 允许包含 Tab 字符，不应因额外 Tab 拒绝整份文件。
// 忽略空行和 # 开头的注释行。
func readEntriesFromFile(path string) ([]cloudfilename.Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []cloudfilename.Entry
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		entry := cloudfilename.Entry{URL: strings.TrimSpace(parts[0])}
		if len(parts) > 1 {
			entry.Filename = strings.TrimSpace(parts[1])
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// collectCloudEntries 汇总位置参数与 --url-file 指定的条目为统一条目列表。
// --url-file 支持每行 "URL" 或 "URL<TAB>FILENAME" 指定保存文件名。
func collectCloudEntries(args []string, urlFile string) ([]cloudfilename.Entry, error) {
	var entries []cloudfilename.Entry
	for _, u := range args {
		entries = append(entries, cloudfilename.Entry{URL: u})
	}
	if urlFile != "" {
		fileEntries, err := readEntriesFromFile(urlFile)
		if err != nil {
			return nil, fmt.Errorf("读取 url-file 失败: %w", err)
		}
		entries = append(entries, fileEntries...)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("未指定下载 URL，请提供 URL 参数或使用 --url-file 指定文件")
	}
	return entries, nil
}

// resolvedFilename 返回条目最终保存的文件名（客户端侧预览，与服务端规则一致）：
// 显式指定则清理，否则按 URL 自动生成后再清理。
func resolvedFilename(entry cloudfilename.Entry) string {
	if entry.Filename != "" {
		return cloudfilename.Safe(entry.Filename)
	}
	return cloudfilename.Safe(cloudfilename.DefaultFromURL(entry.URL))
}

// NewCmdCloudDownload 创建云端下载命令的工厂函数。
// 默认行为是完整链式操作：提交 → 等待 → 打包 → 下载 → 清理。
func NewCmdCloudDownload(factory clientfactory.Factory, ios cli.IOStreams, st *state.State, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud-download <url> [url...]",
		Short: "从云端下载文件（链式操作：提交→等待→打包→下载→清理）",
		Long: `通过 sproxy 服务端从外部 URL 下载文件，自动执行完整链式操作：

  1. 提交云端下载任务
  2. 等待下载完成
  3. 打包归档为 tar.gz
  4. 下载到本地
  5. 清理远端文件

使用 --keep-files 跳过清理步骤。
使用子命令（submit, wait, archive, fetch, list, cancel, resume）执行单个步骤。`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			archiveName, _ := cmd.Flags().GetString("archive-name")
			if archiveName == "" {
				archiveName = fmt.Sprintf("cloud-download-%d.tar.gz", time.Now().Unix())
			}
			outputDir, _ := cmd.Flags().GetString("output-dir")
			keepFiles, _ := cmd.Flags().GetBool("keep-files")
			pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			urlFile, _ := cmd.Flags().GetString("url-file")

			// 收集 URL 条目（--url-file 每行 URL 或 URL<TAB>FILENAME 指定保存文件名）
			entries, err := collectCloudEntries(args, urlFile)
			if err != nil {
				return err
			}

			ios.WriteOutLine("链式下载 %d 个 URL...", len(entries))
			opts := []client.ChainOption{
				client.WithChainPollInterval(pollInterval),
				client.WithChainTimeout(timeout),
				client.WithChainEntries(entries),
			}
			if keepFiles {
				opts = append(opts, client.WithChainKeepFiles())
			}

			// --timeout 约束整个链式操作（含存储超限重试的退避 sleep），
			// 避免 10s/20s/40s 退避使总时长远超用户配置
			chainCtx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				chainCtx, cancel = context.WithTimeout(cmd.Context(), timeout)
				defer cancel()
			}
			// URLs 由 opts 中的 entries 承载（WithChainEntries），这里传 nil
			result, err := svc.CloudDownloadChain(chainCtx, nil, archiveName, outputDir, opts...)
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
	cmd.AddCommand(NewCmdCloudSubmit(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudWait(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudArchive(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudFetch(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudResume(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudList(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudCancel(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroup(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupList(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupArchive(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupCancel(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudGroupResume(factory, ios, cfgSvc))

	return cmd
}

// NewCmdCloudGroup 创建 group-submit 子命令。
func NewCmdCloudGroup(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group <name> <url> [url...]",
		Short: "创建云端下载任务组",
		Long:  `创建一组云端下载任务，文件下载到同一目录，支持组级打包。`,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if len(args) == 0 {
				return fmt.Errorf("请提供组名称: group <name> <url> [url...]")
			}
			name := args[0]
			urlFile, _ := cmd.Flags().GetString("url-file")
			entries, err := collectCloudEntries(args[1:], urlFile)
			if err != nil {
				return err
			}

			// 客户端预校验：组内保存文件名必须唯一（与服务端 CreateGroup 规则一致），
			// 且同一 URL 不允许出现两次（服务端 CreateTask 会去重并返回 409）。
			// 冲突在发送前拦截，避免服务端 409 往返；同时展示每个 URL 的最终保存文件名。
			ios.WriteOutLine("创建下载组 %q (%d 个条目):", name, len(entries))
			filenameSeen := make(map[string]string, len(entries))
			urlSeen := make(map[string]bool, len(entries))
			var conflicts []string
			for _, e := range entries {
				fn, err := cloudfilename.ResolveFilename(e)
				if err != nil {
					return fmt.Errorf("条目 %s 文件名无效: %w", e.URL, err)
				}
				if prev, ok := filenameSeen[fn]; ok {
					conflicts = append(conflicts, fmt.Sprintf("文件名 %q (URL: %s 与 %s)", fn, prev, e.URL))
				} else {
					filenameSeen[fn] = e.URL
				}
				if urlSeen[e.URL] {
					conflicts = append(conflicts, fmt.Sprintf("重复 URL %s（可指定不同保存文件名也无法消除）", e.URL))
				}
				urlSeen[e.URL] = true
				ios.WriteOutLine("  %s -> %s", e.URL, fn)
			}
			if len(conflicts) > 0 {
				return fmt.Errorf("组内条目冲突，无法创建：%s；请在 --url-file 中为冲突条目指定不同的保存文件名（URL<TAB>FILENAME）",
					strings.Join(conflicts, ", "))
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

// NewCmdCloudGroupList 创建 group-list 子命令。
func NewCmdCloudGroupList(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group-list",
		Short: "列出所有下载组",
		Long:  `列出所有云端下载任务组及其状态。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			status, _ := cmd.Flags().GetString("status")
			groups, err := svc.CloudListGroups(cmd.Context(), status)
			if err != nil {
				return fmt.Errorf("列举下载组失败: %w", err)
			}
			if len(groups) == 0 {
				ios.WriteOutLine("暂无下载组")
				return nil
			}
			for _, g := range groups {
				ios.WriteOutLine("  %s: %s (%s) %d/%d 完成", g.ID, g.Name, g.Status, g.Completed, g.TotalTasks)
			}
			return nil
		},
	}
	cmd.Flags().String("status", "", "按状态过滤 (pending|downloading|completed|partial|failed|cancelled)")
	return cmd
}

// NewCmdCloudGroupArchive 创建 group-archive 子命令。
func NewCmdCloudGroupArchive(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group-archive <group-id> [archive-name]",
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
			// 未指定归档名时不传（空串），由服务端生成唯一名 group-<id>-<unix>.tar.gz，
			// 避免客户端静态名导致多次归档互相覆盖
			archiveName := ""
			if len(args) > 1 {
				archiveName = args[1]
			}
			result, err := svc.CloudArchiveGroup(cmd.Context(), groupID, archiveName)
			if err != nil {
				return fmt.Errorf("打包下载组失败: %w", err)
			}
			ios.WriteOutLine("打包完成: %s (%d bytes)", result.File, result.Size)
			return nil
		},
	}
	return cmd
}

// NewCmdCloudGroupCancel 创建 group-cancel 子命令。
func NewCmdCloudGroupCancel(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group-cancel <group-id>",
		Short: "取消下载组内所有任务",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if err := svc.CloudCancelGroup(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("取消下载组失败: %w", err)
			}
			ios.WriteOutLine("下载组已取消")
			return nil
		},
	}
	return cmd
}

// NewCmdCloudGroupResume 创建 group-resume 子命令。
func NewCmdCloudGroupResume(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group-resume <group-id>",
		Short: "恢复下载组内所有失败任务",
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

// NewCmdCloudSubmit 创建 submit 子命令，仅提交云端下载任务。
func NewCmdCloudSubmit(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "submit <url> [url...]",
		Short: "提交云端下载任务（不等待完成）",
		Long: `提交 URL 到服务端进行云端下载，返回任务 ID 和状态。
不等待任务完成，使用 wait 子命令轮询。
支持多个 URL 参数或通过 --url-file 从文件读取 URL 条目。`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			urlFile, _ := cmd.Flags().GetString("url-file")
			entries, err := collectCloudEntries(args, urlFile)
			if err != nil {
				return err
			}

			ios.WriteOutLine("创建云端下载任务...")
			tasks, err := svc.CloudDownloadBatchEntries(cmd.Context(), entries)
			if err != nil {
				return fmt.Errorf("创建云端下载任务失败: %w", err)
			}

			for _, t := range tasks {
				statusLine := fmt.Sprintf("  %s: %s", t.ID, t.Status)
				if t.Filename != "" {
					statusLine += fmt.Sprintf(" (%s)", t.Filename)
				}
				if t.Error != "" {
					statusLine += fmt.Sprintf(" - %s", t.Error)
				}
				ios.WriteOutLine(statusLine)
			}

			// 任一条目提交失败即返回非零，避免脚本把"部分失败"误判为成功
			//（与链式操作 waitForTasks 的"任何失败即报错"语义保持一致）
			failedCount := 0
			for _, t := range tasks {
				if t.Status == "failed" {
					failedCount++
				}
			}
			if failedCount > 0 {
				return fmt.Errorf("%d/%d 个云端下载任务创建失败", failedCount, len(tasks))
			}
			return nil
		},
	}

	cmd.Flags().String("url-file", "", "从文件读取 URL 条目（每行 URL 或 URL<TAB>FILENAME，FILENAME 为可选保存文件名）")
	return cmd
}

// NewCmdCloudWait 创建 wait 子命令，等待云端下载任务完成。
func NewCmdCloudWait(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wait <task-id> [task-id...]",
		Short: "等待云端下载任务完成",
		Long:  `轮询等待指定云端下载任务完成，显示下载进度。`,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			// 从 wait 子命令自身的 flags 读取
			pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
			timeout, _ := cmd.Flags().GetDuration("timeout")

			// 获取初始任务状态
			tasks := make([]client.CloudTask, len(args))
			for i, id := range args {
				task, getErr := svc.GetCloudTask(cmd.Context(), id)
				if getErr != nil {
					// 初始获取失败说明无法确认任务真实状态，不能伪造 failed 假装完成
					return fmt.Errorf("获取任务 %s 信息失败: %w", id, getErr)
				}
				tasks[i] = *task
			}

			// 收集待轮询任务
			pending := make(map[string]client.CloudTask)
			for _, t := range tasks {
				if t.Status == "pending" || t.Status == "downloading" {
					pending[t.ID] = t
				}
			}

			if len(pending) == 0 {
				var failedIDs []string
				for _, t := range tasks {
					ios.WriteOutLine("  %s: %s", t.ID, t.Status)
					if t.Status == "failed" {
						failedIDs = append(failedIDs, t.ID)
					}
				}
				// 初始状态即含失败任务时返回非零（与链式 waitForTasks 语义一致：
				// 仅 failed 视为失败，cancelled 属用户主动取消，不算失败）
				if len(failedIDs) > 0 {
					return fmt.Errorf("%d 个任务失败: %s", len(failedIDs), strings.Join(failedIDs, ", "))
				}
				return nil
			}

			ios.WriteOutLine("等待 %d 个任务完成...", len(pending))

			pollCtx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()

			results := make(map[string]client.CloudTask)
			for _, t := range tasks {
				results[t.ID] = t
			}

			var failedIDs []string
			for len(pending) > 0 {
				select {
				case <-pollCtx.Done():
					if len(pending) > 0 {
						return pollCtx.Err()
					}
					if len(failedIDs) > 0 {
						return fmt.Errorf("%d 个任务失败: %s", len(failedIDs), strings.Join(failedIDs, ", "))
					}
					return nil
				case <-ticker.C:
					for id := range pending {
						task, pollErr := svc.GetCloudTask(pollCtx, id)
						if pollErr != nil {
							// 轮询失败无法确认任务完成，不能静默丢弃后返回成功
							return fmt.Errorf("轮询任务 %s 状态失败: %w", id, pollErr)
						}
						results[id] = *task
						switch task.Status {
						case "completed":
							delete(pending, id)
							ios.WriteOutLine("  ✓ %s: 完成 (%s, %d bytes)", id, task.Filename, task.TotalSize)
						case "failed":
							delete(pending, id)
							ios.WriteOutLine("  ✗ %s: 失败 - %s", id, task.Error)
							failedIDs = append(failedIDs, id)
						case "cancelled":
							// cancelled 属用户主动取消，不算失败（与链式 waitForTasks 语义一致）
							delete(pending, id)
							ios.WriteOutLine("  ✗ %s: 已取消", id)
						default:
							pct := int64(0)
							if task.TotalSize > 0 {
								pct = task.Downloaded * 100 / task.TotalSize
							}
							ios.WriteOutLine("  ⟳ %s: %d%% (%d/%d bytes)", id, pct, task.Downloaded, task.TotalSize)
						}
					}
				}
			}
			// 任一任务失败即返回非零（与链式 waitForTasks 语义一致），
			// 避免脚本把部分失败误判为全部成功
			if len(failedIDs) > 0 {
				return fmt.Errorf("%d 个任务失败: %s", len(failedIDs), strings.Join(failedIDs, ", "))
			}
			return nil
		},
	}

	cmd.Flags().Duration("poll-interval", 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "等待超时时间")
	return cmd
}

// NewCmdCloudFetch 创建 fetch 子命令，执行完整链式下载。
func NewCmdCloudFetch(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fetch <url> [url...]",
		Short: "完整链式下载（提交→等待→打包→下载→清理）",
		Long: `完整链式操作：提交云端下载任务，等待完成，打包归档，下载到本地，清理远端文件。
与 cloud-download 主命令行为相同，作为子命令提供以方便在脚本中使用。`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			archiveName, _ := cmd.Flags().GetString("archive-name")
			if archiveName == "" {
				archiveName = fmt.Sprintf("cloud-download-%d.tar.gz", time.Now().Unix())
			}
			outputDir, _ := cmd.Flags().GetString("output-dir")
			keepFiles, _ := cmd.Flags().GetBool("keep-files")
			pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			urlFile, _ := cmd.Flags().GetString("url-file")

			// 收集 URL 条目（--url-file 每行 URL 或 URL<TAB>FILENAME 指定保存文件名）
			entries, err := collectCloudEntries(args, urlFile)
			if err != nil {
				return err
			}

			ios.WriteOutLine("链式下载 %d 个 URL...", len(entries))
			opts := []client.ChainOption{
				client.WithChainPollInterval(pollInterval),
				client.WithChainTimeout(timeout),
				client.WithChainEntries(entries),
			}
			if keepFiles {
				opts = append(opts, client.WithChainKeepFiles())
			}

			// --timeout 约束整个链式操作（含存储超限重试的退避 sleep），
			// 避免 10s/20s/40s 退避使总时长远超用户配置
			chainCtx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				chainCtx, cancel = context.WithTimeout(cmd.Context(), timeout)
				defer cancel()
			}
			// URLs 由 opts 中的 entries 承载（WithChainEntries），这里传 nil
			result, err := svc.CloudDownloadChain(chainCtx, nil, archiveName, outputDir, opts...)
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

	cmd.Flags().String("archive-name", "", "归档文件名（默认自动生成）")
	cmd.Flags().String("output-dir", ".", "本地输出目录")
	cmd.Flags().Bool("keep-files", false, "下载到本地后不删除云端副本")
	cmd.Flags().Duration("poll-interval", 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "链式操作超时时间")
	cmd.Flags().String("url-file", "", "从文件读取 URL 条目（每行 URL 或 URL<TAB>FILENAME，FILENAME 为可选保存文件名）")

	return cmd
}

// NewCmdCloudResume 创建 resume 子命令，恢复中断的链式操作。
func NewCmdCloudResume(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume <chain-id>",
		Short: "恢复中断的链式操作",
		Long: `从缓存恢复并继续执行中断的链式操作。
需要客户端配置了 cache_dir 以启用链式操作持久化。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			chainID := args[0]
			ios.WriteOutLine("恢复链式操作: %s", chainID)

			result, err := svc.ResumeChain(cmd.Context(), chainID)
			if err != nil {
				return fmt.Errorf("恢复链式操作失败: %w", err)
			}

			ios.WriteOutLine("链式操作完成!")
			ios.WriteOutLine("  本地路径: %s", result.LocalPath())
			if !result.KeepFiles() {
				ios.WriteOutLine("  远端文件: 已清理")
			}
			return nil
		},
	}

	return cmd
}

// extractTarGz 解压 tar.gz 文件到指定目录。
func extractTarGz(src, destDir string) error {
	file, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开归档文件失败: %w", err)
	}
	defer file.Close()

	gr, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("创建 gzip reader 失败: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 tar 头失败: %w", err)
		}

		targetPath := filepath.Join(destDir, filepath.Clean(header.Name))
		// 路径穿越防护：filepath.Clean 不会解开 .. 段（../../evil 原样保留），仅做
		// 前缀检查会被 Join(destDir, ../../evil)=destDir/../../evil 逃过。必须用
		// filepath.Rel 校验规范化后的最终路径仍位于 destDir 之内，否则 tar 内的
		// 恶意 header.Name 可写出 destDir（任意文件写）。
		rel, relErr := filepath.Rel(destDir, targetPath)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			// 路径穿越保护
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return fmt.Errorf("创建目录失败: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return fmt.Errorf("创建目录失败: %w", err)
			}
			outFile, err := os.Create(targetPath)
			if err != nil {
				return fmt.Errorf("创建文件失败: %w", err)
			}
			if _, err := io.CopyN(outFile, tr, header.Size); err != nil {
				outFile.Close()
				return fmt.Errorf("写入文件失败: %w", err)
			}
			outFile.Close()
		}
	}
	return nil
}
