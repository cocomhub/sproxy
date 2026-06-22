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

var downloadCmd = &cobra.Command{
	Use:   "download <filename> [output]",
	Short: "下载文件",
	Long: `从 sproxy 服务端下载文件。
		filename 可以包含路径，如 "dir/file.txt" 下载对应子目录下的文件。
		output 指定本地保存路径，省略时使用文件名。`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			return fmt.Errorf(errFmtInitClient, err)
		}

		filename, err := resolveRemotePathOrErr(args[0])
		if err != nil {
			return err
		}
		outputPath, _ := cmd.Flags().GetString("output")
		if outputPath == "" && len(args) > 1 {
			outputPath = args[1]
		}

		chunkedMode, _ := cmd.Flags().GetBool("chunked")

		// 如果未显式指定分块模式，检查远端文件大小是否达到自动分块阈值
		ctx := context.Background()
		if !chunkedMode {
			if info, statErr := cli.Stat(ctx, filename); statErr == nil && info.Size > 0 {
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
			if err := cli.ChunkedDownload(ctx, filename, outputPath, chunkOpts...); err != nil {
				fmt.Fprintf(os.Stderr, "分块下载失败: %v\n", err)
				return fmt.Errorf("分块下载失败: %w", err)
			}
		} else {
			if err := cli.Download(ctx, filename, outputPath); err != nil {
				fmt.Fprintf(os.Stderr, "下载失败: %v\n", err)
				return fmt.Errorf("下载失败: %w", err)
			}
		}
		fmt.Printf("文件已下载到: %s\n", outputPath)
		return nil
	},
}

func init() {
	downloadCmd.Flags().Bool("chunked", false, "启用分块下载模式")
	downloadCmd.Flags().Int64("chunk-size", 0, "分块大小 (默认 4MB)")
	downloadCmd.Flags().Int("concurrency", 0, "下载并发数 (默认 4)")
	downloadCmd.Flags().Bool("resume", true, "续传模式")
}
