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
	RunE: func(cmd *cobra.Command, args []string) error {
		if cfgProvider == nil {
			return fmt.Errorf("配置未初始化，请通过 sclient --config 指定配置文件")
		}
		cfg, err := client.LoadFromProvider(cfgProvider)
		if err != nil {
			fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
			return fmt.Errorf("加载配置失败: %w", err)
		}

		if len(args) == 0 || args[0] == "show" {
			client.HandleConfigShow(cfg)
			return nil
		}

		if args[0] == "set" {
			if len(args) < 3 {
				return fmt.Errorf("用法: sclient config set <键> <值>")
			}
			if err := client.HandleConfigSet(cfg, cfgFile, args[1], args[2]); err != nil {
				fmt.Fprintf(os.Stderr, "设置配置失败: %v\n", err)
				return fmt.Errorf("设置配置失败: %w", err)
			}
			fmt.Printf("配置已更新: %s = %s\n", args[1], args[2])
			return nil
		}

		if args[0] == "remote" {
			_ = cmd.Help()
			return nil
		}

		fmt.Fprintf(os.Stderr, "未知的 config 子命令: %s\n", args[0])
		return fmt.Errorf("用法: sclient config [show|set <键> <值>|remote]")
	},
}

var configRemoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "查看或修改远程服务器配置",
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}

		cfg, err := cli.GetConfig(cmd.Context())
		if err != nil {
			fmt.Fprintf(os.Stderr, "获取远程配置失败: %v\n", err)
			return fmt.Errorf("获取远程配置失败: %w", err)
		}

		fmt.Printf("远程服务器配置:\n")
		fmt.Printf("  log_level:              %s\n", cfg.LogLevel)
		fmt.Printf("  log_format:             %s\n", cfg.LogFormat)
		fmt.Printf("  auth_token:             %s\n", boolStr(cfg.AuthTokenSet))
		fmt.Printf("  tunnel_key:             %s\n", boolStr(cfg.TunnelKeySet))
		fmt.Printf("  rate_limit_requests:    %d\n", cfg.RateLimitRequests)
		fmt.Printf("  rate_limit_window:      %s\n", cfg.RateLimitWindow)
		fmt.Printf("  max_storage_bytes:      %d\n", cfg.MaxStorageBytes)
		fmt.Printf("  chunk_size:             %d\n", cfg.ChunkSize)
		fmt.Printf("  upload_session_ttl:     %s\n", cfg.UploadSessionTTL)
		fmt.Printf("  versioning_enabled:     %v\n", cfg.VersioningEnabled)
		fmt.Printf("  cloud_max_concurrent:   %d\n", cfg.CloudMaxConcurrent)
		fmt.Printf("  addr:                   %s\n", cfg.Addr)
		fmt.Printf("  uploads_dir:            %s\n", cfg.UploadsDir)
		return nil
	},
}

var configRemoteSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "更新远程服务器运行时配置",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}

		key := args[0]
		value := args[1]

		updates := map[string]interface{}{key: value}
		if err := cli.UpdateConfig(cmd.Context(), updates); err != nil {
			fmt.Fprintf(os.Stderr, "更新远程配置失败: %v\n", err)
			return fmt.Errorf("更新远程配置失败: %w", err)
		}

		fmt.Printf("远程配置已更新: %s = %s\n", key, value)
		return nil
	},
}

func boolStr(v bool) string {
	if v {
		return "已设置"
	}
	return "未设置"
}

func init() {
	configCmd.AddCommand(configRemoteCmd)
	configRemoteCmd.AddCommand(configRemoteSetCmd)
}
