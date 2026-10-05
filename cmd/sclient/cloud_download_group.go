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
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			return runCloudDownloadGroupChain(cmd, ios, svc, args)
		},
	}

	// 注册 flags
	cmd.Flags().String("archive-name", "", "归档文件名（默认自动生成）")
	cmd.Flags().String(flagOutputDir, ".", "本地输出目录")
	cmd.Flags().Bool("keep-files", false, "下载到本地后不删除云端副本")
	cmd.Flags().Duration(flagPollInterval, 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "链式操作超时时间")
	cmd.Flags().String(flagURLFile, "", "从文件读取 URL 条目（每行 URL 或 URL<TAB>FILENAME，FILENAME 为可选保存文件名）")
	cmd.Flags().String(flagTransferVolume, "", "转存目标卷名（组内每个任务下载完成后转存到该卷；secretdata 自动加密）")
	cmd.Flags().String(flagTransferPath, "", "转存目标路径（含文件名；空 = 自动派生）")
	cmd.Flags().Bool(flagSave, true, "保留 cloud 桶副本（false = 任务完成含转存后服务端自动清理，审计可查）")
	cmd.Flags().Bool(flagDownloadLocal, true, "客户端下载本地（链式拉取组归档）；false = 只转存/只保留")

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

// runCloudDownloadGroupChain 执行组链式下载的完整流程（创建组→等待→打包→下载→清理）。
func runCloudDownloadGroupChain(cmd *cobra.Command, ios cli.IOStreams, svc *client.FileClient, args []string) error {
	p, planErr := cloudGroupChainPlan(cmd, ios, args)
	if planErr != nil {
		return planErr
	}
	return runCloudGroupChain(cmd, ios, svc, p)
}

// cloudGroupChainParams 是组链式下载的参数集合（flag 解析 + 客户端预校验结果）。
type cloudGroupChainParams struct {
	name          string
	archiveName   string
	outputDir     string
	keepFiles     bool
	pollInterval  time.Duration
	timeout       time.Duration
	entries       []cloudfilename.Entry
	transfer      *client.TransferSpec // 组内每个任务转存目标（nil = 不转存）
	save          *bool                // 保留 cloud 桶副本（nil = 默认 true）
	downloadLocal bool                 // 客户端是否下载本地（false = 只转存/只保留）
}

// cloudGroupChainPlan 解析组链式下载相关 flags 与 URL 条目，并做客户端预校验。
func cloudGroupChainPlan(cmd *cobra.Command, ios cli.IOStreams, args []string) (cloudGroupChainParams, error) {
	name := args[0]
	archiveName, _ := cmd.Flags().GetString("archive-name")
	if archiveName == "" {
		archiveName = fmt.Sprintf("cloud-download-group-%d.tar.gz", time.Now().Unix())
	}
	outputDir, _ := cmd.Flags().GetString(flagOutputDir)
	keepFiles, _ := cmd.Flags().GetBool("keep-files")
	pollInterval, _ := cmd.Flags().GetDuration(flagPollInterval)
	timeout, _ := cmd.Flags().GetDuration("timeout")
	urlFile, _ := cmd.Flags().GetString(flagURLFile)

	// 三参（transfer/save/download_local）：与单条 chain 语义对齐（C1/M6）。
	var transfer *client.TransferSpec
	if vol, _ := cmd.Flags().GetString(flagTransferVolume); vol != "" {
		p, _ := cmd.Flags().GetString(flagTransferPath)
		transfer = &client.TransferSpec{Volume: vol, Path: p}
	}
	var save *bool
	if cmd.Flags().Changed(flagSave) {
		s, _ := cmd.Flags().GetBool(flagSave)
		save = &s
	}
	downloadLocal := true
	if cmd.Flags().Changed(flagDownloadLocal) {
		l, _ := cmd.Flags().GetBool(flagDownloadLocal)
		downloadLocal = l
	}

	if len(args) < 2 && urlFile == "" {
		return cloudGroupChainParams{}, fmt.Errorf("请提供组名和至少一个 URL，或使用 --url-file 指定 URL 文件")
	}

	entries, collectErr := collectCloudEntries(args[1:], urlFile)
	if collectErr != nil {
		return cloudGroupChainParams{}, collectErr
	}
	if preflightErr := preflightGroupEntries(ios, name, entries); preflightErr != nil {
		return cloudGroupChainParams{}, preflightErr
	}
	return cloudGroupChainParams{
		name: name, archiveName: archiveName, outputDir: outputDir,
		keepFiles: keepFiles, pollInterval: pollInterval, timeout: timeout, entries: entries,
		transfer: transfer, save: save, downloadLocal: downloadLocal,
	}, nil
}

