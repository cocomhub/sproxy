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

var cloudCancelCmd = &cobra.Command{
	Use:   "cancel <task-id>",
	Short: "取消云端下载任务",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		taskID := args[0]

		serverURL, authToken := getCloudServerURL(cmd)
		if serverURL == "" {
			return fmt.Errorf("未指定服务器地址，请使用 --server 或配置 server_url")
		}

		apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID) + "/cancel"
		req, err := http.NewRequest(http.MethodPost, serverURL+apiPath, nil)
		if err != nil {
			return fmt.Errorf("创建请求失败: %w", err)
		}
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("取消云端下载任务失败: %w", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

		fm := buildFormatter(cmd)

		if resp.StatusCode == http.StatusNotFound {
			fm.PrintCloudTaskCancelResult(taskID, false, "任务不存在")
			return nil
		}

		if resp.StatusCode != http.StatusOK {
			fm.PrintCloudTaskCancelResult(taskID, false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
			return nil
		}

		var result struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			fm.PrintCloudTaskCancelResult(taskID, false, "解析响应失败")
			return nil
		}

		fm.PrintCloudTaskCancelResult(taskID, result.Success, result.Message)
		return nil
	},
}

// NewCmdCloudCancel 创建 cloud cancel 命令的工厂函数。
func NewCmdCloudCancel(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <task-id>",
		Short: "取消云端下载任务",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID := args[0]

			serverURL, authToken := getCloudServerURL(cmd)
			if serverURL == "" {
				return fmt.Errorf("未指定服务器地址，请使用 --server 或配置 server_url")
			}

			apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID) + "/cancel"
			req, err := http.NewRequest(http.MethodPost, serverURL+apiPath, nil)
			if err != nil {
				return fmt.Errorf("创建请求失败: %w", err)
			}
			if authToken != "" {
				req.Header.Set("Authorization", "Bearer "+authToken)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("取消云端下载任务失败: %w", err)
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

			fm := buildFormatterWithWriter(ios.Out, cmd)

			if resp.StatusCode == http.StatusNotFound {
				fm.PrintCloudTaskCancelResult(taskID, false, "任务不存在")
				return nil
			}

			if resp.StatusCode != http.StatusOK {
				fm.PrintCloudTaskCancelResult(taskID, false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)))
				return nil
			}

			var result struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				fm.PrintCloudTaskCancelResult(taskID, false, "解析响应失败")
				return nil
			}

			fm.PrintCloudTaskCancelResult(taskID, result.Success, result.Message)
			return nil
		},
	}
}
