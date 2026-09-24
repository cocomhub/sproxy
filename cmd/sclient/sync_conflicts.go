// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// resolveStrategy 是 sync conflicts resolve 的策略枚举（对齐服务端 choice）。
const (
	strategyOurs   = "ours"
	strategyTheirs = "theirs"
	strategyManual = "manual"
)

// newCmdSyncConflicts 创建 `sync conflicts` 子命令族（list/resolve）。
// 服务端冲突索引（ConflictIndex，/api/sync/conflicts）登记 merge3 未自动合并的冲突；
// list 列出未解决冲突，resolve 按策略（ours/theirs/manual）提交解决并写回文件。
func newCmdSyncConflicts(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conflicts",
		Short: "同步冲突列表与解决（list/resolve）",
		Long: `查看与解决同步冲突（服务端 ConflictIndex 登记的未合并冲突）。

  sync conflicts list                 # 列出未解决冲突（ID/path/类型/时间）
  sync conflicts resolve <id> --strategy ours|theirs|manual [--content <text>]

resolve 提交解决策略并写回冲突文件：ours/theirs 取对应侧快照写回；manual 用 --content
显式内容写回（文本冲突场景；二进制内容的 manual 解决留待 --file/stdin 扩展）。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newCmdSyncConflictsList(factory, ios))
	cmd.AddCommand(newCmdSyncConflictsResolve(factory, ios))
	return cmd
}

// newCmdSyncConflictsList 创建 `sync conflicts list`：列出未解决冲突。
func newCmdSyncConflictsList(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出未解决冲突",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			items, err := svc.ListSyncConflicts(cmd.Context())
			if err != nil {
				return fmt.Errorf("获取同步冲突列表失败: %w", err)
			}
			jsonOut, _ := cmd.Flags().GetBool("json")
			if jsonOut {
				enc := json.NewEncoder(ios.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"conflicts": items})
			}
			if len(items) == 0 {
				ios.WriteOutLine("无未解决冲突")
				return nil
			}
			ios.WriteOutLine("%-20s  %-40s  %6s  %-10s  %-10s  %s",
				"ID", "PATH", "HUNKS", "OURS_SHA", "THEIRS_SHA", "TS")
			for _, it := range items {
				ts := time.Unix(0, it.Timestamp).Format(time.RFC3339)
				ios.WriteOutLine("%-20s  %-40s  %6d  %-10s  %-10s  %s",
					it.ID, it.Path, it.HunkCount, shortSHA(it.OursSHA), shortSHA(it.TheirsSHA), ts)
			}
			return nil
		},
	}
	return cmd
}

// shortSHA 返回 SHA 的前 8 位（空串原样返回，供表格展示）。
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// syncConflictsResolveOptions 是 resolve 子命令的 flag 集合。
type syncConflictsResolveOptions struct {
	strategy string
	content  string
}

// newCmdSyncConflictsResolve 创建 `sync conflicts resolve <id>`：提交解决策略。
// 纯参数校验 fail fast 在 NewClient 之前（对齐 newCmdSyncDirection 审查 M-2）：
// strategy 非法 / manual 缺 content 本地报错，不出网。
func newCmdSyncConflictsResolve(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var o syncConflictsResolveOptions
	cmd := &cobra.Command{
		Use:   "resolve <id>",
		Short: "解决同步冲突（ours|theirs|manual）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			choice, err := normalizeResolveStrategy(o.strategy)
			if err != nil {
				return err
			}
			if choice == strategyManual && o.content == "" {
				return fmt.Errorf("--strategy manual 需要 --content <文本>（服务端 400 语义前置到 CLI，防半途网络往返）")
			}

			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			if err := svc.ResolveSyncConflict(cmd.Context(), args[0], choice, o.content); err != nil {
				return fmt.Errorf("解决同步冲突失败: %w", err)
			}
			jsonOut, _ := cmd.Flags().GetBool("json")
			if jsonOut {
				enc := json.NewEncoder(ios.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"success": true, "path": args[0]})
			}
			ios.WriteOutLine("已解决冲突 %s（%s）", args[0], choice)
			return nil
		},
	}
	cmd.Flags().StringVar(&o.strategy, "strategy", "", "解决策略：ours（保留我方）/theirs（保留对方）/manual（--content 显式内容）")
	cmd.Flags().StringVar(&o.content, "content", "", "manual 策略的写回内容（ours/theirs 时忽略）")
	return cmd
}

// normalizeResolveStrategy 校验并归一 resolve 策略（ours|theirs|manual）。
// 非法值报错并列出可选值（fail fast，不出网）。
func normalizeResolveStrategy(v string) (string, error) {
	switch v {
	case strategyOurs:
		return strategyOurs, nil
	case strategyTheirs:
		return strategyTheirs, nil
	case strategyManual:
		return strategyManual, nil
	default:
		return "", fmt.Errorf("无效的 --strategy 值 %q，仅支持 ours|theirs|manual", v)
	}
}
