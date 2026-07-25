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

// NewCmdBatchDelete 创建独立的 batch-delete 命令工厂函数，使用 state.State 替代全局 currentDir。
func NewCmdBatchDelete(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "batch-delete <file1> [file2...]",
		Short: "批量删除文件",
		Long: `批量删除 sproxy 服务端上的多个文件。
		每个文件会先通过 Stat 获取远端 checksum，然后发起删除请求。`,
		Example: `  sclient batch-delete a.txt b.txt dir/file.txt`,
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			results := make([]batchOperationResult, 0, len(args))
			for _, filename := range args {
				result := batchOperationResult{Name: filename}
				remote, err := st.ResolveRemotePath(filename)
				if err != nil {
					result.Message = err.Error()
				} else if err := svc.Delete(cmd.Context(), remote, filename); err != nil {
					result.Message = err.Error()
				} else {
					result.Success = true
					result.Message = "OK"
				}
				results = append(results, result)
			}

			// 打印结果
			for _, r := range results {
				status := "OK"
				if !r.Success {
					status = "FAIL"
				}
				fmt.Fprintf(ios.Out, "[%s] %s: %s\n", status, r.Name, r.Message)
			}

			total := len(results)
			success := 0
			for _, r := range results {
				if r.Success {
					success++
				}
			}
			fail := total - success
			fmt.Fprintf(ios.Out, "\n总: %d, 成功: %d, 失败: %d\n", total, success, fail)
			if fail > 0 {
				return fmt.Errorf("批量删除完成，%d 个操作失败", fail)
			}
			return nil
		},
	}
}
