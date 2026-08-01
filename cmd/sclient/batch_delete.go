// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdBatchDelete 创建独立的 batch-delete 命令工厂函数，使用 state.State 替代全局 currentDir。
func NewCmdBatchDelete(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "batch-delete <file1> [file2...]",
		Short: "批量删除文件",
		Long: `批量删除 sproxy 服务端上的多个文件。
		使用批量 API 一次性提交所有删除请求，避免逐文件 RTT。`,
		Example: `  sclient batch-delete a.txt b.txt dir/file.txt`,
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			// 收集所有文件，先 resolve 路径
			type fileItem struct {
				orig   string
				remote string
			}
			items := make([]fileItem, 0, len(args))
			for _, filename := range args {
				remote, err := st.ResolveRemotePath(filename)
				items = append(items, fileItem{orig: filename, remote: remote})
				if err != nil {
					continue
				}
			}

			// 构建批量删除请求 — 只传成功 resolve 路径的文件
			batchFiles := make([]client.BatchDeleteFile, 0, len(items))
			// itemBatchIdx[i] 对应 items 中第 i 个元素在 batchFiles 中的索引（-1 表示未加入）
			itemBatchIdx := make([]int, len(items))
			for i, item := range items {
				if item.remote == "" {
					itemBatchIdx[i] = -1
					continue
				}
				itemBatchIdx[i] = len(batchFiles)
				batchFiles = append(batchFiles, client.BatchDeleteFile{Filename: item.remote})
			}

			results := make([]batchOperationResult, len(items))
			if len(batchFiles) > 0 {
				apiResults, err := svc.BatchDelete(cmd.Context(), batchFiles)
				if err != nil {
					// 批量 API 整体失败
					for i, item := range items {
						results[i] = batchOperationResult{
							Name:    item.orig,
							Success: false,
							Message: err.Error(),
						}
					}
				} else {
					// 映射 API 返回结果到原始文件名
					for i, item := range items {
						idx := itemBatchIdx[i]
						if idx < 0 {
							results[i] = batchOperationResult{
								Name:    item.orig,
								Success: false,
								Message: "路径解析失败",
							}
							continue
						}
						if idx < len(apiResults) {
							ar := apiResults[idx]
							msg := ar.Message
							if msg == "" {
								if ar.Success {
									msg = "OK"
								} else {
									msg = "删除失败"
								}
							}
							results[i] = batchOperationResult{
								Name:    item.orig,
								Success: ar.Success,
								Message: msg,
							}
						} else {
							results[i] = batchOperationResult{
								Name:    item.orig,
								Success: false,
								Message: "服务端未返回结果",
							}
						}
					}
				}
			} else {
				// 所有路径解析都失败
				for i, item := range items {
					results[i] = batchOperationResult{
						Name:    item.orig,
						Success: false,
						Message: "路径解析失败",
					}
				}
			}

			// 打印结果
			printBatchResults(results, ios.Out)

			total := len(results)
			success := countBatchSuccess(results)
			fail := total - success
			fmt.Fprintf(ios.Out, "\n总: %d, 成功: %d, 失败: %d\n", total, success, fail)
			if fail > 0 {
				return fmt.Errorf("批量删除完成，%d 个操作失败", fail)
			}
			return nil
		},
	}
}