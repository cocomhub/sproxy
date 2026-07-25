// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "查看服务器统计信息",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}

		stats, err := cli.GetStats(cmd.Context())
		if err != nil {
			return fmt.Errorf("获取统计信息失败: %w", err)
		}

		fm := buildFormatter(cmd)
		fm.PrintStats(stats)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statsCmd)
}

// NewCmdStats 创建 stats 命令的工厂函数。
// 使用依赖注入的 Factory 创建客户端，支持 IOStreams 输出。
func NewCmdStats(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "显示服务器统计信息",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}

			stats, err := svc.GetStats(cmd.Context())
			if err != nil {
				return fmt.Errorf("获取统计信息失败: %w", err)
			}

			// 当前使用 TextFormatter 直接输出到 ios.Out，待 PR 6 统一 OutputFormatter
			fm := NewTextFormatter(ios.Out)
			fm.PrintStats(stats)
			return nil
		},
	}
}
