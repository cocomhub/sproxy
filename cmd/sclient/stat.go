// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdStat 创建 stat 命令：无参显示本地 client 状态；server 显示远端服务状态。
// 原 stat <file> 文件元信息功能迁移至 meta。
func NewCmdStat(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stat [server]",
		Short: "显示本地 client 或远端服务状态",
		Args:  cobra.MaximumNArgs(1),
	}
	cmd.AddCommand(NewCmdStatServer(factory, ios))
	cmd.RunE = func(c *cobra.Command, args []string) error {
		cfg, err := cfgSvc.LoadConfig()
		if err != nil {
			return err
		}
		ios.WriteOutLine("server_url: %s", cfg.ServerURL)
		ios.WriteOutLine("version: %s (build: %s)", Version, BuildAt)
		return nil
	}
	return cmd
}

// NewCmdStatServer 创建 stat server 命令：查询远端服务统计。
func NewCmdStatServer(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "显示远端服务状态",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			stats, err := svc.GetStats(cmd.Context())
			if err != nil {
				return fmt.Errorf("获取服务器统计失败: %w", err)
			}
			fm := buildFormatterWithWriter(ios.Out, cmd)
			fm.PrintStats(stats)
			return nil
		},
	}
}
