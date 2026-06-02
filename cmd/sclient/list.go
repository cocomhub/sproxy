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
默认列出上传根目录的顶层文件；可使用 --subdir 参数列出子目录。
如: sclient list --subdir dir1`,
	Run: func(cmd *cobra.Command, args []string) {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			os.Exit(1)
		}

		subdir, _ := cmd.Flags().GetString("subdir")

		var files []client.FileInfo
		if strings.HasPrefix(subdir, "/") {
			files, err = cli.List(context.Background(), subdir)
		} else if currentDir != "" {
			files, err = cli.List(context.Background(), currentDir, subdir)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "列出文件失败: %v\n", err)
			os.Exit(1)
		}

		if len(files) == 0 {
			fmt.Println("no files found")
		} else {
			for _, f := range files {
				if f.IsDir {
					fmt.Printf("%-40s  %10s  %s\n", f.Name+"/", "-", "-")
				} else {
					csPrefix := f.Checksum
					if len(csPrefix) > 16 {
						csPrefix = csPrefix[:16] + "…"
					}
					if csPrefix == "" {
						csPrefix = "-"
					}
					fmt.Printf("%-40s  %10s  %s\n", f.Name, client.FormatByte(float64(f.Size)), csPrefix)
				}
			}
		}
	},
}

func init() {
	listCmd.Flags().String("subdir", "", "列出指定子目录下的文件")
}
