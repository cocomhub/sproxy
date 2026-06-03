// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var searchCmd = &cobra.Command{
	Use:   "search <keyword>",
	Short: "搜索文件",
	Long: `在 sproxy 服务端上搜索文件名包含指定关键字的文件（不区分大小写）。

示例：
  sclient search report     # 搜索文件名包含 "report" 的文件
  sclient search .txt       # 搜索所有 .txt 文件`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			os.Exit(1)
		}

		files, err := cli.Search(context.Background(), args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "搜索失败: %v\n", err)
			os.Exit(1)
		}

		if len(files) == 0 {
			fmt.Println("no files found")
		} else {
			for _, f := range files {
				if f.IsDir {
					fmt.Printf("%-40s  %10s\n", f.Name+"/", "-")
				} else {
					csPrefix := f.Checksum
					if len(csPrefix) > 16 {
						csPrefix = csPrefix[:16] + "…"
					}
					if csPrefix == "" {
						csPrefix = "-"
					}
					fmt.Printf("%-40s  %10s  %s\n", f.Name, client.FormatByte(float64(f.Size)), csPrefix)
				}
			}
		}
	},
}
