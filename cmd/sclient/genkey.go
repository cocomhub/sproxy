// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/spf13/cobra"
)

// NewCmdGenkey 创建 genkey 命令。
func NewCmdGenkey(ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "genkey",
		Short: "生成 tunnel_key 密钥",
		Run: func(cmd *cobra.Command, args []string) {
			key, err := tunnel.GenerateKey()
			if err != nil {
				ios.WriteErrLine("生成密钥失败: %v", err)
				return
			}
			ios.WriteOutLine(key)
		},
	}
}
