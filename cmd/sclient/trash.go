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
	var volume string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出回收站条目",
		Long: "列出回收站条目。默认**汇总** owner 可见的全部卷；--volume 只看该卷。" +
			"\n\n各卷有独立回收站（软删落文件所在卷）：外部/加密卷（如 secretdata vault）的" +
			"\n条目同样列出（条目带 VOLUME 列）。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			items, err := svc.ListTrash(cmd.Context(), volume)
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
			ios.WriteOutLine("%-50s  %-16s  %s", "TRASH_REL", "VOLUME", "ORIGINAL_NAME")
			for _, it := range items {
				vol := it.Volume
				if vol == "" {
					vol = "-"
				}
				ios.WriteOutLine("%-50s  %-16s  %s", it.TrashRel, vol, it.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&volume, "volume", "", "只看该卷的回收站（缺省 = 全部卷汇总）")
	return cmd
}

// newCmdTrashRestore 创建 trash restore <trash_rel>。
func newCmdTrashRestore(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var volume string
	cmd := &cobra.Command{
		Use:   "restore <trash_rel>",
		Short: "恢复回收站条目（trash_rel 来自 list 输出）",
		Long: "恢复回收站条目。缺省**跨卷定位**（条目只存在于软删时所在卷）；" +
			"\n同名 trash_rel 有多份时用 --volume 指定卷（取自 list 的 VOLUME 列）。",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if err := svc.RestoreTrash(cmd.Context(), args[0], volume); err != nil {
				return err
			}
			ios.WriteOutLine("已恢复 %s", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&volume, "volume", "", "条目所在卷（缺省 = 跨卷定位）")
	return cmd
}

// newCmdTrashEmpty 创建 trash empty（--yes 确认）。
func newCmdTrashEmpty(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var (
		yes    bool
		volume string
		all    bool
	)
	cmd := &cobra.Command{
		Use:   "empty",
		Short: "清空回收站（不可恢复，需 --yes 确认）",
		Long: "清空回收站（不可恢复）。**安全默认只清默认卷**；用 --volume <卷> 清指定卷、" +
			"\n--all 清全部卷（外部/加密卷的软删条目也在此清）。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("清空回收站不可恢复，请加 --yes 确认")
			}
			if all && volume != "" {
				return fmt.Errorf("--all 与 --volume 只能二选一")
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if err := svc.EmptyTrash(cmd.Context(), volume, all); err != nil {
				return err
			}
			switch {
			case all:
				ios.WriteOutLine("全部卷回收站已清空")
			case volume != "":
				ios.WriteOutLine("卷 %s 的回收站已清空", volume)
			default:
				ios.WriteOutLine("回收站已清空（默认卷；--all 清全部卷）")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "确认清空（不可恢复）")
	cmd.Flags().StringVar(&volume, "volume", "", "只清该卷的回收站")
	cmd.Flags().BoolVar(&all, "all", false, "清空**全部卷**的回收站（默认只清默认卷）")
	return cmd
}

// 防未使用（json/time 供未来扩展）。
var _ = time.Now
