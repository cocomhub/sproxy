// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/spf13/cobra"
)

// NewCmdGenkey 创建 genkey 命令。
func NewCmdGenkey(ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "genkey",
		Short: "生成 64 hex 随机 AES-256 密钥（自检/手动构造用）",
		// 独立执行（单测）时与经根命令执行一致：不转储 Usage、错误交由入口打印。
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := tunnel.GenerateKey()
			if err != nil {
				// 保留稳定 stderr 文案；返回错误以取得非零退出码（main 不再重复打印）。
				ios.WriteErrLine("生成密钥失败: %v", err)
				return fmt.Errorf("生成密钥失败: %w", err)
			}
			ios.WriteOutLine(key)
			return nil
		},
	}
}
