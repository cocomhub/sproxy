// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdCloudArchive 创建 cloud-archive 命令的工厂函数。
func NewCmdCloudArchive(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive <task-id> [task-id...]",
		Short: "打包云端下载已完成的任务文件",
		Long: `将指定已完成云端下载任务的文件打包为 tar.gz 并存放到服务端 uploads 目录。

支持单个或多个任务 ID。使用 --name 指定归档文件名。`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			archiveName, _ := cmd.Flags().GetString("name")

			if len(args) == 1 {
				result, err := svc.ArchiveCloudTask(cmd.Context(), args[0], archiveName)
				if err != nil {
					ios.WriteErrLine("归档失败: %v", err)
					return fmt.Errorf("归档失败: %w", err)
				}
				ios.WriteOutLine("归档完成: %s (%d bytes)", result.File, result.Size)
			} else {
				result, err := svc.ArchiveCloudTasks(cmd.Context(), args, archiveName)
				if err != nil {
					ios.WriteErrLine("归档失败: %v", err)
					return fmt.Errorf("归档失败: %w", err)
				}
				ios.WriteOutLine("归档完成: %s (%d bytes, %d files)", result.File, result.Size, result.TaskCount)
			}
			return nil
		},
	}

	cmd.Flags().String("name", "", "归档文件名（默认自动生成）")

	return cmd
}