// runCloudGroupChain 执行组链式下载的主体流程（构建选项 → 链式调用 → 结果展示）。
func runCloudGroupChain(cmd *cobra.Command, ios cli.IOStreams, svc *client.FileClient, p cloudGroupChainParams) error {
	ios.WriteOutLine("链式下载组 %q (%d 个条目)...", p.name, len(p.entries))
	opts := []client.ChainOption{
		client.WithChainPollInterval(p.pollInterval),
		client.WithChainTimeout(p.timeout),
	}
	if p.keepFiles {
		opts = append(opts, client.WithChainKeepFiles())
	}
	// 三参透传（transfer/save/download_local）：组链与单条 chain 语义对齐（M6/F1）。
	if p.transfer != nil {
		opts = append(opts, client.WithChainTransfer(p.transfer))
	}
	if p.save != nil {
		opts = append(opts, client.WithChainSave(*p.save))
	}
	opts = append(opts, client.WithChainDownloadLocal(p.downloadLocal))

	chainCtx := cmd.Context()
	if p.timeout > 0 {
		var cancel context.CancelFunc
		chainCtx, cancel = context.WithTimeout(cmd.Context(), p.timeout)
		defer cancel()
	}
	result, err := svc.CloudDownloadGroupChain(chainCtx, p.name, p.entries, p.archiveName, p.outputDir, opts...)
	if err != nil {
		return fmt.Errorf("链式下载失败: %w", err)
	}

	ios.WriteOutLine("链式下载完成!")
	if result.DownloadedLocal() {
		ios.WriteOutLine("  本地路径: %s", result.LocalPath())
	} else {
		ios.WriteOutLine("  未下载本地（只转存/只保留）")
	}
	if !result.KeepFiles() {
		ios.WriteOutLine("  远端文件: 已清理")
	}
	return nil
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
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			name := args[0]
			urlFile, _ := cmd.Flags().GetString(flagURLFile)
			entries, collectErr := collectCloudEntries(args[1:], urlFile)
			if collectErr != nil {
				return collectErr
			}
			if preflightErr := preflightGroupEntries(ios, name, entries); preflightErr != nil {
				return preflightErr
			}

			// 三参透传（C1/F1）：submit 子命令同样可声明转存/保留/下载本地——
			// 父命令 flag 非 persistent，子命令须自注册同款旗标（否则 --transfer-volume
			// 在 submit 下是 unknown flag，组转存入口不可达）。
			opts := []client.CloudDownloadOption{}
			if vol, _ := cmd.Flags().GetString(flagTransferVolume); vol != "" {
				p, _ := cmd.Flags().GetString(flagTransferPath)
				opts = append(opts, client.WithCloudDownloadTransfer(&client.TransferSpec{Volume: vol, Path: p}))
			}
			if cmd.Flags().Changed(flagSave) {
				s, _ := cmd.Flags().GetBool(flagSave)
				opts = append(opts, client.WithCloudDownloadSave(s))
			}
			// I-1（组 submit 同款）：download_local 未显式传 → 按 flag 默认 true 发送，与链式
			// 入口一致——否则 --save=false 下 submit 判真空洞 400、链式却成功。
			if cmd.Flags().Changed(flagDownloadLocal) {
				l, _ := cmd.Flags().GetBool(flagDownloadLocal)
				opts = append(opts, client.WithCloudDownloadLocal(l))
			} else {
				opts = append(opts, client.WithCloudDownloadLocal(true))
			}

			group, err := svc.CloudCreateGroupEntries(cmd.Context(), name, entries, opts...)
			if err != nil {
				return fmt.Errorf("创建下载组失败: %w", err)
			}
			ios.WriteOutLine("  组 ID: %s", group.ID)
			ios.WriteOutLine("  状态: %s", group.Status)
			ios.WriteOutLine("  任务数: %d", group.TotalTasks)
			return nil
		},
	}
	cmd.Flags().String(flagURLFile, "", "从文件读取 URL 条目（每行 URL 或 URL<TAB>FILENAME，FILENAME 为可选保存文件名）")
	// 三参（C1/F1）：submit 子命令自注册（父命令 flag 非 persistent 不可达）。
	cmd.Flags().String(flagTransferVolume, "", "转存目标卷名（组内每个任务下载完成后转存到该卷；secretdata 自动加密）")
	cmd.Flags().String(flagTransferPath, "", "转存目标路径（含文件名；空 = 自动派生）")
	cmd.Flags().Bool(flagSave, true, "保留 cloud 桶副本（false = 任务完成含转存后服务端自动清理，审计可查）")
	cmd.Flags().Bool(flagDownloadLocal, true, "客户端下载本地（链式拉取组归档）；false = 只转存/只保留")
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
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			groupID := args[0]
			pollInterval, _ := cmd.Flags().GetDuration(flagPollInterval)
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

			return cloudGroupWaitPoll(svc, pollCtx, groupID, ios, ticker)
		},
	}
	cmd.Flags().Duration(flagPollInterval, 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "等待超时时间")
	return cmd
}

// cloudGroupWaitPoll 轮询等待下载组完成，直到组到终态或 pollCtx 取消。
func cloudGroupWaitPoll(svc *client.FileClient, pollCtx context.Context, groupID string, ios cli.IOStreams, ticker *time.Ticker) error {
	for {
		select {
		case <-pollCtx.Done():
			return pollCtx.Err()
		case <-ticker.C:
			done, pollErr := cloudGroupWaitPollOnce(svc, pollCtx, groupID, ios)
			if done {
				return pollErr
			}
		}
	}
}

