// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdUpload 创建独立的 upload 命令工厂函数，使用 clientfactory.Factory 替代 buildFileClient。
func NewCmdUpload(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload <file1> [file2...]",
		Short: "上传一个或多个文件",
		Long: `上传一个或多个文件到 sproxy 服务端。
		文件路径中的目录结构会被保留。
		如：sclient upload dir/file.txt 会将文件保存到服务端的 uploads_dir/dir/file.txt

		受当前目录 (cd) 影响：相对路径会拼接当前目录前缀。
		使用 / 开头的绝对路径可以绕过当前目录。`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			chunkedMode, _ := cmd.Flags().GetBool("chunked")
			concurrency, _ := cmd.Flags().GetInt("concurrency")
			chunkSize, _ := cmd.Flags().GetInt64("chunk-size")
			resume, _ := cmd.Flags().GetBool("resume")

			ctx := cmd.Context()
			for _, filePath := range args {
				fmt.Fprintf(ios.Out, "上传: %s\n", filePath)

				useChunked := chunkedMode
				if !useChunked {
					if stat, err := os.Stat(filePath); err == nil {
						useChunked = client.ShouldAutoChunk(stat.Size())
					}
				}

				// 计算远端路径：clean + 拼接 currentDir
				remotePath, err := st.ResolveRemotePathOrErr(filepath.ToSlash(filepath.Clean(filePath)))
				if err != nil {
					return err
				}
				fmt.Fprintf(ios.Out, "远端路径: %s\n", remotePath)

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
					result, err := svc.ChunkedUpload(ctx, filePath, remotePath, chunkOpts...)
					if err != nil {
						fmt.Fprintf(ios.ErrOut, "分块上传失败: %s %v\n", filePath, err)
						return fmt.Errorf("分块上传失败 %s: %w", filePath, err)
					}
					fmt.Fprintf(ios.Out, "成功: %v, 消息: %s\n", result.Success, result.Message)
					if result.FileChecksum != "" {
						fmt.Fprintf(ios.Out, "文件 SHA-256: %s\n", result.FileChecksum)
					}
				} else {
					result, err := svc.Upload(ctx, filePath, remotePath)
					if err != nil {
						fmt.Fprintf(ios.ErrOut, "上传失败: %s %v\n", filePath, err)
						if result != nil {
							fmt.Fprintf(ios.ErrOut, "服务端消息: %s\n", result.Message)
						}
						return fmt.Errorf("上传失败 %s: %w", filePath, err)
					}
					fmt.Fprintf(ios.Out, "成功: %v, 消息: %s\n", result.Success, result.Message)
					if result.Checksum != "" {
						fmt.Fprintf(ios.Out, "文件 SHA-256: %s\n", result.Checksum)
					}
				}
			}
			return nil
		},
	}

	cmd.Flags().Bool("chunked", false, "启用分块上传模式")
	cmd.Flags().Int64("chunk-size", 0, "分块大小 (默认 4MB)")
	cmd.Flags().Int("concurrency", 0, "上传并发数 (默认 4)")
	cmd.Flags().Bool("resume", true, "续传模式")

	return cmd
}
