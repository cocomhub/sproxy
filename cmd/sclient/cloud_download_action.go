// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdCloudDownloadAction 创建 `cloud-download action`：对失败任务执行细粒度处理。
//
// 为什么需要（用户 2026-11-26）：失败任务此前只有「全量重试」——下载阶段失败但已下大半的
// 任务被迫重下，转存阶段失败的任务又被要求重下。这里按意图拆开：
//
//	retry_download   退回下载阶段续传（保留断点）
//	retry_transfer   仅重做转存（产物已完整）
//	rotate_source    换下一个备选源重下
//	discard_partial  清残留（产物 + 断点）后从零重下
func NewCmdCloudDownloadAction(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{SilenceUsage: true, SilenceErrors: true,
		Use:   "action <task-id> <action>",
		Short: "对失败任务执行细粒度处理（续传下载/仅重做转存/换源/清残留重下）",
		Long: `对失败或已取消的云端下载任务执行细粒度处理。

可用动作：
  retry_download    退回下载阶段续传（保留 .partial 与 hybrid 断点，不重下已完成块）
  retry_transfer    仅重做转存（要求产物已完整且配置了转存目标）
  rotate_source     切换到下一个 fallback 源重新下载
  discard_partial   删除已有产物与断点，从零重新下载`,
		Args: cobra.ExactArgs(2),
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, cerr := factory.NewClient(cmd)
		if cerr != nil {
			return cerr
		}
		task, err := c.ApplyCloudTaskAction(cmd.Context(), args[0], args[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(ios.Out, "任务 %s：动作 %s 已提交（status=%s, downloaded=%d/%d）\n",
			task.ID, args[1], task.Status, task.Downloaded, task.TotalSize)
		return nil
	}
	return cmd
}
