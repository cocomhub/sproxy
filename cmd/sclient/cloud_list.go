// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

// cloudTaskInfo 表示一个云端下载任务的信息（与 cloud_download.go 中的 cloudTaskResponse 结构一致）。
type cloudTaskInfo = cloudTaskResponse

var cloudListCmd = &cobra.Command{
	Use:   "list",
	Short: "列出所有云端下载任务",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		statusFilter, _ := cmd.Flags().GetString("status")

		serverURL, authToken := getCloudServerURL(cmd)
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

		fm := buildFormatter(cmd)
		if len(result.Tasks) == 0 {
			fm.Println("暂无云端下载任务")
			return nil
		}

		fm.PrintCloudTaskList(result.Tasks)
		return nil
	},
}

// getCloudServerURL 从 flag 和配置中获取 server URL 和 auth token。
// 与 cloud_download.go 中的 getCloudServerURL 共享逻辑。
func getCloudServerURL(cmd *cobra.Command) (serverURL, authToken string) {
	// 从 root 的 persistent flags 获取（子命令的 Flags() 不包含 inherited flags）
	serverURL, _ = cmd.Root().PersistentFlags().GetString("server")
	if serverURL == "" && cfgProvider != nil {
		cfg, err := loadConfigSimple()
		if err == nil {
			serverURL = cfg.ServerURL
			authToken = cfg.AuthToken
		}
	}
	if authToken == "" {
		authToken, _ = cmd.Root().PersistentFlags().GetString("auth-token")
	}
	return
}

// loadConfigSimple 从 cfgProvider 加载配置。
func loadConfigSimple() (*configSimple, error) {
	if cfgProvider == nil {
		return nil, fmt.Errorf("配置未初始化")
	}
	var cfg configSimple
	if err := cfgProvider.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// configSimple 是配置提供者使用的结构体（仅 server_url 和 auth_token）。
type configSimple struct {
	ServerURL string `mapstructure:"server_url"`
	AuthToken string `mapstructure:"auth_token"`
}

func init() {
	cloudListCmd.Flags().String("status", "", "按状态过滤（pending/downloading/completed/failed/cancelled）")
	cloudDownloadCmd.AddCommand(cloudListCmd)
}
