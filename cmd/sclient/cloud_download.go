// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
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

// collectCloudEntries 汇总位置参数与 --url-file 指定的条目为统一条目列表。
// --url-file 支持每行 "URL" 或 "URL<TAB>FILENAME" 指定保存文件名。
// 行格式契约（Tab 分隔、注释行、CRLF、多 Tab 容错）在域侧：cloudfilename.ReadEntriesFromFile。
func collectCloudEntries(args []string, urlFile string) ([]cloudfilename.Entry, error) {
	var entries []cloudfilename.Entry
	for _, u := range args {
		entries = append(entries, cloudfilename.Entry{URL: u})
	}
	if urlFile != "" {
		fileEntries, err := cloudfilename.ReadEntriesFromFile(urlFile)
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

// buildCloudDownloadChainOpts 组装链式下载选项（含 transfer/save 转存与保留参数）。
func buildCloudDownloadChainOpts(cmd *cobra.Command, pollInterval time.Duration, timeout time.Duration, entries []cloudfilename.Entry, keepFiles bool) []client.ChainOption {
	opts := []client.ChainOption{
		client.WithChainPollInterval(pollInterval),
		client.WithChainTimeout(timeout),
		client.WithChainEntries(entries),
	}
	if keepFiles {
		opts = append(opts, client.WithChainKeepFiles())
	}
	if vol, _ := cmd.Flags().GetString(flagTransferVolume); vol != "" {
		p, _ := cmd.Flags().GetString(flagTransferPath)
		opts = append(opts, client.WithChainTransfer(&client.TransferSpec{Volume: vol, Path: p}))
	}
	if cmd.Flags().Changed(flagSave) {
		s, _ := cmd.Flags().GetBool(flagSave)
		opts = append(opts, client.WithChainSave(s))
	}
	if cmd.Flags().Changed(flagDownloadLocal) {
		l, _ := cmd.Flags().GetBool(flagDownloadLocal)
		opts = append(opts, client.WithChainDownloadLocal(l))
	}
	return opts
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
使用子命令（submit, wait, archive, download, download-archive, delete, list, cancel, resume-chain, resume-download）执行单个步骤。`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			outputDir, _ := cmd.Flags().GetString(flagOutputDir)
			keepFiles, _ := cmd.Flags().GetBool("keep-files")
			pollInterval, _ := cmd.Flags().GetDuration(flagPollInterval)
			timeout, _ := cmd.Flags().GetDuration("timeout")
			urlFile, _ := cmd.Flags().GetString(flagURLFile)

			// 收集 URL 条目（--url-file 每行 URL 或 URL<TAB>FILENAME 指定保存文件名）
			entries, err := collectCloudEntries(args, urlFile)
			if err != nil {
				return err
			}

			archiveName, _ := cmd.Flags().GetString("archive-name")
			if archiveName == "" {
				archiveName = fmt.Sprintf("cloud-download-%d.tar.gz", time.Now().Unix())
			}

			ios.WriteOutLine("链式下载 %d 个 URL...", len(entries))
			opts := buildCloudDownloadChainOpts(cmd, pollInterval, timeout, entries, keepFiles)

			// --timeout 约束整个链式操作（含存储超限重试的退避 sleep），
			// 避免 10s/20s/40s 退避使总时长远超用户配置
			chainCtx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				chainCtx, cancel = context.WithTimeout(cmd.Context(), timeout)
				defer cancel()
			}
			// 服务端任务统计：不假装本地有传输速率（任务在服务端跑），只记提交 URL 数与耗时。
			stats := NewTransferStats()
			stats.ServerSideTask = true
			stats.Requests = len(entries)
			chainStart := time.Now()
			// URLs 由 opts 中的 entries 承载（WithChainEntries），这里传 nil
			result, err := svc.CloudDownloadChain(chainCtx, nil, archiveName, outputDir, opts...)
			if err != nil {
				return fmt.Errorf("链式下载失败: %w", err)
			}
			stats.AddFile("cloud-download", 0, time.Since(chainStart))
			stats.Finalize()

			ios.WriteOutLine("链式下载完成!")
			if result.DownloadedLocal() {
				ios.WriteOutLine("  本地路径: %s", result.LocalPath())
			} else {
				ios.WriteOutLine("  未下载本地（只转存/只保留）")
			}
			if !result.KeepFiles() {
				ios.WriteOutLine("  远端文件: 已清理")
			}
			// 统计行走 formatter：表格输出 FormatLine 文本；--json 输出 stats 对象。
			buildFormatterWithWriter(ios.Out, cmd).PrintTransferStats(stats)
			return nil
		},
	}

	// 注册 flags
	cmd.Flags().String("archive-name", "", "归档文件名（默认自动生成）")
	cmd.Flags().String(flagOutputDir, ".", "本地输出目录")
	cmd.Flags().Bool("keep-files", false, "下载到本地后不删除云端副本")
	cmd.Flags().Duration(flagPollInterval, 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "链式操作超时时间")
	cmd.Flags().String(flagURLFile, "", "从文件读取 URL 条目（每行 URL 或 URL<TAB>FILENAME，FILENAME 为可选保存文件名）")
	cmd.Flags().String(flagTransferVolume, "", "转存目标卷名（下载完成后转存到该卷；secretdata 自动加密）")
	cmd.Flags().String(flagTransferPath, "", "转存目标路径（含文件名；空 = 自动派生）")
	cmd.Flags().Bool(flagSave, true, "保留 cloud 桶副本（false = 任务完成含转存后服务端自动清理，审计可查）")
	cmd.Flags().Bool(flagDownloadLocal, true, "客户端下载本地（链式拉取 cloud 桶文件）；false = 只转存/只保留")

	// 注册子命令
	cmd.AddCommand(NewCmdCloudSubmit(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudWait(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudArchive(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudDownloadFile(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudDownloadArchive(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudDeleteTask(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudResume(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudResumeDownload(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudList(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudCancel(factory, ios, cfgSvc))

	// 兼容性 stub：group 相关子命令已迁移到独立命令 cloud-download-group，
	// 保留空壳提示迁移路径，避免存量脚本静默收到 unknown command。
	cmd.AddCommand(migratedGroupSubcommand("group", "submit",
		"group <name> <url> [url...]", "创建云端下载任务组"))
	cmd.AddCommand(migratedGroupSubcommand("group-list", "list",
		"group-list", "列出所有下载组"))
	cmd.AddCommand(migratedGroupSubcommand("group-archive", "archive",
		"group-archive <group-id> [archive-name]", "打包下载组文件为 tar.gz"))
	cmd.AddCommand(migratedGroupSubcommand("group-cancel", "cancel",
		"group-cancel <group-id>", "取消下载组内所有任务"))
	cmd.AddCommand(migratedGroupSubcommand("group-resume", "resume-download",
		"group-resume <group-id>", "恢复下载组内所有失败任务"))

	// 兼容性 stub：resume 已改名 resume-chain，保留空壳提示迁移路径
	cmd.AddCommand(&cobra.Command{
		Use:   "resume <chain-id>",
		Short: "恢复中断的链式操作",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("命令 \"resume\" 已改名：请使用 'sclient cloud-download resume-chain <chain-id>' 代替")
		},
	})

	return cmd
}

// migratedGroupSubcommand 为已迁移到 cloud-download-group 的子命令创建兼容 stub。
// 用户执行旧命令时打印迁移指引并返回非零退出码，避免静默 unknown command。
func migratedGroupSubcommand(name, newName, use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("命令 %q 已迁移：请使用 'sclient cloud-download-group %s' 代替（%s 相关子命令现归 cloud-download-group 管理）",
				name, newName, name)
		},
	}
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
			return runCloudSubmit(cmd, args, factory, ios)
		},
	}

	cmd.Flags().String(flagURLFile, "", "从文件读取 URL 条目（每行 URL 或 URL<TAB>FILENAME，FILENAME 为可选保存文件名）")
	cmd.Flags().String(flagTransferVolume, "", "转存目标卷名（下载完成后转存到该卷；secretdata 自动加密）")
	cmd.Flags().String(flagTransferPath, "", "转存目标路径（含文件名；空 = 自动派生）")
	cmd.Flags().Bool(flagSave, true, "保留 cloud 桶副本（false = 任务完成含转存后服务端自动清理，审计可查）")
	cmd.Flags().Bool(flagDownloadLocal, true, "客户端下载本地（链式拉取 cloud 桶文件）；false = 只转存/只保留")
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
			return runCloudWait(cmd, args, factory, ios)
		},
	}

	cmd.Flags().Duration(flagPollInterval, 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "等待超时时间")
	return cmd
}

// NewCmdCloudResume 创建 resume-chain 子命令，恢复中断的链式操作。
func NewCmdCloudResume(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume-chain <chain-id>",
		Short: "恢复中断的链式操作",
		Long: `从缓存恢复并继续执行中断的链式操作。
需要客户端配置了 cache_dir 以启用链式操作持久化。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			chainID := args[0]
			ios.WriteOutLine("恢复链式操作: %s", chainID)

			result, err := svc.ResumeChain(cmd.Context(), chainID)
			if err != nil {
				return fmt.Errorf("恢复链式操作失败: %w", err)
			}

			ios.WriteOutLine("链式操作完成!")
			if result.DownloadedLocal() {
				ios.WriteOutLine("  本地路径: %s", result.LocalPath())
			} else {
				ios.WriteOutLine("  未下载本地（只转存/只保留）")
			}
			if !result.KeepFiles() {
				ios.WriteOutLine("  远端文件: 已清理")
			}
			return nil
		},
	}

	return cmd
}

