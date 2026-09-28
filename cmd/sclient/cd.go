// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// ---- 工厂函数 ----

// NewCmdCd 创建独立的 cd 命令工厂函数，使用 state.State 替代全局 currentDir。
func NewCmdCd(st *state.State, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "cd [path]",
		Short: "切换当前目录",
		Long: `切换当前操作目录，后续 upload/download/list/delete 等命令将以此目录为基准。
		cd 带参数时进入指定子目录，无参数时打印当前目录。
		cd / 回到根目录，cd .. 返回上级目录。`,
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) == 0 {
				printCurrentDirPath(st, ios)
				return
			}
			path := args[0]
			if cdApplySpecialPath(st, ios, path) {
				return
			}
			cdApplyNormalPath(st, ios, path)
		},
	}
}

// printCurrentDirPath 打印当前目录（空表示根，带前导 /）。
func printCurrentDirPath(st *state.State, ios cli.IOStreams) {
	if st.CurrentDir == "" {
		ios.WriteOutLine("/")
	} else {
		ios.WriteOutLine("/%s", st.CurrentDir)
	}
}

// cdApplySpecialPath 处理 cd 的特殊路径（/、.、..）；返回 true 表示已处理（调用方直接返回）。
func cdApplySpecialPath(st *state.State, ios cli.IOStreams, path string) bool {
	switch path {
	case "/":
		st.CurrentDir = ""
		saveCurrentDirValue(st.CurrentDir)
	case ".":
		return true
	case "..":
		if st.CurrentDir == "" {
			return true
		}
		parts := strings.Split(st.CurrentDir, "/")
		if len(parts) <= 1 {
			st.CurrentDir = ""
		} else {
			st.CurrentDir = strings.Join(parts[:len(parts)-1], "/")
		}
		saveCurrentDirValue(st.CurrentDir)
		return true
	default:
		return false
	}
	return true
}

// cdApplyNormalPath 处理普通相对路径：拼接当前目录、清理、拒绝越界后写入目标目录。
func cdApplyNormalPath(st *state.State, ios cli.IOStreams, path string) {
	newDir := path
	if st.CurrentDir != "" {
		newDir = st.CurrentDir + "/" + path
	}
	cleaned := filepath.ToSlash(filepath.Clean(newDir))
	if cleaned == "." {
		cleaned = ""
	}
	if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "../") {
		ios.WriteErrLine("无效的路径")
		return
	}
	st.CurrentDir = cleaned
	saveCurrentDirValue(st.CurrentDir)
}

// NewCmdPwd 创建独立的 pwd 命令工厂函数。
func NewCmdPwd(st *state.State, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "pwd",
		Short: "打印当前目录",
		Run: func(cmd *cobra.Command, args []string) {
			if st.CurrentDir == "" {
				ios.WriteOutLine("/")
			} else {
				ios.WriteOutLine("/%s", st.CurrentDir)
			}
		},
	}
}

// NewCmdMkdir 创建独立的 mkdir 命令工厂函数。
func NewCmdMkdir(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "mkdir <dirname>",
		Short: "在服务端创建目录",
		Long:  "在服务端上传目录下创建指定子目录。路径相对当前目录 (cd)，支持绝对路径 (/开头)。",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			dirname, err := st.ResolveRemotePath(args[0])
			if err != nil {
				ios.WriteErrLine("无效的路径: %v", err)
				return fmt.Errorf(errFmtInvalidPath, err)
			}
			if err := svc.Mkdir(cmd.Context(), dirname); err != nil {
				ios.WriteErrLine("创建目录失败: %v", err)
				return fmt.Errorf(errFmtMkdirFailed, err)
			}
			fmt.Fprintf(ios.Out, "目录已创建: %s\n", dirname)
			return nil
		},
	}
}

// NewCmdRmdir 创建独立的 rmdir 命令工厂函数。
func NewCmdRmdir(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rmdir <dirname>",
		Short: "删除服务端目录",
		Long:  "删除服务端上传目录下的指定目录（含所有内容）。路径相对当前目录。\n使用 --force 跳过确认提示。",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			dirname, err := st.ResolveRemotePath(args[0])
			if err != nil {
				ios.WriteErrLine("无效的路径: %v", err)
				return fmt.Errorf(errFmtInvalidPath, err)
			}

			entries, listErr := svc.List(cmd.Context(), dirname)
			force, _ := cmd.Flags().GetBool("force")

			if listErr == nil && len(entries) > 0 && !force {
				fmt.Fprintf(ios.ErrOut, "警告: 目录 '%s' 包含 %d 个条目，非空删除将清除所有内容\n", dirname, len(entries))
				fmt.Fprint(ios.ErrOut, "确认删除? (y/N): ")
				reader := bufio.NewReader(ios.In)
				answer, _ := reader.ReadString('\n')
				answer = strings.TrimSpace(strings.ToLower(answer))
				if answer != "y" && answer != "yes" {
					fmt.Fprintln(ios.Out, "已取消")
					return nil
				}
			}

			if err := svc.Rmdir(cmd.Context(), dirname); err != nil {
				ios.WriteErrLine("删除目录失败: %v", err)
				return fmt.Errorf("删除目录失败: %w", err)
			}
			fmt.Fprintf(ios.Out, "目录已删除: %s\n", dirname)
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "跳过非空确认提示")
	return cmd
}

// ---- XDG 缓存持久化 ----

const cacheDirName = "sproxy"
const cacheFile = "current_dir"

// saveCurrentDirValue 将指定目录持久化到 XDG 缓存目录。
func saveCurrentDirValue(dir string) {
	cachePath, err := xdg.CacheFile(filepath.Join(cacheDirName, cacheFile))
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(cachePath), 0755)
	_ = os.WriteFile(cachePath, []byte(dir), 0644)
}

// loadCurrentDir 从 XDG 缓存目录加载当前目录。
func loadCurrentDir() string {
	cachePath, err := xdg.CacheFile(filepath.Join(cacheDirName, cacheFile))
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
