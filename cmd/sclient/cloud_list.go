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

// cloudTaskInfo 表示一个云端下载任务的信息（与 cloud_types.go 中的 cloudTaskResponse 结构一致）。
type cloudTaskInfo = cloudTaskResponse

// getCloudServerURL 从 flag 和配置中获取 server URL 和 auth token。
// 被 cloud_cancel.go 和 preview.go 共享使用。
func getCloudServerURL(cmd *cobra.Command, cfgSvc ConfigProvider) (serverURL, authToken string) {
	serverURL, _ = cmd.Root().PersistentFlags().GetString("server")
	if serverURL == "" && cfgSvc != nil {
		if cfg, err := cfgSvc.LoadConfig(); err == nil {
			serverURL = cfg.ServerURL
			authToken = cfg.AuthToken
		}
	}
	if authToken == "" {
		authToken, _ = cmd.Root().PersistentFlags().GetString("auth-token")
	}
	return
}

// NewCmdCloudList 创建 cloud list 命令的工厂函数。
func NewCmdCloudList(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出所有云端下载任务",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			statusFilter, _ := cmd.Flags().GetString("status")
			tasks, err := svc.ListCloudTasks(cmd.Context(), statusFilter)
			if err != nil {
				return fmt.Errorf("获取云端下载任务列表失败: %w", err)
			}

			fm := buildFormatterWithWriter(ios.Out, cmd)
			if len(tasks) == 0 {
				fm.Println("暂无云端下载任务")
				return nil
			}

			// 转换为 cloudTaskInfo 用于格式化输出
			infos := make([]cloudTaskInfo, len(tasks))
			for i, t := range tasks {
				infos[i] = cloudTaskToInfo(t)
			}
			fm.PrintCloudTaskList(infos)
			return nil
		},
	}

	cmd.Flags().String("status", "", "按状态过滤（pending/downloading/completed/failed/cancelled）")

	return cmd
}

// cloudTaskToInfo 将 client.CloudTask 转换为 cloudTaskInfo。
func cloudTaskToInfo(t client.CloudTask) cloudTaskInfo {
	return cloudTaskInfo{
		ID:         t.ID,
		URL:        t.URL,
		Filename:   t.Filename,
		Status:     t.Status,
		TotalSize:  t.TotalSize,
		Downloaded: t.Downloaded,
		Checksum:   t.Checksum,
		Error:      t.Error,
	}
}

// 确保 cloudTaskResponse 的字段与 client.CloudTask 兼容
var _ = cloudTaskToInfo // 使用引用避免未使用错误
