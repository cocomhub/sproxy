// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// hubNode 表示 Hub 节点信息。
type hubNode struct {
	ID        string `json:"id"`
	Addr      string `json:"addr,omitempty"`
	Connected string `json:"connected,omitempty"`
}

var relayRemoveNodeCmd = &cobra.Command{
	Use:   "remove-node <node-id>",
	Short: "从 Hub 移除指定节点",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		nodeID := args[0]

		serverURL, authToken := getHubServerURL(cmd)
		if serverURL == "" {
			return fmt.Errorf("未指定服务器地址，请使用 --server 或 --hub 或配置 server_url")
		}

		apiPath := "/api/hub/nodes/" + url.PathEscape(nodeID)
		req, err := http.NewRequest(http.MethodDelete, serverURL+apiPath, nil)
		if err != nil {
			return fmt.Errorf("创建请求失败: %w", err)
		}
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}

		httpClient := &http.Client{Timeout: 10 * time.Second}
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("移除节点失败: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("节点 %s 不存在", nodeID)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			return fmt.Errorf("移除节点失败 (HTTP %d): %s", resp.StatusCode, string(body))
		}

		fmt.Printf("已移除节点: %s\n", nodeID)
		return nil
	},
}

var relayStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "查看 Hub 统计信息",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		serverURL, authToken := getHubServerURL(cmd)
		if serverURL == "" {
			return fmt.Errorf("未指定服务器地址，请使用 --server 或 --hub 或配置 server_url")
		}

		apiPath := "/api/hub/stats"
		req, err := http.NewRequest(http.MethodGet, serverURL+apiPath, nil)
		if err != nil {
			return fmt.Errorf("创建请求失败: %w", err)
		}
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}

		httpClient := &http.Client{Timeout: 10 * time.Second}
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("获取 Hub 统计失败: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			return fmt.Errorf("获取 Hub 统计失败 (HTTP %d): %s", resp.StatusCode, string(body))
		}

		var stats struct {
			NodeCount int `json:"node_count"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
			return fmt.Errorf("解析响应失败: %w", err)
		}

		fmt.Printf("Hub 已连接节点数: %d\n", stats.NodeCount)
		return nil
	},
}

// getHubServerURL 从 flag 和配置中获取 Hub 服务器地址和 auth token。
func getHubServerURL(cmd *cobra.Command) (serverURL, authToken string) {
	serverURL, _ = cmd.Root().PersistentFlags().GetString("server")
	if serverURL == "" {
		if hubURL, _ := cmd.Flags().GetString("hub"); hubURL != "" {
			// ws://host:port/path -> http://host:port
			if u, parseErr := url.Parse(hubURL); parseErr == nil {
				u.Scheme = "http"
				u.Path = ""
				serverURL = u.String()
			}
		}
	}
	if serverURL == "" && cfgProvider != nil {
		cfg, _ := loadConfigSimple()
		if cfg != nil {
			serverURL = cfg.ServerURL
			authToken = cfg.AuthToken
		}
	}
	if authToken == "" {
		authToken, _ = cmd.Root().PersistentFlags().GetString("auth-token")
	}
	return
}

func init() {
	relayRemoveNodeCmd.Flags().String("hub", "", "Hub 的 HTTP 地址 (如 http://127.0.0.1:18083)")
	relayStatsCmd.Flags().String("hub", "", "Hub 的 HTTP 地址 (如 http://127.0.0.1:18083)")

	relayCmd.AddCommand(relayRemoveNodeCmd)
	relayCmd.AddCommand(relayStatsCmd)
}

// NewCmdRelayRemoveNode 创建 relay remove-node 命令的工厂函数。
func NewCmdRelayRemoveNode(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-node <node-id>",
		Short: "从 Hub 移除指定节点",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeID := args[0]

			serverURL, authToken := getHubServerURL(cmd)
			if serverURL == "" {
				return fmt.Errorf("未指定服务器地址，请使用 --server 或 --hub 或配置 server_url")
			}

			apiPath := "/api/hub/nodes/" + url.PathEscape(nodeID)
			req, err := http.NewRequest(http.MethodDelete, serverURL+apiPath, nil)
			if err != nil {
				return fmt.Errorf("创建请求失败: %w", err)
			}
			if authToken != "" {
				req.Header.Set("Authorization", "Bearer "+authToken)
			}

			httpClient := &http.Client{Timeout: 10 * time.Second}
			resp, err := httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("移除节点失败: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusNotFound {
				return fmt.Errorf("节点 %s 不存在", nodeID)
			}
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
				return fmt.Errorf("移除节点失败 (HTTP %d): %s", resp.StatusCode, string(body))
			}

			ios.WriteOutLine("已移除节点: %s", nodeID)
			return nil
		},
	}

	cmd.Flags().String("hub", "", "Hub 的 HTTP 地址 (如 http://127.0.0.1:18083)")

	return cmd
}

// NewCmdRelayStats 创建 relay stats 命令的工厂函数。
func NewCmdRelayStats(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "查看 Hub 统计信息",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			serverURL, authToken := getHubServerURL(cmd)
			if serverURL == "" {
				return fmt.Errorf("未指定服务器地址，请使用 --server 或 --hub 或配置 server_url")
			}

			apiPath := "/api/hub/stats"
			req, err := http.NewRequest(http.MethodGet, serverURL+apiPath, nil)
			if err != nil {
				return fmt.Errorf("创建请求失败: %w", err)
			}
			if authToken != "" {
				req.Header.Set("Authorization", "Bearer "+authToken)
			}

			httpClient := &http.Client{Timeout: 10 * time.Second}
			resp, err := httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("获取 Hub 统计失败: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
				return fmt.Errorf("获取 Hub 统计失败 (HTTP %d): %s", resp.StatusCode, string(body))
			}

			var stats struct {
				NodeCount int `json:"node_count"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
				return fmt.Errorf("解析响应失败: %w", err)
			}

			ios.WriteOutLine("Hub 已连接节点数: %d", stats.NodeCount)
			return nil
		},
	}

	cmd.Flags().String("hub", "", "Hub 的 HTTP 地址 (如 http://127.0.0.1:18083)")

	return cmd
}
