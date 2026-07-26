// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdCloudCancel 创建 cloud cancel 命令的工厂函数。
func NewCmdCloudCancel(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <task-id>",
		Short: "取消云端下载任务",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			taskID := args[0]
			fm := buildFormatterWithWriter(ios.Out, cmd)

			if err := svc.CancelCloudTask(cmd.Context(), taskID); err != nil {
				fm.PrintCloudTaskCancelResult(taskID, false, err.Error())
				// 任务不存在时保持旧行为：不返回错误，仅打印消息
				return nil
			}

			fm.PrintCloudTaskCancelResult(taskID, true, "已取消")
			return nil
		},
	}

	return cmd
}
