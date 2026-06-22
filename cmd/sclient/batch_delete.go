// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var batchDeleteCmd = &cobra.Command{
	Use:   "batch-delete <file1> [file2...]",
	Short: "批量删除文件",
	Long: `批量删除 sproxy 服务端上的多个文件。
	每个文件会先通过 Stat 获取远端 checksum，然后发起删除请求。`,
	Example: `  sclient batch-delete a.txt b.txt dir/file.txt`,
	Args:    cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			return fmt.Errorf(errFmtInitClient, err)
		}

		results := runBatchOperation(args, func(filename string) error {
			remote, _ := resolveRemotePath(filename)
			return cli.Delete(context.Background(), remote, filename)
		})

		printBatchResults(results)

		total := len(results)
		success := countBatchSuccess(results)
		fail := total - success
		fmt.Printf("\n总: %d, 成功: %d, 失败: %d\n", total, success, fail)
		if fail > 0 {
			return fmt.Errorf("批量删除完成，%d 个操作失败", fail)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(batchDeleteCmd)
}