// cloudGroupWaitPollOnce 执行一次轮询并返回是否应停止等待（done=true 时 pollErr 即结果）。
func cloudGroupWaitPollOnce(svc *client.FileClient, pollCtx context.Context, groupID string, ios cli.IOStreams) (bool, error) {
	detail, err := svc.CloudGetGroup(pollCtx, groupID)
	if err != nil {
		return true, fmt.Errorf("轮询下载组状态失败: %w", err)
	}
	if detail.Group == nil {
		return true, fmt.Errorf("下载组 %s 不存在", groupID)
	}
	completed, failed, cancelled, active := cloudGroupCountTasks(detail.Tasks)
	ios.WriteOutLine("  %s: %s (%d/%d 完成, %d 失败, %d 取消, %d 进行中)",
		groupID, detail.Group.Status, completed, detail.Group.TotalTasks, failed, cancelled, active)

	if failed > 0 || cancelled > 0 {
		return true, fmt.Errorf("下载组 %s 有 %d 个失败, %d 个取消，无法完成", groupID, failed, cancelled)
	}
	if active == 0 {
		// 组状态为 failed/cancelled 且无活跃任务（空列表边界）→ 终态，视为异常报错，
		// 避免转圈到超时（C7）。
		if detail.Group.Status == "failed" || detail.Group.Status == "cancelled" {
			return true, fmt.Errorf("下载组 %s 已终止（状态 %s），无法完成", groupID, detail.Group.Status)
		}
		// 防御：服务端在极早期可能返回空 tasks，但组状态尚未到 completed。
		// 只有组状态为 completed（或 tasks 非空且无活跃）才视为完成。
		if detail.Group.Status == "completed" || len(detail.Tasks) > 0 {
			return true, nil
		}
		return false, nil
	}
	return false, nil
}

// cloudGroupCountTasks 统计组内子任务状态计数。
func cloudGroupCountTasks(tasks []client.CloudTask) (completed, failed, cancelled, active int) {
	for _, t := range tasks {
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
	return
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
				ios.WriteErrLine(errFmtInitClientPrint, err)
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
				ios.WriteErrLine(errFmtInitClientPrint, err)
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
				ios.WriteErrLine(errFmtInitClientPrint, err)
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
				ios.WriteErrLine(errFmtInitClientPrint, err)
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
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			return runCloudGroupDownload(cmd, ios, svc, args)
		},
	}
	cmd.Flags().Int("concurrency", 2, "最大并发下载数（0=不限制，1=顺序，默认 2）")
	cmd.Flags().String(flagOutputDir, ".", "本地输出目录（默认当前目录）")
	return cmd
}

// runCloudGroupDownload 下载组内指定子任务的原始文件。
func runCloudGroupDownload(cmd *cobra.Command, ios cli.IOStreams, svc *client.FileClient, args []string) error {
	concurrency, _ := cmd.Flags().GetInt("concurrency")
	outputDir, _ := cmd.Flags().GetString(flagOutputDir)

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
		// 审查 C1：云任务原始文件用 kind=cloud_task + <taskID>/<file>（服务端
		// 校验任务 owner 后拼接内部路径，普通下载不开放 .__ 路径访问）。
		remotePath := subID + "/" + task.Filename
		items = append(items, client.DownloadItem{RemotePath: remotePath, LocalPath: filepath.Join(outputDir, task.Filename), Kind: client.DownloadKindCloudTask})
	}

	ios.WriteOutLine("下载组 %s 中 %d 个子任务原始文件...", groupID, len(items))
	opts := []client.DownloadOption{client.WithDownloadConcurrency(concurrency)}
	if err := svc.DownloadItems(cmd.Context(), items, opts...); err != nil {
		return fmt.Errorf("批量下载失败: %w", err)
	}
	ios.WriteOutLine("  ✓ 全部下载完成")
	return nil
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
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			concurrency, _ := cmd.Flags().GetInt("concurrency")
			outputDir, _ := cmd.Flags().GetString(flagOutputDir)

			items := make([]client.DownloadItem, 0, len(args))
			for _, archiveFile := range args {
				local := filepath.Join(outputDir, filepath.Base(archiveFile))
				// 归档存服务端租户 archive 桶；download-archive 传归档名 + kind=cloud_archive
				// （内部桶路径不直接透传）。
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
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			chainID := args[0]
			ios.WriteOutLine("恢复组链式操作: %s", chainID)

			result, err := svc.ResumeChain(cmd.Context(), chainID)
			if err != nil {
				return fmt.Errorf("恢复组链式操作失败: %w", err)
			}

			ios.WriteOutLine("组链式操作完成!")
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

// NewCmdCloudGroupDelete 创建 delete 子命令。
func NewCmdCloudGroupDelete(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <group-id>",
		Short: "删除下载组及所有关联文件",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
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
