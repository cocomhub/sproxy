// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// tree.go 实现 `tree` 命令（Linux 习惯命令集；用户口径 2026-10-14）：递归显示远端目录树。
//
// 卷寻址与其它文件命令**完全一致**：路径写 `<卷名>:/<路径>`（唯一方式，无 --volume flag）；
// 目录与文件同一形式（`tree vault:/a/b`）。
//
// 输出形如：
//
//	main:/a
//	├── b/
//	│   └── c.txt
//	└── x.bin
//
//	1 directory, 2 files
func NewCmdTree(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tree [path]",
		Short: "以树形递归显示目录结构（Linux 习惯；支持卷 URI）",
		Long: `以树形递归显示远端目录结构。

			路径可写卷 URI（如 vault:/a/b），与其它文件命令一致；缺省 = 当前目录（cd 设定的位置）。
			--depth 限制递归深度（0 = 不限）；--dirs-only 只显示目录。`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := applyVolumeContext(cmd, st, args); err != nil {
				return err
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			root, err := treeRoot(st, args)
			if err != nil {
				return err
			}
			depth, _ := cmd.Flags().GetInt("depth")
			dirsOnly, _ := cmd.Flags().GetBool("dirs-only")
			list := func(dir string) ([]client.FileInfo, error) {
				return svc.List(cmd.Context(), dir)
			}
			var st2 treeStats
			fmt.Fprintln(ios.Out, treeRootLabel(root))
			if werr := walkTree(list, root, "", depth, dirsOnly, ios.Out, &st2); werr != nil {
				ios.WriteErrLine("列出目录失败: %v", werr)
				return fmt.Errorf("列出目录失败: %w", werr)
			}
			fmt.Fprintf(ios.Out, "\n%d directories, %d files\n", st2.dirs, st2.files)
			return nil
		},
	}
	cmd.Flags().Int("depth", 0, "最大递归深度（0 = 不限）")
	cmd.Flags().Bool("dirs-only", false, "只显示目录")
	return cmd
}

// treeRoot 解析 tree 的起始目录（位置参数优先，缺省 = 当前目录；返回**绝对远端路径**）。
func treeRoot(st *state.State, args []string) (string, error) {
	if len(args) > 0 {
		return st.ResolveRemotePathOrErr(args[0])
	}
	if st != nil && st.CurrentDir != "" {
		return st.CurrentDir, nil
	}
	return "/", nil
}

// treeRootLabel 是树根显示名（绝对路径；卷名已由命令层解析，故此处不重复卷名）。
func treeRootLabel(root string) string {
	if root == "" {
		return "/"
	}
	return root
}

// treeStats 累计目录/文件数（tree 结尾统计行）。
type treeStats struct{ dirs, files int }

// treeLister 是递归列目录的接缝（生产 = svc.List；测试注入内存树）。
type treeLister func(dir string) ([]client.FileInfo, error)

// walkTree 递归输出目录树。prefix 是当前层的前缀（`│   ` / `    `），depth 为剩余深度
// （0 = 不限）。目录项以 `/` 结尾（与 `ls` 的显示约定一致）。
func walkTree(list treeLister, dir, prefix string, depth int, dirsOnly bool, w io.Writer, stats *treeStats) error {
	entries, err := list(dir)
	if err != nil {
		return err
	}
	sortTreeEntries(entries)
	shown := filterTreeEntries(entries, dirsOnly)
	for i, e := range shown {
		if werr := renderTreeEntry(list, dir, prefix, i == len(shown)-1, depth, dirsOnly, w, stats, e); werr != nil {
			return werr
		}
	}
	return nil
}

// filterTreeEntries 按 --dirs-only 过滤条目（保持顺序）。
func filterTreeEntries(entries []client.FileInfo, dirsOnly bool) []client.FileInfo {
	if !dirsOnly {
		return entries
	}
	out := make([]client.FileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir {
			out = append(out, e)
		}
	}
	return out
}

// renderTreeEntry 渲染单个条目（含分支符号）并在其为目录时递归。
func renderTreeEntry(list treeLister, dir, prefix string, last bool, depth int, dirsOnly bool, w io.Writer, stats *treeStats, e client.FileInfo) error {
	branch, nextPrefix := "├── ", prefix+"│   "
	if last {
		branch, nextPrefix = "└── ", prefix+"    "
	}
	name := e.Name
	if e.IsDir {
		name += "/"
	}
	fmt.Fprintf(w, "%s%s%s\n", prefix, branch, name)
	if !e.IsDir {
		stats.files++
		return nil
	}
	stats.dirs++
	if depth == 1 {
		return nil
	}
	next := 0
	if depth > 1 {
		next = depth - 1
	}
	return walkTree(list, path.Join(dir, e.Name), nextPrefix, next, dirsOnly, w, stats)
}

// sortTreeEntries 目录在前、同类型按名排序（稳定输出）。
func sortTreeEntries(entries []client.FileInfo) {
	// 插入排序足够（单目录条目量级小且服务端已基本有序）；避免引入 sort 依赖的额外比较闭包。
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && treeLess(entries[j], entries[j-1]); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

// treeLess 定义 tree 的显示顺序：目录优先，其次名字（大小写不敏感稳定比较）。
func treeLess(a, b client.FileInfo) bool {
	if a.IsDir != b.IsDir {
		return a.IsDir
	}
	return strings.ToLower(a.Name) < strings.ToLower(b.Name)
}
