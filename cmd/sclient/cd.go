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
				if st.CurrentDir == "" {
					ios.WriteOutLine("/")
				} else {
					ios.WriteOutLine("/%s", st.CurrentDir)
				}
				return
			}

			path := args[0]
			switch path {
			case "/":
				st.CurrentDir = ""
				saveCurrentDirValue(st.CurrentDir)
				return
			case ".":
				return
			case "..":
				if st.CurrentDir == "" {
					return
				}
				parts := strings.Split(st.CurrentDir, "/")
				if len(parts) <= 1 {
					st.CurrentDir = ""
				} else {
					st.CurrentDir = strings.Join(parts[:len(parts)-1], "/")
				}
				saveCurrentDirValue(st.CurrentDir)
				return
			}

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
		},
	}
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
				ios.WriteErrLine("初始化客户端失败: %v", err)
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
				ios.WriteErrLine("初始化客户端失败: %v", err)
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

// saveCurrentDir 将当前目录持久化到 XDG 缓存目录。
func saveCurrentDir() {
	saveCurrentDirValue(currentDir)
}

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
func loadCurrentDir() {
	cachePath, err := xdg.CacheFile(filepath.Join(cacheDirName, cacheFile))
	if err != nil {
		return
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return
	}
	currentDir = strings.TrimSpace(string(data))
}

// ---- 远端路径解析 ----

// resolveRemotePath 根据当前目录和用户传入的路径，返回完整的远端路径。
// 若用户传入绝对路径（以 / 开头）：直接使用清洗后的路径（脱掉前导 /）；
// 否则拼接 currentDir。
func resolveRemotePath(userPath string) (string, error) {
	if userPath == "" {
		return currentDir, nil
	}

	var raw string
	if strings.HasPrefix(userPath, "/") {
		raw = userPath[1:]
	} else if currentDir != "" {
		raw = currentDir + "/" + userPath
	} else {
		raw = userPath
	}

	cleaned := filepath.ToSlash(filepath.Clean(raw))
	if cleaned == "." {
		cleaned = ""
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("路径包含父级引用 '..'，禁止访问上层目录: %s", userPath)
	}
	return cleaned, nil
}

// resolveRemotePathOrErr 是 resolveRemotePath 的便捷封装，供 RunE 命令使用。
// 路径校验失败时返回 error。
func resolveRemotePathOrErr(userPath string) (string, error) {
	cleaned, err := resolveRemotePath(userPath)
	if err != nil {
		return "", fmt.Errorf(errFmtInvalidPath, err)
	}
	return cleaned, nil
}
