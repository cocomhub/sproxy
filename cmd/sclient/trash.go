// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// trash.go 实现 `trash list|restore <trash_rel>|empty` 子命令（roadmap 11.8-A2）：
// 服务端 /api/trash 三端点封装（回收站软删除的管理面）。

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// newCmdTrash 创建 trash 命令族（list/restore/empty）。
func newCmdTrash(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "回收站管理（list/restore/empty）",
		Long: `查看与操作回收站（软删除的文件，防误删可恢复）。

  trash list                  # 列出回收站条目（trash_rel 令牌 + 原文件名）
  trash restore <trash_rel>   # 恢复指定条目（trash_rel 来自 list 输出）
  trash empty                 # 清空回收站（不可恢复，需确认 --yes）`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newCmdTrashList(factory, ios))
	cmd.AddCommand(newCmdTrashRestore(factory, ios))
	cmd.AddCommand(newCmdTrashEmpty(factory, ios))
	return cmd
}

// newCmdTrashList 创建 trash list。
func newCmdTrashList(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出回收站条目",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			items, err := svc.ListTrash(cmd.Context())
			if err != nil {
				return err
			}
			jsonOut, _ := cmd.Flags().GetBool("json")
			if jsonOut {
				enc := json.NewEncoder(ios.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"entries": items})
			}
			if len(items) == 0 {
				ios.WriteOutLine("回收站为空")
				return nil
			}
			ios.WriteOutLine("%-50s  %s", "TRASH_REL", "ORIGINAL_NAME")
			for _, it := range items {
				ios.WriteOutLine("%-50s  %s", it.TrashRel, it.Name)
			}
			return nil
		},
	}
}

// newCmdTrashRestore 创建 trash restore <trash_rel>。
func newCmdTrashRestore(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <trash_rel>",
		Short: "恢复回收站条目（trash_rel 来自 list 输出）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if err := svc.RestoreTrash(cmd.Context(), args[0]); err != nil {
				return err
			}
			ios.WriteOutLine("已恢复 %s", args[0])
			return nil
		},
	}
}

// newCmdTrashEmpty 创建 trash empty（--yes 确认）。
func newCmdTrashEmpty(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "empty",
		Short: "清空回收站（不可恢复，需 --yes 确认）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("清空回收站不可恢复，请加 --yes 确认")
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if err := svc.EmptyTrash(cmd.Context()); err != nil {
				return err
			}
			ios.WriteOutLine("回收站已清空")
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "确认清空（不可恢复）")
	return cmd
}

// 防未使用（json/time 供未来扩展）。
var _ = time.Now
