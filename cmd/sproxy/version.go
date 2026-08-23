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
	info := buildinfo.New()
	info.Version = Version
	info.BuiltAt = BuildAt
	info.DirtyInfo = buildmeta.DirtyInfo()
	info.CommitID = "unknown"
	info.Branch = "unknown"
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
