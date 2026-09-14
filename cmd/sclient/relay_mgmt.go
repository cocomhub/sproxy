// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// getHubServerURL 从 flag 和配置中获取 Hub 服务器地址与 SproxySig 认证 AK/SK。
//
// 解析顺序：根 `--server` > 本命令 `--hub`（`ws(s)://…` → `http(s)://host:port`）> 配置 `server_url`；
// 凭据：配置 `access_key*` 优先，其次根 `--access-key` / `--access-key-secret` / `--access-key-id`。
func getHubServerURL(cmd *cobra.Command, cfgSvc ConfigProvider) (serverURL, accessKey, accessKeySecret, accessKeyID string) {
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

// bindHubAsServer 把解析出的 Hub 地址固定到根 `--server` 持久 flag，使 relay 系列能走统一的
// `factory.NewClient`（凭据 / TLS / `--insecure` / 卷上下文都由工厂装配），而不再在 cmd 内自建
// HTTP + 手写 SproxySig。
//
// 语义保持：把地址写成「显式 --server」⇒ 工厂的 `serverFlagNotSet=false` ⇒ **不走隧道**，与旧实现
// 一致（旧 relay 管理路径就是直连 HTTP + SproxySig 签名）。
func bindHubAsServer(cmd *cobra.Command, cfgSvc ConfigProvider) {
	root := cmd.Root()
	if root == nil || root.PersistentFlags().Lookup("server") == nil {
		return // 单命令测试环境没有根 persistent flag，交给注入的 factory
	}
	if v, _ := root.PersistentFlags().GetString("server"); v != "" {
		return
	}
	if serverURL, _, _, _ := getHubServerURL(cmd, cfgSvc); serverURL != "" {
		_ = root.PersistentFlags().Set("server", serverURL)
	}
}

// relayHubClient 是 relay 管理子命令的公共前置：解析 Hub 地址（`--server` > `--hub` > 配置），
// 固定到 `--server` 后经工厂构造客户端；地址缺失时给出与历史一致的提示。
func relayHubClient(cmd *cobra.Command, factory clientfactory.Factory, cfgSvc ConfigProvider) (*client.FileClient, error) {
	if serverURL, _, _, _ := getHubServerURL(cmd, cfgSvc); serverURL == "" {
		return nil, fmt.Errorf("未指定服务器地址，请使用 --server 或 --hub 或配置 server_url")
	}
	bindHubAsServer(cmd, cfgSvc)
	svc, err := factory.NewClient(cmd)
	if err != nil {
		return nil, fmt.Errorf(errFmtInitClient, err)
	}
	return svc, nil
}

// NewCmdRelayRemoveNode 创建 relay remove-node 命令的工厂函数。
func NewCmdRelayRemoveNode(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-node <node-id>",
		Short: "从 Hub 移除指定节点",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeID := args[0]

			svc, err := relayHubClient(cmd, factory, cfgSvc)
			if err != nil {
				return err
			}
			if err := svc.RemoveHubNode(cmd.Context(), nodeID); err != nil {
				// 404 → 与历史一致的用户文案（SDK 以 ErrNotFound 哨兵上报，不做字符串匹配）。
				if errors.Is(err, client.ErrNotFound) {
					return fmt.Errorf("节点 %s 不存在", nodeID)
				}
				return fmt.Errorf("移除节点失败: %w", err)
			}

			ios.WriteOutLine("已移除节点: %s", nodeID)
			return nil
		},
	}

	cmd.Flags().String("hub", "", "Hub 的 HTTP 地址 (如 http://127.0.0.1:18083)")

	return cmd
}

// NewCmdRelayStats 创建 relay stats 命令的工厂函数。
func NewCmdRelayStats(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "查看 Hub 统计信息",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := relayHubClient(cmd, factory, cfgSvc)
			if err != nil {
				return err
			}
			stats, err := svc.GetHubStats(cmd.Context())
			if err != nil {
				return fmt.Errorf("获取 Hub 统计失败: %w", err)
			}
			ios.WriteOutLine("Hub 已连接节点数: %d", stats.NodesConnected)
			return nil
		},
	}

	cmd.Flags().String("hub", "", "Hub 的 HTTP 地址 (如 http://127.0.0.1:18083)")

	return cmd
}
