// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var configCmd = &cobra.Command{
	Use:   "config [show|set <key> <value>]",
	Short: "配置管理",
	Long:  "查看或修改 sclient 配置。\n\n可用配置项:\n  server_url      服务器地址 (如 http://localhost:18083)\n  check_checksum  是否启用 SHA-256 校验 (true/false)\n  timeout         HTTP 超时秒数\n  tunnel_key      隧道密钥 (64 位 hex)\n  chunk_size      分块上传/下载块大小 (字节)\n  max_chunk_size  最大分块大小 (字节)",
	Args:  cobra.MaximumNArgs(3),
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := client.LoadFromViper(viper.GetViper())
		if err != nil {
			fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
			os.Exit(1)
		}

		if len(args) == 0 {
			client.HandleConfigShow(cfg)
			return
		}

		switch args[0] {
		case "show":
			client.HandleConfigShow(cfg)
		case "set":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "用法: sclient config set <键> <值>")
				os.Exit(1)
			}
			if err := client.HandleConfigSet(cfg, cfgFile, args[1], args[2]); err != nil {
				fmt.Fprintf(os.Stderr, "设置配置失败: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("配置已更新: %s = %s\n", args[1], args[2])
		default:
			fmt.Fprintf(os.Stderr, "未知的 config 子命令: %s\n", args[0])
			fmt.Fprintln(os.Stderr, "用法: sclient config [show|set <键> <值>]")
			os.Exit(1)
		}
	},
}
