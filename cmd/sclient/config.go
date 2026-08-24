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

// NewCmdConfig 创建独立的 config 命令工厂函数，接收 IOStreams 用于输出。
// cfgFile 为配置文件路径指针，用于 config set 时的回写。
func NewCmdConfig(factory clientfactory.Factory, ios cli.IOStreams, cfgFile *string, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config [show|set <key> <value>|remote]",
		Short: "配置管理",
		Long:  "查看或修改 sclient 配置。\n\n可用配置项:\n  server_url      服务器地址 (如 https://127.0.0.1:18083)\n  access_key      SproxySig 认证 AccessKey（服务端配置了 access_keys 时需要）\n  access_key_secret  SproxySig 认证 AccessKeySecret（本地密钥，仅计算签名，永不上线）\n  timeout         HTTP 超时秒数\n  tunnel_key      隧道密钥 (64 位 hex)\n  chunk_size      分块上传/下载块大小 (字节)\n  max_chunk_size  最大分块大小 (字节)\n  hub_url         mesh/relay/p2p 共用的 hub 地址 (如 wss://hub.example.com/ws)\n  node_id         本节点默认 ID (mesh/relay/p2p 信令来源；为空回落主机名)\n\n多环境：SCLIENT_ENV=prod 时默认加载 sclient.prod.yaml。",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := cfgSvc.LoadConfig()
			if err != nil {
				ios.WriteErrLine("加载配置失败: %v", err)
				return fmt.Errorf("加载配置失败: %w", err)
			}

			if len(args) == 0 || args[0] == "show" {
				client.HandleConfigShow(cfg, ios.Out)
				return nil
			}

			if args[0] == "set" {
				if len(args) < 3 {
					return fmt.Errorf("用法: sclient config set <键> <值>")
				}
				if err := client.ApplyConfigSet(cfg, args[1], args[2]); err != nil {
					ios.WriteErrLine("设置配置失败: %v", err)
					return fmt.Errorf("设置配置失败: %w", err)
				}
				if err := client.SaveConfig(cfg, *cfgFile); err != nil {
					ios.WriteErrLine("保存配置失败: %v", err)
					return fmt.Errorf("保存配置失败: %w", err)
				}
				fmt.Fprintf(ios.Out, "配置已更新: %s = %s\n", args[1], args[2])
				return nil
			}

			ios.WriteErrLine("未知的 config 子命令: %s", args[0])
			return fmt.Errorf("用法: sclient config [show|set <键> <值>|remote]")
		},
	}
	cmd.AddCommand(NewCmdConfigRemote(factory, ios))
	return cmd
}

// NewCmdConfigRemote 创建独立的 config remote 命令工厂函数。
func NewCmdConfigRemote(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "查看或修改远程服务器配置",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}

			cfg, err := svc.GetConfig(cmd.Context())
			if err != nil {
				ios.WriteErrLine("获取远程配置失败: %v", err)
				return fmt.Errorf("获取远程配置失败: %w", err)
			}
			fm := buildFormatterWithWriter(ios.Out, cmd)
			fm.PrintConfig(cfg)
			return nil
		},
	}
	cmd.AddCommand(NewCmdConfigRemoteSet(factory, ios))
	return cmd
}

// NewCmdConfigRemoteSet 创建独立的 config remote set 命令工厂函数。
func NewCmdConfigRemoteSet(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "更新远程服务器运行时配置",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			key := args[0]
			value := args[1]
			updates := map[string]any{key: value}
			if err := svc.UpdateConfig(cmd.Context(), updates); err != nil {
				ios.WriteErrLine("更新远程配置失败: %v", err)
				return fmt.Errorf("更新远程配置失败: %w", err)
			}
			fmt.Fprintf(ios.Out, "远程配置已更新: %s = %s\n", key, value)
			return nil
		},
	}
}
