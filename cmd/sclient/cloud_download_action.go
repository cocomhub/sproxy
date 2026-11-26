// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdCloudDownloadAction 创建 `cloud-download action`：对失败任务执行细粒度处理。
//
// 支持单条与批量（--ids / --all-failed）；retry_from_url 用 --url 提供新源。
func NewCmdCloudDownloadAction(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var (
		newURL    string
		idsFlag   string
		allFailed bool
		status    string
	)
	cmd := &cobra.Command{SilenceUsage: true, SilenceErrors: true,
		Use:   "action [task-id] <action>",
		Short: "对失败任务执行细粒度处理（续传下载/仅重做转存/覆盖转存/换链接/换源/清残留）",
		Long: `对失败或已取消的云端下载任务执行细粒度处理。

可用动作：
  retry_download            退回下载阶段续传（保留断点，不重下已完成块）
  retry_transfer            仅重做转存（要求产物已完整且配置了转存目标）
  retry_transfer_overwrite  仅重做转存并覆盖已存在的目标（显式 opt-in）
  retry_from_url            用新的源 URL 重试（需 --url；保留断点）
  rotate_source             切换到下一个 fallback 源重新下载
  discard_partial           删除已有产物与断点，从零重新下载

批量：--ids a,b,c 或 --all-failed（服务端串行执行，单次最多 50 个）。`,
		Args: cobra.RangeArgs(1, 2),
	}
	cmd.Flags().StringVar(&newURL, "url", "", "retry_from_url 的新源 URL")
	cmd.Flags().StringVar(&idsFlag, "ids", "", "批量：逗号分隔的任务 ID")
	cmd.Flags().BoolVar(&allFailed, "all-failed", false, "批量：对所有失败/取消任务执行该动作")
	cmd.Flags().StringVar(&status, "status", "", "配合 --all-failed：只处理该状态（failed/cancelled）")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, cerr := factory.NewClient(cmd)
		if cerr != nil {
			return cerr
		}
		if len(args) == 2 {
			return runSingleTaskAction(cmd, c, ios, args[0], args[1], newURL)
		}
		return runBatchTaskAction(cmd, c, ios, args[0], newURL, splitCSV(idsFlag), allFailed, status)
	}
	return cmd
}

// runSingleTaskAction 单条动作（task-id + action）。
func runSingleTaskAction(cmd *cobra.Command, c *client.FileClient, ios cli.IOStreams, taskID, action, newURL string) error {
	task, err := c.ApplyCloudTaskActionWithURL(cmd.Context(), taskID, action, newURL)
	if err != nil {
		return err
	}
	fmt.Fprintf(ios.Out, "任务 %s：动作 %s 已提交（status=%s, downloaded=%d/%d）\n",
		task.ID, action, task.Status, task.Downloaded, task.TotalSize)
	return nil
}

// runBatchTaskAction 批量动作（--ids / --all-failed）。
func runBatchTaskAction(cmd *cobra.Command, c *client.FileClient, ios cli.IOStreams,
	action, newURL string, ids []string, allFailed bool, status string) error {
	if allFailed {
		tasks, _, lerr := c.ListCloudTasksWithTotal(cmd.Context(), status, 0, 0)
		if lerr != nil {
			return lerr
		}
		for _, t := range tasks {
			if t.Status == client.TaskStatusFailed || t.Status == client.TaskStatusCancelled {
				ids = append(ids, t.ID)
			}
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("批量执行需要 --ids 或 --all-failed（且存在匹配任务）")
	}
	res, err := c.ApplyCloudTaskActionsBatch(cmd.Context(), action, newURL, ids)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(ios.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "动作 %s：成功 %d，失败 %d\n", res.Action, res.Succeeded, res.Failed)
	for _, r := range res.Results {
		state, detail := "ok", ""
		if !r.OK {
			state, detail = "failed", r.Error
		} else if r.Task != nil {
			detail = r.Task.Status
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.TaskID, state, detail)
	}
	return tw.Flush()
}

// splitCSV 拆分逗号分隔串（去空白/空项）。
func splitCSV(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
