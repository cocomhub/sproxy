// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var shareCmd = &cobra.Command{
	Use:   "share",
	Short: "文件分享管理",
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
	},
}

var shareCreateCmd = &cobra.Command{
	Use:   "create <filename>",
	Short: "创建文件分享链接",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}

		ttlStr, _ := cmd.Flags().GetString("ttl")
		ttl := 24 * time.Hour
		if ttlStr != "" {
			d, parseErr := time.ParseDuration(ttlStr)
			if parseErr == nil && d > 0 {
				ttl = d
			}
		}
		maxDownloads, _ := cmd.Flags().GetInt("max-downloads")
		oneTime, _ := cmd.Flags().GetBool("one-time")

		link, err := cli.CreateShare(cmd.Context(), args[0], ttl, maxDownloads, oneTime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "创建分享链接失败: %v\n", err)
			return fmt.Errorf("创建分享链接失败: %w", err)
		}

		serverURL, _ := cmd.Flags().GetString("server")
		if serverURL == "" && cfgProvider != nil {
			cfg, cfgErr := client.LoadFromProvider(cfgProvider)
			if cfgErr == nil {
				serverURL = cfg.ServerURL
			}
		}
		shareURL := strings.TrimRight(serverURL, "/") + "/s/" + link.Token

		fm := buildFormatter(cmd)
		fm.Printf("分享链接: %s\n", shareURL)
		fm.Printf("Token: %s\n", link.Token)
		fm.Printf("有效期至: %s\n", link.ExpiresAt)
		fm.Printf("最大下载次数: %d\n", link.MaxDownloads)
		fm.Printf("一次性: %v\n", link.OneTime)
		return nil
	},
}

var shareListCmd = &cobra.Command{
	Use:   "list",
	Short: "列出所有分享链接",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}

		shares, err := cli.ListShares(cmd.Context())
		if err != nil {
			fmt.Fprintf(os.Stderr, "获取分享列表失败: %v\n", err)
			return fmt.Errorf("获取分享列表失败: %w", err)
		}

		fm := buildFormatter(cmd)
		fm.PrintShareList(shares)
		return nil
	},
}

var shareRevokeCmd = &cobra.Command{
	Use:   "revoke <token>",
	Short: "撤销分享链接",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}

		if err := cli.RevokeShare(cmd.Context(), args[0]); err != nil {
			fmt.Fprintf(os.Stderr, "撤销分享链接失败: %v\n", err)
			return fmt.Errorf("撤销分享链接失败: %w", err)
		}

		fm := buildFormatter(cmd)
		fm.Printf("已撤销分享: %s\n", args[0])
		return nil
	},
}

func init() {
	shareCreateCmd.Flags().String("ttl", "24h", "有效期（例如 1h, 24h, 168h, 720h，不支持 d 天）")
	shareCreateCmd.Flags().Int("max-downloads", 0, "最大下载次数（0=不限）")
	shareCreateCmd.Flags().Bool("one-time", false, "一次性分享（下载一次后自动失效）")

	shareCmd.AddCommand(shareCreateCmd)
	shareCmd.AddCommand(shareListCmd)
	shareCmd.AddCommand(shareRevokeCmd)
}
