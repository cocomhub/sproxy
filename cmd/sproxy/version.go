// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"

	"github.com/cocomhub/buildinfo"
	"github.com/cocomhub/sproxy/internal/buildmeta"
	"github.com/spf13/cobra"
)

// NewVersionSubcommand 创建 version 子命令，输出程序二进制版本与构建元信息。
func NewVersionSubcommand() *cobra.Command {
	info := buildinfo.Default()
	// Version/BuiltAt 走 main 变量（Makefile -X main.Version/main.BuildAt 注入）；
	// Branch/CommitID/ReleaseURL 走 buildinfo 包级变量（-X buildinfo.* 注入）。
	info.Version = Version
	info.BuiltAt = BuildAt
	info.DirtyInfo = buildmeta.DirtyInfo()
	cmd := buildinfo.NewVersionCmd(info)
	dirty := &cobra.Command{
		Use:   "dirty-info",
		Short: "显示自上次提交以来的未提交变更",
		Run: func(c *cobra.Command, args []string) {
			_, _ = io.WriteString(c.OutOrStdout(), buildmeta.DirtyInfo())
		},
	}
	cmd.AddCommand(dirty)
	return cmd
}
