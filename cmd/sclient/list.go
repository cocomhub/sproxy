// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
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
			return fmt.Errorf(errFmtInitClient, err)
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

		fm := buildFormatter(cmd)
		if len(files) == 0 {
			fm.Println("no files found")
		} else {
			fm.PrintFileList(files)
		}
		return nil
	},
}

// NewCmdList 创建独立的 list 命令工厂函数，使用 factory 创建客户端。
func NewCmdList(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出服务器上的文件",
		Long: `列出 sproxy 服务端上的文件。
				默认列出当前目录的顶层文件。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			var subdir string
			if len(args) > 0 {
				subdir = args[0]
			}

			var files []client.FileInfo
			if !strings.HasPrefix(subdir, "/") {
				files, err = svc.List(cmd.Context(), st.CurrentDir, subdir)
			} else {
				files, err = svc.List(cmd.Context(), subdir)
			}
			if err != nil {
				ios.WriteErrLine("列出文件失败: %v", err)
				return fmt.Errorf("列出文件失败: %w", err)
			}

			fm := buildFormatterWithWriter(ios.Out, cmd)
			if len(files) == 0 {
				fm.Println("no files found")
			} else {
				fm.PrintFileList(files)
			}
			return nil
		},
	}
	cmd.Flags().String("subdir", "", "列出指定子目录下的文件")
	return cmd
}
