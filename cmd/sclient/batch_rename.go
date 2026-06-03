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

var batchRenameCmd = &cobra.Command{
	Use:   "batch-rename <from1> <to1> [from2 to2...]",
	Short: "批量重命名文件",
	Long: `批量重命名 sproxy 服务端上的文件。
参数成对传入：每对 (from, to) 是一次重命名操作。
每个源文件会先通过 Stat 获取 checksum 用于校验。

示例：
  sclient batch-rename old1.txt new1.txt old2.txt new2.txt`,
	Args: cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		argsLen := len(args)
		if argsLen%2 != 0 {
			fmt.Fprintln(os.Stderr, "参数必须成对提供：batch-rename <from1> <to1> [from2 to2...]")
			os.Exit(1)
		}

		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			os.Exit(1)
		}

		ctx := context.Background()
		operations := make([]client.BatchRenameOp, 0, argsLen/2)
		for i := 0; i < argsLen; i += 2 {
			from := mustResolveRemotePath(args[i])
			to := mustResolveRemotePath(args[i+1])

			info, statErr := cli.Stat(ctx, from)
			if statErr != nil {
				fmt.Fprintf(os.Stderr, "获取 %s 信息失败: %v\n", from, statErr)
				os.Exit(1)
			}
			operations = append(operations, client.BatchRenameOp{
				From:     from,
				To:       to,
				Checksum: info.Checksum,
			})
		}

		results, err := cli.BatchRename(ctx, operations)
		if err != nil {
			fmt.Fprintf(os.Stderr, "批量重命名失败: %v\n", err)
			os.Exit(1)
		}
		for _, r := range results {
			status := "OK"
			if !r.Success {
				status = "FAIL"
			}
			fmt.Printf("[%s] %s: %s\n", status, r.Filename, r.Message)
		}
	},
}

func init() {
	rootCmd.AddCommand(batchRenameCmd)
}
