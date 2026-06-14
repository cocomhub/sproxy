// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var batchRenameCmd = &cobra.Command{
	Use:   "batch-rename <from1> <to1> [from2 to2...]",
	Short: "批量重命名文件",
	Long: `批量重命名 sproxy 服务端上的文件。
	参数成对传入：每对 (from, to) 构成一次重命名操作。`,
	Example: `  sclient batch-rename old1.txt new1.txt old2.txt new2.txt`,
	Args:    cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args)%2 != 0 {
			return fmt.Errorf("参数必须成对出现")
		}

		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			return fmt.Errorf("初始化客户端失败: %w", err)
		}

		// 构造成对参数列表
		pairs := make([]struct{ from, to string }, len(args)/2)
		for i := 0; i < len(args); i += 2 {
			pairs[i/2].from, pairs[i/2].to = args[i], args[i+1]
		}

		results := make([]batchOperationResult, 0, len(pairs))
		for _, p := range pairs {
			result := batchOperationResult{Name: fmt.Sprintf("%s -> %s", p.from, p.to)}
			// 先 stat 获取远端 checksum
			info, err := cli.Stat(context.Background(), p.from)
			if err != nil {
				result.Message = fmt.Sprintf("stat 失败: %v", err)
				results = append(results, result)
				continue
			}
			if info.Checksum == "" {
				result.Message = "远端文件 checksum 为空"
				results = append(results, result)
				continue
			}

			if err := cli.Rename(context.Background(), p.from, p.to, info.Checksum); err != nil {
				result.Message = err.Error()
			} else {
				result.Success = true
				result.Message = "OK"
			}
			results = append(results, result)
		}

		printBatchResults(results)

		total := len(results)
		success := countBatchSuccess(results)
		fail := total - success
		fmt.Printf("\n总: %d, 成功: %d, 失败: %d\n", total, success, fail)
		if fail > 0 {
			return fmt.Errorf("批量重命名完成，%d 个操作失败", fail)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(batchRenameCmd)
}
