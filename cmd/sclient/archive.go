// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var archiveCmd = &cobra.Command{
	Use:   "archive [flags] <file...>",
	Short: "将服务端文件打包下载为 tar.gz",
	Long: `将服务端上指定文件打包为 tar.gz 下载到本地。

	文件名会保留远端目录结构（如有），本地保存时保持相对路径不变。`,
	Example: `  sclient archive report.pdf logs/app.log
  sclient archive -o backup.tar.gz file1.txt dir/file2.txt`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			return fmt.Errorf(errFmtInitClient, err)
		}

		output, _ := cmd.Flags().GetString("output")
		if output == "" {
			output = "archive.tar.gz"
		}

		if err := cli.Archive(context.Background(), args, output); err != nil {
			fmt.Fprintf(os.Stderr, "打包失败: %v\n", err)
			return fmt.Errorf("打包失败: %w", err)
		}
		fmt.Printf("打包完成: %s\n", output)
		return nil
	},
}

var archiveDirCmd = &cobra.Command{
	Use:   "archive-dir [flags] <dirname>",
	Short: "将服务端整个目录打包下载为 tar.gz",
	Long:  `将服务端上整个目录打包为 tar.gz 下载到本地。`,
	Example: `  sclient archive-dir myfolder
  sclient archive-dir -o myfolder.tar.gz logs`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			return fmt.Errorf(errFmtInitClient, err)
		}

		output, _ := cmd.Flags().GetString("output")
		if output == "" {
			output = args[0] + ".tar.gz"
		}

		if err := cli.ArchiveDir(context.Background(), args[0], output); err != nil {
			fmt.Fprintf(os.Stderr, "目录打包失败: %v\n", err)
			return fmt.Errorf("目录打包失败: %w", err)
		}
		fmt.Printf("目录打包完成: %s\n", output)
		return nil
	},
}

// writeArchiveResponse writes HTTP response body to a file.
func writeArchiveResponse(resp *http.Response, outputPath string) error {
	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建输出文件失败: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	return nil
}

func init() {
	archiveCmd.Flags().StringP("output", "o", "", "输出文件路径（默认 archive.tar.gz）")
	archiveDirCmd.Flags().StringP("output", "o", "", "输出文件路径（默认 <dirname>.tar.gz）")

	rootCmd.AddCommand(archiveCmd)
	rootCmd.AddCommand(archiveDirCmd)
}

// ensure unused import suppression for tools that only reference via archive.go
var _ = bufio.NewReader
var _ = io.Copy
var _ = http.MethodGet
var _ = writeArchiveResponse
var _ = client.FileInfo{}
