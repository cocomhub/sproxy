// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"time"

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
	Run: func(cmd *cobra.Command, args []string) {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			os.Exit(1)
		}

		filename := mustResolveRemotePath(args[0])
		info, err := cli.Stat(context.Background(), filename)
		if err != nil {
			fmt.Fprintf(os.Stderr, "获取文件信息失败: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("name:     %s\n", filename)
		if info.IsDir {
			fmt.Println("type:     directory")
		} else {
			fmt.Println("type:     file")
			fmt.Printf("size:     %d 字节\n", info.Size)
		}
		if info.Checksum != "" {
			fmt.Printf("checksum: %s\n", info.Checksum)
		}
		if info.ModTime > 0 {
			mt := time.Unix(0, info.ModTime)
			fmt.Printf("mtime:    %s\n", mt.Format(time.RFC3339))
		}
	},
}

func init() {
	rootCmd.AddCommand(statCmd)
}
