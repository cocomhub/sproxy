// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// renameFailureMsg 生成批量重命名单条失败的消息。
// apiErr 非空表示批量 API 整体失败；否则说明该条目未进入请求（stat 失败或 checksum 为空）。
func renameFailureMsg(statErr error, info *client.FileInfo, apiErr string) string {
	if statErr != nil {
		return fmt.Sprintf("stat 失败: %v", statErr)
	}
	if info == nil || info.Checksum == "" {
		return "远端文件 checksum 为空"
	}
	return apiErr
}

// renamePair 记录一次批量重命名操作的 from/to 路径。
type renamePair struct {
	from string
	to   string
}

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
			return runBatchRename(cmd, args, factory, ios)
		},
	}
}

// runBatchRename 执行 batch-rename 命令主体。
func runBatchRename(cmd *cobra.Command, args []string, factory clientfactory.Factory, ios cli.IOStreams) error {
	if len(args)%2 != 0 {
		return fmt.Errorf("参数必须成对出现")
	}

	svc, err := factory.NewClient(cmd)
	if err != nil {
		ios.WriteErrLine(errFmtInitClientPrint, err)
		return fmt.Errorf(errFmtInitClient, err)
	}

	pairs := buildRenamePairs(args)
	statResults, statErrors := statRenamePairs(pairs, svc, cmd.Context())
	renameOps, pairBatchIdx := buildRenameOps(pairs, statResults, statErrors)
	results := mapBatchRenameResults(pairs, statResults, statErrors, pairBatchIdx, renameOps, svc, cmd.Context())

	printBatchResults(results, ios.Out)
	total := len(results)
	success := countBatchSuccess(results)
	fail := total - success
	fmt.Fprintf(ios.Out, "\n总: %d, 成功: %d, 失败: %d\n", total, success, fail)
	if fail > 0 {
		return fmt.Errorf("批量重命名完成，%d 个操作失败", fail)
	}
	return nil
}

// buildRenamePairs 把成对参数构造成 renamePair 列表。
func buildRenamePairs(args []string) []renamePair {
	pairs := make([]renamePair, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		pairs[i/2].from, pairs[i/2].to = args[i], args[i+1]
	}
	return pairs
}

// statRenamePairs 批量 Stat 获取每个 from 路径的文件信息与错误。
func statRenamePairs(pairs []renamePair, svc *client.FileClient, ctx context.Context) ([]*client.FileInfo, []error) {
	statResults := make([]*client.FileInfo, len(pairs))
	statErrors := make([]error, len(pairs))
	for i, p := range pairs {
		info, err := svc.Stat(ctx, p.from)
		statResults[i] = info
		statErrors[i] = err
	}
	return statResults, statErrors
}

// buildRenameOps 构造批量重命名请求（只纳入 stat 成功且 checksum 非空的条目）。
func buildRenameOps(pairs []renamePair, statResults []*client.FileInfo, statErrors []error) ([]client.BatchRenameOp, []int) {
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
	return renameOps, pairBatchIdx
}

// mapBatchRenameResults 调用批量重命名 API 并把返回结果映射回原始 from/to。
func mapBatchRenameResults(pairs []renamePair, statResults []*client.FileInfo, statErrors []error, pairBatchIdx []int, renameOps []client.BatchRenameOp, svc *client.FileClient, ctx context.Context) []batchOperationResult {
	results := make([]batchOperationResult, len(pairs))
	if len(renameOps) == 0 {
		// 所有 stat 都失败
		return fillRenameAll(pairs, statResults, statErrors, results, "")
	}
	apiResults, err := svc.BatchRename(ctx, renameOps)
	if err != nil {
		// 批量 API 整体失败
		return fillRenameAll(pairs, statResults, statErrors, results, err.Error())
	}
	// 映射 API 返回结果
	for i, p := range pairs {
		results[i] = renameResultFor(p, statResults[i], statErrors[i], pairBatchIdx[i], apiResults)
	}
	return results
}

// renameResultFor 计算单个重命名操作（按在 renameOps 中的索引）的结果。
func renameResultFor(p renamePair, info *client.FileInfo, statErr error, idx int, apiResults []client.BatchOperationResult) batchOperationResult {
	name := fmt.Sprintf("%s -> %s", p.from, p.to)
	if idx < 0 {
		return batchOperationResult{Name: name, Success: false, Message: renameFailureMsg(statErr, info, "")}
	}
	if idx >= len(apiResults) {
		return batchOperationResult{Name: name, Success: false, Message: "服务端未返回结果"}
	}
	ar := apiResults[idx]
	msg := ar.Message
	if msg == "" {
		if ar.Success {
			msg = "OK"
		} else {
			msg = "重命名失败"
		}
	}
	return batchOperationResult{Name: name, Success: ar.Success, Message: msg}
}

// fillRenameAll 以统一文案填充全部失败结果（单循环）。apiErr 非空表示批量 API 整体失败。
func fillRenameAll(pairs []renamePair, statResults []*client.FileInfo, statErrors []error, results []batchOperationResult, apiErr string) []batchOperationResult {
	for i, p := range pairs {
		results[i] = batchOperationResult{
			Name:    fmt.Sprintf("%s -> %s", p.from, p.to),
			Success: false,
			Message: renameFailureMsg(statErrors[i], statResults[i], apiErr),
		}
	}
	return results
}
