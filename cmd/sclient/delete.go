// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var deleteCmd = &cobra.Command{
	Use:   "delete <filename>",
	Short: "删除文件",
	Long: `从 sproxy 服务端删除文件。
		filename 可以包含路径，如 "dir/file.txt"。

		默认从远端获取文件 SHA-256 进行校验删除，无需本地文件。
		使用 --check-local 选项指定本地文件路径，在校验本地文件 checksum
		与远端一致后才执行删除，提供额外安全保护。`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			return fmt.Errorf("初始化客户端失败: %w", err)
		}

		filename, err := resolveRemotePathOrErr(args[0])
		if err != nil {
			return err
		}
		localPath, _ := cmd.Flags().GetString("check-local")

		if err := cli.Delete(context.Background(), filename, localPath); err != nil {
			fmt.Fprintf(os.Stderr, "删除失败: %v\n", err)
			return fmt.Errorf("删除失败: %w", err)
		}
		fmt.Printf("文件删除成功: %s\n", filename)
		return nil
	},
}

func init() {
	deleteCmd.Flags().String("check-local", "", "指定本地文件路径，校验其 SHA-256 与远端一致后才执行删除")
}
