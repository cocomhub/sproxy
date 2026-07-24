// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config [show|set <key> <value>]",
	Short: "配置管理",
	Long:  "查看或修改 sclient 配置。\n\n可用配置项:\n  server_url      服务器地址 (如 http://localhost:18083)\n  auth_token      Bearer Token 认证令牌\n  timeout         HTTP 超时秒数\n  tunnel_key      隧道密钥 (64 位 hex)\n  chunk_size      分块上传/下载块大小 (字节)\n  max_chunk_size  最大分块大小 (字节)",
	Args:  cobra.MaximumNArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		if cfgProvider == nil {
			return fmt.Errorf("配置未初始化，请通过 sclient --config 指定配置文件")
		}
		cfg, err := client.LoadFromProvider(cfgProvider)
		if err != nil {
			fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
			return fmt.Errorf("加载配置失败: %w", err)
		}

		if len(args) == 0 {
			client.HandleConfigShow(cfg)
			return nil
		}

		switch args[0] {
		case "show":
			client.HandleConfigShow(cfg)
		case "set":
			if len(args) < 3 {
				return fmt.Errorf("用法: sclient config set <键> <值>")
			}
			if err := client.HandleConfigSet(cfg, cfgFile, args[1], args[2]); err != nil {
				fmt.Fprintf(os.Stderr, "设置配置失败: %v\n", err)
				return fmt.Errorf("设置配置失败: %w", err)
			}
			fmt.Printf("配置已更新: %s = %s\n", args[1], args[2])
		default:
			fmt.Fprintf(os.Stderr, "未知的 config 子命令: %s\n", args[0])
			return fmt.Errorf("用法: sclient config [show|set <键> <值>]")
		}
		return nil
	},
}
