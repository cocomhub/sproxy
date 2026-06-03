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

var batchDeleteCmd = &cobra.Command{
	Use:   "batch-delete <file1> [file2...]",
	Short: "批量删除文件",
	Long: `批量删除 sproxy 服务端上的多个文件。
每个文件会先通过 Stat 获取远端的 checksum 用于校验。

示例：
  sclient batch-delete a.txt b.txt dir/file.txt`,
	Args: cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			os.Exit(1)
		}

		ctx := context.Background()
		var files []client.BatchDeleteFile
		for _, name := range args {
			remote := mustResolveRemotePath(name)
			// 先通过 Stat 获取 checksum
			info, statErr := cli.Stat(ctx, remote)
			if statErr != nil {
				fmt.Fprintf(os.Stderr, "获取 %s 信息失败: %v\n", remote, statErr)
				os.Exit(1)
			}
			files = append(files, client.BatchDeleteFile{
				Filename: remote,
				Checksum: info.Checksum,
			})
		}

		results, err := cli.BatchDelete(ctx, files)
		if err != nil {
			fmt.Fprintf(os.Stderr, "批量删除失败: %v\n", err)
			os.Exit(1)
		}
		for _, r := range results {
			status := "OK"
			if !r.Success {
				status = "FAIL"
			}
			fmt.Printf("[%s] %s: %s\n", status, r.Filename, r.Message)
		}
	},
}

func init() {
	rootCmd.AddCommand(batchDeleteCmd)
}
