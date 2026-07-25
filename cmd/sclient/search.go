// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdSearch 创建 search 命令的工厂函数。
// search 命令不依赖当前目录，因此不需要 state.State 参数。
func NewCmdSearch(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "search <keyword>",
		Short: "搜索文件",
		Long: `搜索 sproxy 服务端上名称匹配的文件。

		搜索关键字支持模糊匹配，例如：
		  sclient search report     # 搜索名称包含 "report" 的文件
		  sclient search .txt       # 搜索名称包含 .txt 的文件`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			files, err := svc.Search(cmd.Context(), args[0])
			if err != nil {
				ios.WriteErrLine("搜索失败: %v", err)
				return fmt.Errorf("搜索失败: %w", err)
			}

			fm := buildFormatterWithWriter(ios.Out, cmd)
			if len(files) == 0 {
				fm.Println("no files found")
			} else {
				fm.PrintFileList(files)
			}
			return nil
		},
	}
}
