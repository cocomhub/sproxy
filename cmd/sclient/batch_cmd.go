// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdBatch 创建 batch 主命令：从文件逐行读取操作并并发执行。
//
// 每行是一个完整子命令调用（子命令名 + 空格分隔的参数），行间并行执行
// （--workers 控制并发峰值，默认 4）；结果按文件行序输出，与逐行执行
// 输出逐字节一致（脚本兼容零回归）。任一行 FAIL 时命令以非 0 退出码结束。
func NewCmdBatch(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "batch <file>",
		Short: "从文件逐行读取命令批量执行",
		Long: `从文件逐行读取命令并批量执行（--workers 并发，默认 4）。

	每行是一个完整子命令调用：子命令名 + 空格分隔的参数（如 "delete dir/a.txt"）。
	空行与 # 开头的注释行跳过；其余行按输入顺序并行执行，结果按文件行序输出
	（与逐行执行逐字节一致，脚本兼容零回归）。任一行失败时命令以非 0 退出码结束。

	--workers 1 退化为严格串行（等价逐行执行）；--workers 超过文件行数时按行数钳位。`,
		Example: `  sclient batch ops.txt
  sclient batch --workers 8 ops.txt
  sclient batch --workers 1 ops.txt   # 严格串行（等价逐行执行）`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			workers, _ := cmd.Flags().GetInt("workers")

			ops, err := readBatchOpsFile(args[0])
			if err != nil {
				return err
			}
			if len(ops) == 0 {
				fmt.Fprintln(ios.Out, "batch 文件为空（无待执行操作）")
				return nil
			}

			results := runBatchConcurrent(cmd.Context(), ops, workers, func(raw string) batchOperationResult {
				return runBatchLine(cmd, svc, st, raw)
			}, nil)

			// 结果按文件行序输出（runBatchConcurrent 已保序）。
			printBatchResults(results, ios.Out)

			total := len(results)
			success := countBatchSuccess(results)
			fail := total - success
			fmt.Fprintf(ios.Out, "\n总: %d, 成功: %d, 失败: %d\n", total, success, fail)
			if fail > 0 {
				return fmt.Errorf("batch 完成，%d 个操作失败", fail)
			}
			return nil
		},
	}
	cmd.Flags().Int("workers", 4, "并发 worker 数（0/1 = 串行退化；超过行数时按行数钳位）")
	return cmd
}

// readBatchOpsFile 逐行读取 batch 文件：跳过空行与 # 注释行，返回待执行操作列表。
// 每行是一个完整子命令调用（子命令名 + 空格分隔的参数）。
func readBatchOpsFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 batch 文件失败: %w", err)
	}
	defer f.Close()

	var ops []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ops = append(ops, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取 batch 文件失败: %w", err)
	}
	return ops, nil
}

// runBatchLine 执行单行操作（真实 sclient 子命令语义）。
//
// 目前支持 delete / mkdir / rmdir / meta（stat）四个轻量文件操作子集；
// 其它子命令名返回显式 FAIL（后续片按需扩充），绝不静默跳过。
func runBatchLine(cmd *cobra.Command, svc *client.FileClient, st *state.State, raw string) batchOperationResult {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return batchOperationResult{Name: raw, Success: false, Message: "空操作行"}
	}
	sub, args := fields[0], fields[1:]

	switch sub {
	case "delete":
		if len(args) != 1 {
			return batchOperationResult{Name: raw, Success: false, Message: "delete 需要 1 个参数"}
		}
		remote, err := st.ResolveRemotePathOrErr(args[0])
		if err != nil {
			return batchOperationResult{Name: raw, Success: false, Message: err.Error()}
		}
		if err := svc.Delete(cmd.Context(), remote, ""); err != nil {
			return batchOperationResult{Name: raw, Success: false, Message: err.Error()}
		}
		return batchOperationResult{Name: raw, Success: true, Message: "OK"}
	case "mkdir":
		if len(args) != 1 {
			return batchOperationResult{Name: raw, Success: false, Message: "mkdir 需要 1 个参数"}
		}
		remote, err := st.ResolveRemotePath(args[0])
		if err != nil {
			return batchOperationResult{Name: raw, Success: false, Message: err.Error()}
		}
		if err := svc.Mkdir(cmd.Context(), remote); err != nil {
			return batchOperationResult{Name: raw, Success: false, Message: err.Error()}
		}
		return batchOperationResult{Name: raw, Success: true, Message: "OK"}
	case "rmdir":
		if len(args) != 1 {
			return batchOperationResult{Name: raw, Success: false, Message: "rmdir 需要 1 个参数"}
		}
		remote, err := st.ResolveRemotePathOrErr(args[0])
		if err != nil {
			return batchOperationResult{Name: raw, Success: false, Message: err.Error()}
		}
		if err := svc.Rmdir(cmd.Context(), remote); err != nil {
			return batchOperationResult{Name: raw, Success: false, Message: err.Error()}
		}
		return batchOperationResult{Name: raw, Success: true, Message: "OK"}
	case "meta":
		if len(args) != 1 {
			return batchOperationResult{Name: raw, Success: false, Message: "meta 需要 1 个参数"}
		}
		remote, err := st.ResolveRemotePathOrErr(args[0])
		if err != nil {
			return batchOperationResult{Name: raw, Success: false, Message: err.Error()}
		}
		info, err := svc.Stat(cmd.Context(), remote)
		if err != nil {
			return batchOperationResult{Name: raw, Success: false, Message: err.Error()}
		}
		_ = info
		return batchOperationResult{Name: raw, Success: true, Message: "OK"}
	default:
		return batchOperationResult{Name: raw, Success: false, Message: fmt.Sprintf("batch 暂不支持子命令 %q", sub)}
	}
}
