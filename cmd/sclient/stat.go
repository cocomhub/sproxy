// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

var statCmd = &cobra.Command{
	Use:   "stat <filename>",
	Short: "查询远端文件元信息（不下载）",
	Long: `通过 HEAD /api/files/stat 获取远端单个文件的元信息：
		size、checksum、mod_time。不下载文件内容。

		filename 受当前目录 (cd) 影响：相对路径自动拼接前缀，绝对路径 (/开头) 绕过。

		示例:
		  sclient stat README.md
		  sclient stat sub/dir/file.txt`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			return fmt.Errorf(errFmtInitClient, err)
		}

		filename, err := resolveRemotePathOrErr(args[0])
		if err != nil {
			return err
		}
		info, err := cli.Stat(context.Background(), filename)
		if err != nil {
			fmt.Fprintf(os.Stderr, "获取文件信息失败: %v\n", err)
			return fmt.Errorf("获取文件信息失败: %w", err)
		}

		fm := buildFormatter(cmd)
		fm.PrintStat(info, filename)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statCmd)
}

// NewCmdStat 创建 stat 命令的工厂函数，使用 state.State 替代全局变量。
func NewCmdStat(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "stat <filename>",
		Short: "查询远端文件元信息（不下载）",
		Long: `通过 HEAD /api/files/stat 获取远端单个文件的元信息：
		size、checksum、mod_time。不下载文件内容。

		filename 受当前目录 (cd) 影响：相对路径自动拼接前缀，绝对路径 (/开头) 绕过。

		示例:
		  sclient stat README.md
		  sclient stat sub/dir/file.txt`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			info, err := svc.Stat(cmd.Context(), filename)
			if err != nil {
				ios.WriteErrLine("获取文件信息失败: %v", err)
				return fmt.Errorf("获取文件信息失败: %w", err)
			}

			fm := buildFormatterWithWriter(ios.Out, cmd)
			fm.PrintStat(info, filename)
			return nil
		},
	}
}
