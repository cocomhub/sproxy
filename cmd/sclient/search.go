// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var searchCmd = &cobra.Command{
	Use:   "search <keyword>",
	Short: "搜索文件",
	Long: `搜索 sproxy 服务端上名称匹配的文件。

	搜索关键字支持模糊匹配，例如：
	  sclient search report     # 搜索名称包含 "report" 的文件
	  sclient search .txt       # 搜索名称包含 .txt 的文件`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			return fmt.Errorf(errFmtInitClient, err)
		}

		files, err := cli.Search(context.Background(), args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "搜索失败: %v\n", err)
			return fmt.Errorf("搜索失败: %w", err)
		}

		if len(files) == 0 {
			fmt.Println("no files found")
		} else {
			printFileList(files, os.Stdout)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(searchCmd)
}
