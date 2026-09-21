// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdDownload 创建独立的 download 命令工厂函数，使用 state.State 替代全局 currentDir。
func NewCmdDownload(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "download <filename> [output]",
		Short: "下载文件",
		Long: `从 sproxy 服务端下载文件。
			filename 可以包含路径，如 "dir/file.txt" 下载对应子目录下的文件。
			output 指定本地保存路径，省略时使用文件名。`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			outputPath, _ := cmd.Flags().GetString("output")
			if outputPath == "" && len(args) > 1 {
				outputPath = args[1]
			}

			chunkedMode, _ := cmd.Flags().GetBool("chunked")

			// 如果未显式指定分块模式，检查远端文件大小是否达到自动分块阈值
			if !chunkedMode {
				if info, statErr := svc.Stat(cmd.Context(), filename); statErr == nil && info.Size > 0 {
					chunkedMode = client.ShouldAutoChunk(info.Size)
				}
			}

			concurrency, _ := cmd.Flags().GetInt("concurrency")
			chunkSize, _ := cmd.Flags().GetInt64("chunk-size")
			resume, _ := cmd.Flags().GetBool("resume")

			if chunkedMode {
				chunkOpts := []client.ChunkedOption{
					client.WithChunkedResume(resume),
				}
				if chunkSize > 0 {
					chunkOpts = append(chunkOpts, client.WithChunkedChunkSize(chunkSize))
				}
				if concurrency > 0 {
					chunkOpts = append(chunkOpts, client.WithChunkedConcurrency(concurrency))
				}
				stats := NewTransferStats()
				fileStart := time.Now()
				if err := svc.ChunkedDownload(cmd.Context(), filename, outputPath, chunkOpts...); err != nil {
					ios.WriteErrLine("分块下载失败: %v", err)
					return fmt.Errorf("分块下载失败: %w", err)
				}
				// 分块下载成功 = 校验通过；大小以本地落盘文件为准（尽力而为）。
				stats.SetChunkSuccessRate(totalChunksOr(filename, svc), 0)
				stats.AddFile(outputPath, fileSizeOr(outputPath), time.Since(fileStart))
				stats.Finalize()
				// 统计行走 formatter：表格输出 FormatLine 文本；--json 输出 stats 对象。
				buildFormatterWithWriter(ios.Out, cmd).PrintTransferStats(stats)
			} else {
				stats := NewTransferStats()
				fileStart := time.Now()
				if err := svc.Download(cmd.Context(), filename, outputPath); err != nil {
					ios.WriteErrLine("下载失败: %v", err)
					return fmt.Errorf("下载失败: %w", err)
				}
				stats.AddFile(outputPath, fileSizeOr(outputPath), time.Since(fileStart))
				stats.Finalize()
				// 统计行走 formatter：表格输出 FormatLine 文本；--json 输出 stats 对象。
				buildFormatterWithWriter(ios.Out, cmd).PrintTransferStats(stats)
			}
			fmt.Fprintf(ios.Out, "文件已下载到: %s\n", outputPath)
			return nil
		},
	}
	cmd.Flags().Bool("chunked", false, "启用分块下载模式")
	cmd.Flags().Int64("chunk-size", 0, "分块大小 (默认 4MB)")
	cmd.Flags().Int("concurrency", 0, "下载并发数 (默认 4)")
	cmd.Flags().Bool("resume", true, "续传模式")
	return cmd
}

// totalChunksOr 估算分块下载的分块总数（远端文件大小 / 默认分块大小，向上取整）。
// Stat 失败或大小未知时返回 0（成功率段省略，零回归）。
// 注意：这是尽力而为的估算——分块下载内部真实块数由服务端会话决定，
// 客户端仅用于展示分块成功率（成功 = 下载成功即全成功）。
func totalChunksOr(filename string, svc *client.FileClient) int {
	info, err := svc.Stat(context.Background(), filename)
	if err != nil || info.Size <= 0 {
		return 0
	}
	const defaultChunk = 4 * 1024 * 1024 // 与 client 默认分块大小一致（4 MiB）
	n := int((info.Size + defaultChunk - 1) / defaultChunk)
	if n <= 0 {
		return 0
	}
	return n
}
