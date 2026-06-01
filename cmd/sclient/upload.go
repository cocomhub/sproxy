// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var uploadCmd = &cobra.Command{
	Use:   "upload <file1> [file2...]",
	Short: "上传一个或多个文件",
	Long: `上传一个或多个文件到 sproxy 服务端。
文件路径中的目录结构会被保留。
如：sclient upload dir/file.txt 会将文件保存到服务端的 uploads_dir/dir/file.txt

受当前目录 (cd) 影响：相对路径会拼接当前目录前缀。
使用 / 开头的绝对路径可以绕过当前目录。`,
	Args: cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			os.Exit(1)
		}

		chunkedMode, _ := cmd.Flags().GetBool("chunked")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		chunkSize, _ := cmd.Flags().GetInt64("chunk-size")
		resume, _ := cmd.Flags().GetBool("resume")

		ctx := context.Background()
		for _, filePath := range args {
			fmt.Printf("上传: %s\n", filePath)

			useChunked := chunkedMode
			if !useChunked {
				if stat, err := os.Stat(filePath); err == nil {
					useChunked = client.ShouldAutoChunk(stat.Size())
				}
			}

			// 计算远端路径：clean + 拼接 currentDir
			remotePath := resolveRemotePath(filepath.ToSlash(filepath.Clean(filePath)))

			if useChunked {
				chunkOpts := []client.ChunkedOption{
					client.WithChunkedResume(resume),
				}
				if chunkSize > 0 {
					chunkOpts = append(chunkOpts, client.WithChunkedChunkSize(chunkSize))
				}
				if concurrency > 0 {
					chunkOpts = append(chunkOpts, client.WithChunkedConcurrency(concurrency))
				}
				result, err := cli.ChunkedUpload(ctx, filePath, remotePath, chunkOpts...)
				if err != nil {
					fmt.Fprintf(os.Stderr, "分块上传失败: %s %v\n", filePath, err)
					os.Exit(1)
				}
				fmt.Printf("成功: %v, 消息: %s\n", result.Success, result.Message)
				if result.FileChecksum != "" {
					fmt.Printf("文件 SHA-256: %s\n", result.FileChecksum)
				}
			} else {
				result, err := cli.Upload(ctx, filePath, remotePath)
				if err != nil {
					fmt.Fprintf(os.Stderr, "上传失败: %s %v\n", filePath, err)
					if result != nil {
						fmt.Fprintf(os.Stderr, "服务端消息: %s\n", result.Message)
					}
					os.Exit(1)
				}
				fmt.Printf("成功: %v, 消息: %s\n", result.Success, result.Message)
				if result.Checksum != "" {
					fmt.Printf("文件 SHA-256: %s\n", result.Checksum)
				}
			}
		}
	},
}

func init() {
	uploadCmd.Flags().Bool("chunked", false, "启用分块上传模式")
	uploadCmd.Flags().Int64("chunk-size", 0, "分块大小 (默认 4MB)")
	uploadCmd.Flags().Int("concurrency", 0, "上传并发数 (默认 4)")
	uploadCmd.Flags().Bool("resume", true, "续传模式")
}
