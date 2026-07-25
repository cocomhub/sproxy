// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// ---- 工厂函数 ----

// NewCmdMv 创建独立的 mv 命令工厂函数，使用 state.State 替代全局 currentDir。
func NewCmdMv(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "mv <from> <to>",
		Short: "重命名 / 移动远端文件",
		Long: `重命名或移动 sproxy 服务端上的文件。

		服务端会先校验源文件的 SHA-256（避免在并发写入下误覆盖），然后执行 rename。
		目标父目录不存在时自动 mkdir -p；目标已存在时返回 409。

		from 和 to 都受当前目录 (cd) 影响：相对路径自动拼接前缀，绝对路径 (/开头) 绕过。

		示例:
		  sclient mv old.txt new.txt
		  sclient mv old.txt sub/dir/new.txt
		  sclient mv /a/b.txt /c/b.txt`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			from, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			to, err := st.ResolveRemotePathOrErr(args[1])
			if err != nil {
				return err
			}

			ctx := cmd.Context()

			info, err := svc.Stat(ctx, from)
			if err != nil {
				ios.WriteErrLine("获取源文件信息失败: %v", err)
				return fmt.Errorf("获取源文件信息失败: %w", err)
			}
			if info.Checksum == "" {
				return fmt.Errorf("源文件 checksum 为空，无法重命名")
			}

			if err := svc.Rename(ctx, from, to, info.Checksum); err != nil {
				ios.WriteErrLine("重命名失败: %v", err)
				return fmt.Errorf("重命名失败: %w", err)
			}
			fmt.Fprintf(ios.Out, "已重命名: %s -> %s\n", from, to)
			return nil
		},
	}
}

// ---- 旧版全局命令（待迁移） ----

var mvCmd = &cobra.Command{
	Use:   "mv <from> <to>",
	Short: "重命名 / 移动远端文件",
	Long: `重命名或移动 sproxy 服务端上的文件。

		服务端会先校验源文件的 SHA-256（避免在并发写入下误覆盖），然后执行 rename。
		目标父目录不存在时自动 mkdir -p；目标已存在时返回 409。

		from 和 to 都受当前目录 (cd) 影响：相对路径自动拼接前缀，绝对路径 (/开头) 绕过。

		示例:
		  sclient mv old.txt new.txt
		  sclient mv old.txt sub/dir/new.txt
		  sclient mv /a/b.txt /c/b.txt`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
			return fmt.Errorf(errFmtInitClient, err)
		}

		from, err := resolveRemotePathOrErr(args[0])
		if err != nil {
			return err
		}
		to, err := resolveRemotePathOrErr(args[1])
		if err != nil {
			return err
		}

		ctx := context.Background()

		info, err := cli.Stat(ctx, from)
		if err != nil {
			fmt.Fprintf(os.Stderr, "获取源文件信息失败: %v\n", err)
			return fmt.Errorf("获取源文件信息失败: %w", err)
		}
		if info.Checksum == "" {
			return fmt.Errorf("源文件 checksum 为空，无法重命名")
		}

		if err := cli.Rename(ctx, from, to, info.Checksum); err != nil {
			fmt.Fprintf(os.Stderr, "重命名失败: %v\n", err)
			return fmt.Errorf("重命名失败: %w", err)
		}
		fmt.Printf("已重命名: %s -> %s\n", from, to)
		return nil
	},
}
