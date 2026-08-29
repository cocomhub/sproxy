// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"

	"github.com/cocomhub/buildinfo"
	"github.com/cocomhub/sproxy/internal/buildmeta"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdVersion 创建 version 命令，显示程序二进制版本。
// 移除文件版本管理（迁移至 meta version list|restore|delete）。
func NewCmdVersion(ios cli.IOStreams) *cobra.Command {
	info := buildinfo.Default()
	// Version/BuiltAt 走 main 变量（Makefile -X main.Version/main.BuildAt 注入）；
	// Branch/CommitID/ReleaseURL 走 buildinfo 包级变量（-X buildinfo.* 注入）。
	info.Version = Version
	info.BuiltAt = BuildAt
	info.DirtyInfo = buildmeta.DirtyInfo()
	cmd := buildinfo.NewVersionCmd(info)

	// dirty-info 子命令：输出内嵌 diff（对应 cocom version dirty-info）。
	dirty := &cobra.Command{
		Use:   "dirty-info",
		Short: "显示自上次提交以来的未提交变更",
		Run: func(c *cobra.Command, args []string) {
			_, _ = io.WriteString(ios.Out, buildmeta.DirtyInfo())
		},
	}
	cmd.AddCommand(dirty)
	return cmd
}
