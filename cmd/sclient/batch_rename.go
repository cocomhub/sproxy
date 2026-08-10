// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdBatchRename 创建独立的 batch-rename 命令工厂函数。
// 注意：batch-rename 不需要 st *state.State，因为参数是成对的 from/to 路径。
func NewCmdBatchRename(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "batch-rename <from1> <to1> [from2 to2...]",
		Short: "批量重命名文件",
		Long: `批量重命名 sproxy 服务端上的文件。
		参数成对传入：每对 (from, to) 构成一次重命名操作。
		先批量 Stat 获取所有文件的 checksum，然后一次性提交批量重命名请求。`,
		Example: `  sclient batch-rename old1.txt new1.txt old2.txt new2.txt`,
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args)%2 != 0 {
				return fmt.Errorf("参数必须成对出现")
			}

			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			// 构造成对参数列表
			type renamePair struct {
				from string
				to   string
			}
			pairs := make([]renamePair, len(args)/2)
			for i := 0; i < len(args); i += 2 {
				pairs[i/2].from, pairs[i/2].to = args[i], args[i+1]
			}

			// 第一步：批量 Stat 获取所有文件的 checksum
			statResults := make([]*client.FileInfo, len(pairs))
			statErrors := make([]error, len(pairs))
			for i, p := range pairs {
				info, err := svc.Stat(cmd.Context(), p.from)
				statResults[i] = info
				statErrors[i] = err
			}

			// 第二步：构造批量重命名请求
			renameOps := make([]client.BatchRenameOp, 0, len(pairs))
			// pairBatchIdx[i] 对应 pairs[i] 在 renameOps 中的索引（-1 表示跳过）
			pairBatchIdx := make([]int, len(pairs))
			for i, p := range pairs {
				if statErrors[i] != nil {
					pairBatchIdx[i] = -1
					continue
				}
				if statResults[i] == nil || statResults[i].Checksum == "" {
					pairBatchIdx[i] = -1
					continue
				}
				pairBatchIdx[i] = len(renameOps)
				renameOps = append(renameOps, client.BatchRenameOp{
					From:     p.from,
					To:       p.to,
					Checksum: statResults[i].Checksum,
				})
			}

			results := make([]batchOperationResult, len(pairs))
			if len(renameOps) > 0 {
				apiResults, err := svc.BatchRename(cmd.Context(), renameOps)
				if err != nil {
					// 批量 API 整体失败
					for i, p := range pairs {
						msg := ""
						if statErrors[i] != nil {
							msg = fmt.Sprintf("stat 失败: %v", statErrors[i])
						} else if statResults[i] == nil || statResults[i].Checksum == "" {
							msg = "远端文件 checksum 为空"
						} else {
							msg = err.Error()
						}
						results[i] = batchOperationResult{
							Name:    fmt.Sprintf("%s -> %s", p.from, p.to),
							Success: false,
							Message: msg,
						}
					}
				} else {
					// 映射 API 返回结果
					for i, p := range pairs {
						idx := pairBatchIdx[i]
						if idx < 0 {
							msg := ""
							if statErrors[i] != nil {
								msg = fmt.Sprintf("stat 失败: %v", statErrors[i])
							} else {
								msg = "远端文件 checksum 为空"
							}
							results[i] = batchOperationResult{
								Name:    fmt.Sprintf("%s -> %s", p.from, p.to),
								Success: false,
								Message: msg,
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
									msg = "重命名失败"
								}
							}
							results[i] = batchOperationResult{
								Name:    fmt.Sprintf("%s -> %s", p.from, p.to),
								Success: ar.Success,
								Message: msg,
							}
						} else {
							results[i] = batchOperationResult{
								Name:    fmt.Sprintf("%s -> %s", p.from, p.to),
								Success: false,
								Message: "服务端未返回结果",
							}
						}
					}
				}
			} else {
				// 所有 stat 都失败
				for i, p := range pairs {
					msg := ""
					if statErrors[i] != nil {
						msg = fmt.Sprintf("stat 失败: %v", statErrors[i])
					} else {
						msg = "远端文件 checksum 为空"
					}
					results[i] = batchOperationResult{
						Name:    fmt.Sprintf("%s -> %s", p.from, p.to),
						Success: false,
						Message: msg,
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
				return fmt.Errorf("批量重命名完成，%d 个操作失败", fail)
			}
			return nil
		},
	}
}
