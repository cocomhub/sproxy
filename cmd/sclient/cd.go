// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var cdCmd = &cobra.Command{
	Use:   "cd [path]",
	Short: "切换当前目录",
	Long: `切换当前操作目录，后续 upload/download/list/delete 等命令将以此目录为基准。
cd 带参数时进入指定子目录，无参数时打印当前目录。
cd / 回到根目录，cd .. 返回上级目录。`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) == 0 {
			// 无参数：打印当前目录
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
			return
		case ".":
			return
		case "..":
			if currentDir == "" {
				return // 已在根目录
			}
			// 回退一级
			parts := strings.SplitN(currentDir, "/", -1)
			if len(parts) <= 1 {
				currentDir = ""
			} else {
				currentDir = strings.Join(parts[:len(parts)-1], "/")
			}
			return
		}

		// 拼接当前目录
		newDir := path
		if currentDir != "" {
			newDir = currentDir + "/" + path
		}
		// 规范化
		cleaned := filepath.ToSlash(filepath.Clean(newDir))
		if cleaned == "." {
			cleaned = ""
		}
		if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "../") {
			fmt.Fprintln(os.Stderr, "无效的路径")
			return
		}
		currentDir = cleaned
	},
}

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

func init() {
	// cd 和 pwd 注册到 rootCmd
	rootCmd.AddCommand(cdCmd)
	rootCmd.AddCommand(pwdCmd)
}

// resolveRemotePath 根据当前目录和用户传入的路径，返回完整的远端路径。
// 若用户传入绝对路径（以 / 开头）或包含 ..，直接使用用户路径；
// 否则拼接 currentDir。
func resolveRemotePath(userPath string) string {
	if userPath == "" {
		return currentDir
	}
	if strings.HasPrefix(userPath, "/") {
		// 绝对路径：去掉前导 /
		return filepath.ToSlash(filepath.Clean(userPath[1:]))
	}
	if strings.HasPrefix(userPath, "..") || strings.Contains(userPath, "../") {
		// 包含 .. 的路径直接使用
		return filepath.ToSlash(filepath.Clean(userPath))
	}
	if currentDir != "" {
		return filepath.ToSlash(filepath.Clean(currentDir + "/" + userPath))
	}
	return filepath.ToSlash(filepath.Clean(userPath))
}
