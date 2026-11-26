// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdCloudDownloadActions 创建 `cloud-download actions`：列出任务当前可用的处理动作。
//
// 为什么需要（用户 2026-11-26）：动作可用性由服务端判定（产物是否完整 / 是否有备选源），
// CLI 用户需要先看清"哪些动作可用、为什么不可用"，再执行 `cloud-download action`。
func NewCmdCloudDownloadActions(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{SilenceUsage: true, SilenceErrors: true,
		Use:   "actions <task-id>",
		Short: "列出任务可用的处理动作（含不可用原因）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cerr := factory.NewClient(cmd)
			if cerr != nil {
				return cerr
			}
			task, err := c.GetCloudTask(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(task.Actions) == 0 {
				fmt.Fprintf(ios.Out, "任务 %s（status=%s）当前没有可执行动作\n", task.ID, task.Status)
				return nil
			}
			tw := tabwriter.NewWriter(ios.Out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "动作\t说明\t可用\t原因\t参数")
			for _, a := range task.Actions {
				ok := "是"
				if a.Disabled {
					ok = "否"
				}
				need := ""
				if a.NeedsURL {
					need = "需要 --url"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Action, a.Label+"（"+a.Hint+"）", ok, a.Reason, need)
			}
			return tw.Flush()
		},
	}
}