// NewCmdCloudDownloadFile 创建 download 子命令，下载云端下载任务的原始文件到本地。
func NewCmdCloudDownloadFile(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "download <task-id> [task-id...]",
		Short: "下载云端下载任务的原始文件到本地",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			concurrency, _ := cmd.Flags().GetInt("concurrency")
			outputDir, _ := cmd.Flags().GetString(flagOutputDir)

			// 先获取所有任务信息，构造下载条目（需要 Filename 拼远端路径）
			items := make([]client.DownloadItem, 0, len(args))
			for _, taskID := range args {
				task, err := svc.GetCloudTask(cmd.Context(), taskID)
				if err != nil {
					return fmt.Errorf("获取任务 %s 信息失败: %w", taskID, err)
				}
				if task.Status != client.TaskStatusCompleted {
					return fmt.Errorf("任务 %s 未完成（当前 %s），无法下载原始文件", taskID, task.Status)
				}
				// 审查 C1：云任务原始文件存服务端租户 cloud 桶（内部桶），
				// 普通下载不开放内部桶路径访问——改用 kind=cloud_task + <taskID>/<file>
				// （服务端校验任务 owner 后拼接内部路径）。
				remotePath := taskID + "/" + task.Filename
				local := filepath.Join(outputDir, task.Filename)
				items = append(items, client.DownloadItem{RemotePath: remotePath, LocalPath: local, Kind: client.DownloadKindCloudTask})
			}

			ios.WriteOutLine("下载 %d 个任务的原始文件...", len(items))
			opts := []client.DownloadOption{client.WithDownloadConcurrency(concurrency)}
			if err := svc.DownloadItems(cmd.Context(), items, opts...); err != nil {
				return fmt.Errorf("批量下载失败: %w", err)
			}
			ios.WriteOutLine("  ✓ 全部下载完成")
			return nil
		},
	}
	cmd.Flags().Int("concurrency", 2, "最大并发下载数（0=不限制，1=顺序，默认 2）")
	cmd.Flags().String(flagOutputDir, ".", "本地输出目录（默认当前目录）")
	return cmd
}

