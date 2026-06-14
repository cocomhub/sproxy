// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "列出服务器上的文件",
	Long: `列出 sproxy 服务端上的文件。
			默认列出当前目录的顶层文件。`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			return fmt.Errorf("初始化客户端失败: %w", err)
		}

		var subdir string
		if len(args) > 0 {
			subdir = args[0]
		}

		var files []client.FileInfo
		if !strings.HasPrefix(subdir, "/") {
			files, err = cli.List(context.Background(), currentDir, subdir)
		} else {
			files, err = cli.List(context.Background(), subdir)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "列出文件失败: %v\n", err)
			return fmt.Errorf("列出文件失败: %w", err)
		}

		if len(files) == 0 {
			fmt.Println("no files found")
		} else {
			printFileList(files, os.Stdout)
		}
		return nil
	},
}

func init() {
	listCmd.Flags().String("subdir", "", "列出指定子目录下的文件")
}
