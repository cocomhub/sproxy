// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
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
			return runBatchDelete(cmd, args, factory, ios, st)
		},
	}
}

// batchDeleteItem 记录 batch-delete 的原始文件名与解析后的远端路径。
type batchDeleteItem struct {
	orig   string
	remote string
}

// runBatchDelete 执行 batch-delete 命令主体。
func runBatchDelete(cmd *cobra.Command, args []string, factory clientfactory.Factory, ios cli.IOStreams, st *state.State) error {
	svc, err := factory.NewClient(cmd)
	if err != nil {
		ios.WriteErrLine(errFmtInitClientPrint, err)
		return fmt.Errorf(errFmtInitClient, err)
	}

	items, itemBatchIdx, batchFiles := resolveBatchDeleteItems(args, st)
	results := mapBatchDeleteResults(items, itemBatchIdx, batchFiles, svc, cmd.Context())

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
}

// resolveBatchDeleteItems 收集所有文件并 resolve 路径，构建批量删除请求与逐项索引。
func resolveBatchDeleteItems(args []string, st *state.State) ([]batchDeleteItem, []int, []client.BatchDeleteFile) {
	// 收集所有文件，先 resolve 路径
	items := make([]batchDeleteItem, 0, len(args))
	for _, filename := range args {
		remote, err := st.ResolveRemotePath(filename)
		items = append(items, batchDeleteItem{orig: filename, remote: remote})
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
	return items, itemBatchIdx, batchFiles
}

// mapBatchDeleteResults 调用批量删除 API 并把返回结果映射回原始文件名。
func mapBatchDeleteResults(items []batchDeleteItem, itemBatchIdx []int, batchFiles []client.BatchDeleteFile, svc *client.FileClient, ctx context.Context) []batchOperationResult {
	results := make([]batchOperationResult, len(items))
	if len(batchFiles) == 0 {
		// 所有路径解析都失败
		return fillBatchDeleteResult(items, results, "路径解析失败")
	}
	apiResults, err := svc.BatchDelete(ctx, batchFiles)
	if err != nil {
		// 批量 API 整体失败
		return fillBatchDeleteResultErr(items, results, err)
	}
	// 映射 API 返回结果到原始文件名
	for i, item := range items {
		results[i] = batchDeleteResultFor(item, itemBatchIdx[i], apiResults)
	}
	return results
}

// batchDeleteResultFor 计算单个文件（按在 batchFiles 中的索引）的删除结果。
func batchDeleteResultFor(item batchDeleteItem, idx int, apiResults []client.BatchOperationResult) batchOperationResult {
	if idx < 0 {
		return batchOperationResult{Name: item.orig, Success: false, Message: "路径解析失败"}
	}
	if idx >= len(apiResults) {
		return batchOperationResult{Name: item.orig, Success: false, Message: "服务端未返回结果"}
	}
	ar := apiResults[idx]
	msg := ar.Message
	if msg == "" {
		if ar.Success {
			msg = "OK"
		} else {
			msg = "删除失败"
		}
	}
	return batchOperationResult{Name: item.orig, Success: ar.Success, Message: msg}
}

// fillBatchDeleteResult 以统一文案填充全部失败结果（单循环）。
func fillBatchDeleteResult(items []batchDeleteItem, results []batchOperationResult, message string) []batchOperationResult {
	for i, item := range items {
		results[i] = batchOperationResult{Name: item.orig, Success: false, Message: message}
	}
	return results
}

// fillBatchDeleteResultErr 以整体错误信息填充全部失败结果（单循环）。
func fillBatchDeleteResultErr(items []batchDeleteItem, results []batchOperationResult, err error) []batchOperationResult {
	for i, item := range items {
		results[i] = batchOperationResult{Name: item.orig, Success: false, Message: err.Error()}
	}
	return results
}
