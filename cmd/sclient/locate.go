// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdLocate 创建 locate 命令：按**内容 sha256** 反查卷内路径。
//
// 为什么需要（2026-11-17）：上传结果/manifest 里的 rel 是**快照**，文件事后可能被
// **改名纠正**（`CODE.mp4` → `CODE 日文名.mp4`）或**目录搬移** ⇒ 用旧 rel 做 stat 会 not found。
// 按内容反查即可稳定判定"同一内容现在在哪"（用于重入校验后删除本地原件等场景）。
func NewCmdLocate(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{SilenceUsage: true, SilenceErrors: true,
		Use:   "locate <sha256>",
		Short: "按内容 sha256 反查卷内路径（解耦改名/搬移）",
		Args:  cobra.ExactArgs(1),
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, cerr := factory.NewClient(cmd)
		if cerr != nil {
			return cerr
		}
		paths, supported, lerr := c.LocateByChecksum(cmd.Context(), args[0])
		if lerr != nil {
			return lerr
		}
		if !supported {
			ios.WriteErrLine("服务端未装配按内容反查能力（不支持，非无命中）")
			return nil
		}
		if len(paths) == 0 {
			ios.WriteOutLine("no files found")
			return nil
		}
		for _, p := range paths {
			ios.WriteOutLine("%s", p)
		}
		return nil
	}
	return cmd
}
