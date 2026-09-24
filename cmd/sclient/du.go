// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdDu 创建 du 命令：按目录递归统计（文件数/字节/子目录数）。
// path 缺省 = 当前目录；受 cd 与 --vol 影响。
func NewCmdDu(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "du [path]",
		Short: "查看目录空间占用（递归统计文件数/大小）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			path := ""
			if len(args) > 0 {
				path = args[0]
			}
			if path != "" && !isRemoteAbs(path) {
				resolved, rerr := st.ResolveRemotePath(path)
				if rerr != nil {
					return fmt.Errorf("解析路径失败: %w", rerr)
				}
				path = resolved
			}
			res, err := svc.Du(cmd.Context(), path)
			if err != nil {
				ios.WriteErrLine("获取目录统计失败: %v", err)
				return fmt.Errorf("获取目录统计失败: %w", err)
			}
			jsonOut, _ := cmd.Flags().GetBool("json")
			if jsonOut {
				enc := json.NewEncoder(ios.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"du": res})
			}
			displayPath := res.Path
			if displayPath == "user" {
				displayPath = "."
			}
			fmt.Fprintf(ios.Out, "%-24s %10s  %8d 文件  %8d 目录\n",
				displayPath, formatBytes(res.Size), res.Files, res.Dirs)
			return nil
		},
	}
	return cmd
}

// NewCmdDF 创建 df 命令：卷水位 + 磁盘水位（复用 /api/stats）。
func NewCmdDF(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "df",
		Short: "查看卷水位与磁盘空间",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			stats, err := svc.GetStats(cmd.Context())
			if err != nil {
				ios.WriteErrLine("获取统计信息失败: %v", err)
				return fmt.Errorf("获取统计信息失败: %w", err)
			}
			jsonOut, _ := cmd.Flags().GetBool("json")
			if jsonOut {
				enc := json.NewEncoder(ios.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"df": stats})
			}
			fmt.Fprintf(ios.Out, "%-10s %14s %14s %8s\n", "类型", "已用", "可用/上限", "水位")
			fmt.Fprintf(ios.Out, "%-10s %14s %14s %8s\n",
				"磁盘", formatBytes(stats.DiskUsed), formatBytes(stats.DiskFree), "")
			switch {
			case stats.Quota != nil && stats.Quota.MaxBytes > 0:
				fmt.Fprintf(ios.Out, "%-10s %14s %14s %8d%%\n",
					"配额", formatBytes(stats.Quota.Usage), formatBytes(stats.Quota.MaxBytes), stats.Quota.Watermark)
			case stats.Quota != nil:
				fmt.Fprintf(ios.Out, "%-10s %14s %14s %8s\n",
					"配额", formatBytes(stats.Quota.Usage), "不限", "")
			default:
				fmt.Fprintf(ios.Out, "%-10s %14s %14s %8s\n", "配额", "—", "—", "—")
			}
			return nil
		},
	}
	return cmd
}

// isRemoteAbs 判断用户输入是否为服务端绝对路径（/ 开头）。
func isRemoteAbs(p string) bool {
	return len(p) > 0 && p[0] == '/'
}

// formatBytes 人类可读字节（1 KiB 等；quota.go 已定义，复用）。
