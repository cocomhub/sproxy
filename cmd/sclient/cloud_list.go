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

// cloudTaskInfo 直接复用 client.CloudTask（服务端返回的完整字段，含 ETag/GroupID/FileMTime/时间戳）。
type cloudTaskInfo = client.CloudTask

// getCloudServerURL 从 flag 和配置中获取 server URL 与 SproxySig 认证 AccessKey/SK。
// 被 cloud_cancel.go 和 preview.go 共享使用。
func getCloudServerURL(cmd *cobra.Command, cfgSvc ConfigProvider) (serverURL, accessKey, accessKeySecret, accessKeyID string) {
	serverURL, _ = cmd.Root().PersistentFlags().GetString("server")
	if serverURL == "" && cfgSvc != nil {
		if cfg, err := cfgSvc.LoadConfig(); err == nil {
			serverURL = cfg.ServerURL
			accessKey = cfg.AccessKey
			accessKeySecret = cfg.AccessKeySecret
			accessKeyID = cfg.AccessKeyID
		}
	}
	if accessKeySecret == "" {
		accessKey, _ = cmd.Root().PersistentFlags().GetString("access-key")
		accessKeySecret, _ = cmd.Root().PersistentFlags().GetString("access-key-secret")
		accessKeyID, _ = cmd.Root().PersistentFlags().GetString("access-key-id")
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
			offset, _ := cmd.Flags().GetInt("offset")
			limit, _ := cmd.Flags().GetInt("limit")
			tasks, total, err := svc.ListCloudTasksWithTotal(cmd.Context(), statusFilter, offset, limit)
			if err != nil {
				return fmt.Errorf("获取云端下载任务列表失败: %w", err)
			}

			fm := buildFormatterWithWriter(ios.Out, cmd)
			// 分页时展示总数（total=0 时不显示，避免每次都在空列表旁打印 0）
			if total > 0 && (offset > 0 || limit > 0) {
				fm.Printf("云任务总数: %d", total)
			}
			if len(tasks) == 0 {
				fm.Println("暂无云端下载任务")
				return nil
			}

			// client.CloudTask 即 cloudTaskInfo（类型别名），直接透传完整字段
			fm.PrintCloudTaskList(tasks)
			return nil
		},
	}

	cmd.Flags().String("status", "", "按状态过滤（pending/downloading/completed/failed/cancelled）")
	cmd.Flags().Int("offset", -1, "跳过前 N 条（默认 -1 不偏移）")
	cmd.Flags().Int("limit", 0, "返回条数上限（默认 0 返回全部）")

	return cmd
}
