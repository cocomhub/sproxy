// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var mvCmd = &cobra.Command{
	Use:   "mv <from> <to>",
	Short: "重命名 / 移动远端文件",
	Long: `重命名或移动 sproxy 服务端上的文件。

服务端会先校验源文件的 SHA-256（避免在并发写入下误覆盖），然后执行 rename。
目标父目录不存在时自动 mkdir -p；目标已存在时返回 409。

from 和 to 都受当前目录 (cd) 影响：相对路径自动拼接前缀，绝对路径 (/开头) 绕过。

示例:
  sclient mv old.txt new.txt
  sclient mv old.txt sub/dir/new.txt
  sclient mv /a/b.txt /c/b.txt`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			os.Exit(1)
		}

		from := mustResolveRemotePath(args[0])
		to := mustResolveRemotePath(args[1])

		ctx := context.Background()

		info, err := cli.Stat(ctx, from)
		if err != nil {
			fmt.Fprintf(os.Stderr, "获取源文件信息失败: %v\n", err)
			os.Exit(1)
		}
		if info.Checksum == "" {
			fmt.Fprintln(os.Stderr, "源文件 checksum 为空，无法重命名")
			os.Exit(1)
		}

		if err := cli.Rename(ctx, from, to, info.Checksum); err != nil {
			fmt.Fprintf(os.Stderr, "重命名失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("已重命名: %s -> %s\n", from, to)
	},
}

func init() {
	rootCmd.AddCommand(mvCmd)
}
