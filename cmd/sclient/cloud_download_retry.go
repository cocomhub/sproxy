// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdCloudDownloadRetry 创建 `cloud-download retry`：按条件重试云端下载任务。
//
// 为什么需要（2026-11-21）：失败任务此前默认 1h 即被清理，且重试只能按 task id ——
// 任务消失后完全无从下手。本命令支持按 **url / 转存路径 / 组** 重试，不依赖历史 task id。
func NewCmdCloudDownloadRetry(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var sel client.CloudRetrySelector
	cmd := &cobra.Command{SilenceUsage: true, SilenceErrors: true,
		Use:   "retry",
		Short: "按条件重试云端下载任务（task-id / url / 转存路径 / 组）",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringVar(&sel.TaskID, "task-id", "", "按任务 ID 重试")
	cmd.Flags().StringVar(&sel.URL, "url", "", "按源 URL 子串匹配重试")
	cmd.Flags().StringVar(&sel.TransferPath, "transfer-path", "", "按转存目标路径精确匹配重试")
	cmd.Flags().StringVar(&sel.GroupID, "group-id", "", "按任务组重试")
	cmd.Flags().BoolVar(&sel.Force, "force", false, "强制重试（透传 resume 的 force）")
	cmd.Flags().IntVar(&sel.Limit, "limit", 0, "最多重试数量（0 = 不限）")
	cmd.Flags().BoolVar(&sel.IncludeCompletedDamaged, "include-completed-damaged", true,
		"把 completed 且完整性 damaged 的任务也纳入重试（重下自愈）")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, cerr := factory.NewClient(cmd)
		if cerr != nil {
			return cerr
		}
		res, err := c.RetryCloudTasks(cmd.Context(), sel)
		if err != nil {
			return err
		}
		ios.WriteOutLine("已提交重试 %d 个任务", len(res.Retried))
		for _, id := range res.Retried {
			ios.WriteOutLine("  %s", id)
		}
		if len(res.Errors) > 0 {
			ios.WriteErrLine("失败 %d 个：", len(res.Errors))
			for id, msg := range res.Errors {
				ios.WriteErrLine("  %s: %s", id, msg)
			}
			return fmt.Errorf("%d 个任务未能重试", len(res.Errors))
		}
		return nil
	}
	return cmd
}