// NewCmdCloudDownloadArchive 创建 download-archive 子命令，下载已归档的 tar.gz 文件到本地。
func NewCmdCloudDownloadArchive(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "download-archive <archive-file> [archive-file...]",
		Short: "下载已归档的 tar.gz 文件到本地",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			concurrency, _ := cmd.Flags().GetInt("concurrency")
			outputDir, _ := cmd.Flags().GetString(flagOutputDir)

			items := make([]client.DownloadItem, 0, len(args))
			for _, archiveFile := range args {
				local := filepath.Join(outputDir, filepath.Base(archiveFile))
				// 归档存服务端租户 archive 桶；download-archive 传归档名 + kind=cloud_archive，
				// 服务端按 owner 在 archive 桶内拼接（内部桶路径不直接透传）。
				// filepath.Base 同时中和用户输入的路径穿越组件。
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
	cmd.Flags().String(flagOutputDir, ".", "本地输出目录（默认当前目录）")
	return cmd
}

// NewCmdCloudDeleteTask 创建 delete 子命令，删除云端下载任务。
func NewCmdCloudDeleteTask(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <task-id>",
		Short: "删除云端下载任务及关联文件",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			yes, _ := cmd.Flags().GetBool("yes")
			if !yes {
				ios.WriteErrLine("将永久删除任务 %s 及关联云端文件，请使用 --yes 确认", args[0])
				return fmt.Errorf("delete 需要 --yes 确认")
			}
			if err := svc.DeleteCloudTask(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("删除任务失败: %w", err)
			}
			ios.WriteOutLine("任务 %s 已删除", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("yes", false, "确认永久删除（任务与云端文件）")
	return cmd
}

// NewCmdCloudResumeDownload 创建 resume-download 子命令，恢复云端下载任务。
func NewCmdCloudResumeDownload(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume-download <task-id>",
		Short: "恢复云端下载任务（续传或重新下载）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			force, _ := cmd.Flags().GetBool("force")
			if err := svc.CloudResumeTask(cmd.Context(), args[0], force); err != nil {
				return fmt.Errorf("恢复任务失败: %w", err)
			}
			ios.WriteOutLine("任务 %s 已恢复", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "强制重新下载，不使用续传")
	return cmd
}

// cloudDownloadSubmitOpts 组装 submit 子命令的转存/保留/下载本地选项（H3：与链式路径一致透传）。
func cloudDownloadSubmitOpts(cmd *cobra.Command) []client.CloudDownloadOption {
	var opts []client.CloudDownloadOption
	if vol, _ := cmd.Flags().GetString(flagTransferVolume); vol != "" {
		p, _ := cmd.Flags().GetString(flagTransferPath)
		opts = append(opts, client.WithCloudDownloadTransfer(&client.TransferSpec{Volume: vol, Path: p}))
	}
	if cmd.Flags().Changed(flagSave) {
		s, _ := cmd.Flags().GetBool(flagSave)
		opts = append(opts, client.WithCloudDownloadSave(s))
	}
	// I-1：download_local 未显式传时必须按 flag 默认值 true 发送——否则 submit 省略该字段
	// → 服务端解析为 false，与链式入口（恒发 true）分歧：--save=false 下 submit 被判真空洞
	// 400，而链式成功（flag 帮助文案默认 true 也必须兑现）。
	if cmd.Flags().Changed(flagDownloadLocal) {
		l, _ := cmd.Flags().GetBool(flagDownloadLocal)
		opts = append(opts, client.WithCloudDownloadLocal(l))
	} else {
		opts = append(opts, client.WithCloudDownloadLocal(true))
	}
	return opts
}

// runCloudSubmit 执行 cloud-download submit 子命令主体。
func runCloudSubmit(cmd *cobra.Command, args []string, factory clientfactory.Factory, ios cli.IOStreams) error {
	svc, err := factory.NewClient(cmd)
	if err != nil {
		ios.WriteErrLine(errFmtInitClientPrint, err)
		return fmt.Errorf(errFmtInitClient, err)
	}

	urlFile, _ := cmd.Flags().GetString(flagURLFile)
	entries, err := collectCloudEntries(args, urlFile)
	if err != nil {
		return err
	}
	// H3：submit 也透传 transfer/save/download_local（此前旗标注册但调用不带 options 静默丢弃）。
	dlOpts := cloudDownloadSubmitOpts(cmd)

	ios.WriteOutLine("创建云端下载任务...")
	tasks, err := svc.CloudDownloadBatchEntries(cmd.Context(), entries, dlOpts...)
	if err != nil {
		return fmt.Errorf("创建云端下载任务失败: %w", err)
	}

	printCloudTasks(tasks, ios)
	// 任一条目提交失败即返回非零，避免脚本把"部分失败"误判为成功
	// （与链式操作 waitForTasks 的"任何失败即报错"语义保持一致）
	if failed := countCloudFailed(tasks); failed > 0 {
		return fmt.Errorf("%d/%d 个云端下载任务创建失败", failed, len(tasks))
	}
	return nil
}

// cloudTaskStatusLine 生成单条云端下载任务的展示行。
func cloudTaskStatusLine(t client.CloudTask) string {
	statusLine := fmt.Sprintf("  %s: %s", t.ID, t.Status)
	if t.Filename != "" {
		statusLine += fmt.Sprintf(" (%s)", t.Filename)
	}
	if t.Error != "" {
		statusLine += fmt.Sprintf(" - %s", t.Error)
	}
	return statusLine
}

// printCloudTasks 逐行打印云端下载任务状态。
func printCloudTasks(tasks []client.CloudTask, ios cli.IOStreams) {
	for _, t := range tasks {
		ios.WriteOutLine(cloudTaskStatusLine(t))
	}
}

// countCloudFailed 统计状态为 failed 的任务数。
func countCloudFailed(tasks []client.CloudTask) int {
	failedCount := 0
	for _, t := range tasks {
		if t.Status == "failed" {
			failedCount++
		}
	}
	return failedCount
}

// runCloudWait 执行 cloud-download wait 子命令主体。
func runCloudWait(cmd *cobra.Command, args []string, factory clientfactory.Factory, ios cli.IOStreams) error {
	svc, err := factory.NewClient(cmd)
	if err != nil {
		ios.WriteErrLine(errFmtInitClientPrint, err)
		return fmt.Errorf(errFmtInitClient, err)
	}

	// 从 wait 子命令自身的 flags 读取
	pollInterval, _ := cmd.Flags().GetDuration(flagPollInterval)
	timeout, _ := cmd.Flags().GetDuration("timeout")

	tasks, err := fetchCloudTasks(cmd.Context(), svc, args)
	if err != nil {
		return err
	}
	pending := collectCloudPending(tasks)
	if len(pending) == 0 {
		return cloudWaitImmediate(tasks, ios)
	}

	ios.WriteOutLine("等待 %d 个任务完成...", len(pending))
	// timeout>0 才设超时；timeout=0 表示不限时（与链式入口一致）
	pollCtx, cancel := pollCloudContext(cmd.Context(), timeout)
	defer cancel()

	return pollCloudTasks(pollCtx, pollInterval, pending, svc, ios)
}

// fetchCloudTasks 获取初始任务状态（任一初始获取失败即返回错误，不伪造 failed）。
func fetchCloudTasks(ctx context.Context, svc *client.FileClient, ids []string) ([]client.CloudTask, error) {
	tasks := make([]client.CloudTask, len(ids))
	for i, id := range ids {
		task, getErr := svc.GetCloudTask(ctx, id)
		if getErr != nil {
			// 初始获取失败说明无法确认任务真实状态，不能伪造 failed 假装完成
			return nil, fmt.Errorf("获取任务 %s 信息失败: %w", id, getErr)
		}
		tasks[i] = *task
	}
	return tasks, nil
}

// collectCloudPending 收集待轮询任务（pending/downloading 状态）。
func collectCloudPending(tasks []client.CloudTask) map[string]client.CloudTask {
	pending := make(map[string]client.CloudTask)
	for _, t := range tasks {
		if t.Status == "pending" || t.Status == "downloading" {
			pending[t.ID] = t
		}
	}
	return pending
}

// cloudFailedErr 将失败/取消任务列表归纳为统一错误（空列表返回 nil）。
func cloudFailedErr(failedIDs []string) error {
	if len(failedIDs) > 0 {
		return fmt.Errorf("%d 个任务失败/取消: %s", len(failedIDs), strings.Join(failedIDs, ", "))
	}
	return nil
}

// cloudWaitImmediate 处理初始状态无待轮询任务的场景。
func cloudWaitImmediate(tasks []client.CloudTask, ios cli.IOStreams) error {
	var failedIDs []string
	for _, t := range tasks {
		ios.WriteOutLine("  %s: %s", t.ID, t.Status)
		if t.Status == "failed" || t.Status == "cancelled" {
			failedIDs = append(failedIDs, t.ID)
		}
	}
	// 初始状态即含失败/取消任务时返回非零（cancelled 计入失败——用户确认 cancelled=失败）
	return cloudFailedErr(failedIDs)
}

// pollCloudContext 根据 timeout 构造轮询上下文（0 = 不限时）。
func pollCloudContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}

// pollCloudTasks 轮询等待 pending 任务完成，返回统一失败错误。
func pollCloudTasks(pollCtx context.Context, pollInterval time.Duration, pending map[string]client.CloudTask, svc *client.FileClient, ios cli.IOStreams) error {
	var failedIDs []string
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for len(pending) > 0 {
		select {
		case <-pollCtx.Done():
			if len(pending) > 0 {
				return pollCtx.Err()
			}
			return cloudFailedErr(failedIDs)
		case <-ticker.C:
			for id := range pending {
				task, pollErr := svc.GetCloudTask(pollCtx, id)
				if pollErr != nil {
					// 轮询失败无法确认任务完成，不能静默丢弃后返回成功
					return fmt.Errorf("轮询任务 %s 状态失败: %w", id, pollErr)
				}
				pollCloudTaskResult(id, *task, pending, &failedIDs, ios)
			}
		}
	}
	// 任一任务失败/取消即返回非零（与链式 waitForTasks 语义一致）
	return cloudFailedErr(failedIDs)
}

// pollCloudTaskResult 处理单次轮询到的任务状态。
func pollCloudTaskResult(id string, task client.CloudTask, pending map[string]client.CloudTask, failedIDs *[]string, ios cli.IOStreams) {
	switch task.Status {
	case "completed":
		delete(pending, id)
		ios.WriteOutLine("  ✓ %s: 完成 (%s, %d bytes)", id, task.Filename, task.TotalSize)
		// M4：save=false 清理失败要可见（cloud 桶残留一直占盘）——completed 但 CleanupStatus=failed。
		if task.CleanupStatus == "failed" {
			ios.WriteOutLine("  ⚠ %s: cloud 桶文件清理失败（残留保留），原因: %s", id, task.CleanupErr)
		}
	case "failed":
		delete(pending, id)
		ios.WriteOutLine("  ✗ %s: 失败 - %s", id, task.Error)
		*failedIDs = append(*failedIDs, id)
	case "cancelled":
		// cancelled 计入失败（与链式 waitForTasks 一致）
		delete(pending, id)
		ios.WriteOutLine("  ✗ %s: 已取消", id)
		*failedIDs = append(*failedIDs, id)
	default:
		pct := int64(0)
		if task.TotalSize > 0 {
			pct = task.Downloaded * 100 / task.TotalSize
		}
		ios.WriteOutLine("  ⟳ %s: %d%% (%d/%d bytes)", id, pct, task.Downloaded, task.TotalSize)
	}
}
