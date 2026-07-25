// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// cloudTaskInfo 表示一个云端下载任务的信息（与 cloud_download.go 中的 cloudTaskResponse 结构一致）。
type cloudTaskInfo = cloudTaskResponse

// getCloudServerURL 从 flag 和配置中获取 server URL 和 auth token。
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
			statusFilter, _ := cmd.Flags().GetString("status")

			serverURL, authToken := getCloudServerURL(cmd, cfgSvc)
			if serverURL == "" {
				return fmt.Errorf("未指定服务器地址，请使用 --server 或配置 server_url")
			}

			apiPath := "/api/cloud/tasks"
			if statusFilter != "" {
				apiPath += "?status=" + url.QueryEscape(statusFilter)
			}

			req, err := http.NewRequest(http.MethodGet, serverURL+apiPath, nil)
			if err != nil {
				return fmt.Errorf("创建请求失败: %w", err)
			}
			if authToken != "" {
				req.Header.Set("Authorization", "Bearer "+authToken)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("获取云端下载任务列表失败: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
				return fmt.Errorf("获取云端下载任务列表失败 (HTTP %d): %s", resp.StatusCode, string(body))
			}

			var result struct {
				Tasks []cloudTaskInfo `json:"tasks"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				return fmt.Errorf("解析响应失败: %w", err)
			}

			fm := buildFormatterWithWriter(ios.Out, cmd)
			if len(result.Tasks) == 0 {
				fm.Println("暂无云端下载任务")
				return nil
			}

			fm.PrintCloudTaskList(result.Tasks)
			return nil
		},
	}

	cmd.Flags().String("status", "", "按状态过滤（pending/downloading/completed/failed/cancelled）")

	return cmd
}
