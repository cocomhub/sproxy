// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdList 创建独立的 list 命令工厂函数，使用 factory 创建客户端。
func NewCmdList(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出服务器上的文件",
		Long: `列出 sproxy 服务端上的文件。
				默认列出当前目录的顶层文件。
				使用 --offset 和 --limit 参数进行分页查询。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			subdir := resolveListSubdir(args)

			files, err := listFetchFiles(cmd, svc, st, subdir)
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
	cmd.Flags().Int("offset", 0, "分页偏移量（从 0 开始）")
	cmd.Flags().Int("limit", 0, "分页返回条数上限（0 表示不限制）")
	return cmd
}

// resolveListSubdir 解析列目录目标：首个位置参数优先，缺省为空（顶层）。
func resolveListSubdir(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// listFetchFiles 按分页标记拉取文件列表：分页用 ListWithPagination，否则用 List；
// / 开头的 absolute subdir 绕过当前目录。返回原始错误由调用方包装。
func listFetchFiles(cmd *cobra.Command, svc *client.FileClient, st *state.State, subdir string) ([]client.FileInfo, error) {
	offset, _ := cmd.Flags().GetInt("offset")
	limit, _ := cmd.Flags().GetInt("limit")
	usePagination := offset > 0 || limit > 0
	var files []client.FileInfo
	var err error
	if usePagination {
		if !strings.HasPrefix(subdir, "/") {
			files, _, err = svc.ListWithPagination(cmd.Context(), offset, limit, st.CurrentDir, subdir)
		} else {
			files, _, err = svc.ListWithPagination(cmd.Context(), offset, limit, subdir)
		}
	} else {
		if !strings.HasPrefix(subdir, "/") {
			files, err = svc.List(cmd.Context(), st.CurrentDir, subdir)
		} else {
			files, err = svc.List(cmd.Context(), subdir)
		}
	}
	if err != nil {
		return nil, err
	}
	return files, nil
}
