// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrg/xdg"
	"github.com/spf13/cobra"
)

func init() {
	rmdirCmd.Flags().Bool("force", false, "跳过非空确认提示")

	rootCmd.AddCommand(cdCmd)
	rootCmd.AddCommand(pwdCmd)
	rootCmd.AddCommand(mkdirCmd)
	rootCmd.AddCommand(rmdirCmd)
}

// ---- cd 命令 ----

var cdCmd = &cobra.Command{
	Use:   "cd [path]",
	Short: "切换当前目录",
	Long: `切换当前操作目录，后续 upload/download/list/delete 等命令将以此目录为基准。
cd 带参数时进入指定子目录，无参数时打印当前目录。
cd / 回到根目录，cd .. 返回上级目录。`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) == 0 {
			if currentDir == "" {
				fmt.Println("/")
			} else {
				fmt.Println("/" + currentDir)
			}
			return
		}

		path := args[0]
		switch path {
		case "/":
			currentDir = ""
			saveCurrentDir()
			return
		case ".":
			return
		case "..":
			if currentDir == "" {
				return
			}
			parts := strings.SplitN(currentDir, "/", -1)
			if len(parts) <= 1 {
				currentDir = ""
			} else {
				currentDir = strings.Join(parts[:len(parts)-1], "/")
			}
			saveCurrentDir()
			return
		}

		newDir := path
		if currentDir != "" {
			newDir = currentDir + "/" + path
		}
		cleaned := filepath.ToSlash(filepath.Clean(newDir))
		if cleaned == "." {
			cleaned = ""
		}
		if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "../") {
			fmt.Fprintln(os.Stderr, "无效的路径")
			return
		}
		currentDir = cleaned
		saveCurrentDir()
	},
}

// ---- pwd 命令 ----

var pwdCmd = &cobra.Command{
	Use:   "pwd",
	Short: "打印当前目录",
	Run: func(cmd *cobra.Command, args []string) {
		if currentDir == "" {
			fmt.Println("/")
		} else {
			fmt.Println("/" + currentDir)
		}
	},
}

// ---- mkdir 命令 ----

var mkdirCmd = &cobra.Command{
	Use:   "mkdir <dirname>",
	Short: "在服务端创建目录",
	Long:  "在服务端上传目录下创建指定子目录。路径相对当前目录 (cd)，支持绝对路径 (/开头)。",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			os.Exit(1)
		}

		dirname := resolveRemotePath(args[0])
		if err := cli.Mkdir(context.TODO(), dirname); err != nil {
			fmt.Fprintf(os.Stderr, "创建目录失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("目录已创建: %s\n", dirname)
	},
}

// ---- rmdir 命令 ----

var rmdirCmd = &cobra.Command{
	Use:   "rmdir <dirname>",
	Short: "删除服务端目录",
	Long:  "删除服务端上传目录下的指定目录（含所有内容）。路径相对当前目录。\n使用 --force 跳过确认提示。",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			os.Exit(1)
		}

		dirname := resolveRemotePath(args[0])

		// 检查目录是否为空：先 list 子目录
		entries, listErr := cli.ListSubdir(context.TODO(), dirname)
		force, _ := cmd.Flags().GetBool("force")

		if listErr == nil && len(entries) > 0 && !force {
			fmt.Fprintf(os.Stderr, "警告: 目录 '%s' 包含 %d 个条目，非空删除将清除所有内容\n", dirname, len(entries))
			fmt.Fprint(os.Stderr, "确认删除? (y/N): ")
			reader := bufio.NewReader(os.Stdin)
			answer, _ := reader.ReadString('\n')
			answer = strings.TrimSpace(strings.ToLower(answer))
			if answer != "y" && answer != "yes" {
				fmt.Println("已取消")
				return
			}
		}

		if err := cli.Rmdir(context.TODO(), dirname); err != nil {
			fmt.Fprintf(os.Stderr, "删除目录失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("目录已删除: %s\n", dirname)
	},
}

func init() {
	rmdirCmd.Flags().Bool("force", false, "跳过非空确认提示")

	rootCmd.AddCommand(cdCmd)
	rootCmd.AddCommand(pwdCmd)
	rootCmd.AddCommand(mkdirCmd)
	rootCmd.AddCommand(rmdirCmd)
}

// ---- XDG 缓存持久化 ----

const cacheDirName = "sproxy"
const cacheFile = "current_dir"

// saveCurrentDir 将当前目录持久化到 XDG 缓存目录。
func saveCurrentDir() {
	cachePath, err := xdg.CacheFile(filepath.Join(cacheDirName, cacheFile))
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(cachePath), 0755)
	_ = os.WriteFile(cachePath, []byte(currentDir), 0644)
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
// 若用户传入绝对路径（以 / 开头）或包含 ..，直接使用用户路径；
// 否则拼接 currentDir。
func resolveRemotePath(userPath string) string {
	if userPath == "" {
		return currentDir
	}
	if strings.HasPrefix(userPath, "/") {
		return filepath.ToSlash(filepath.Clean(userPath[1:]))
	}
	if strings.HasPrefix(userPath, "..") || strings.Contains(userPath, "../") {
		return filepath.ToSlash(filepath.Clean(userPath))
	}
	if currentDir != "" {
		return filepath.ToSlash(filepath.Clean(currentDir + "/" + userPath))
	}
	return filepath.ToSlash(filepath.Clean(userPath))
}
