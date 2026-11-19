// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdVolumeOrphans 创建 `volume orphans` 命令：**只读**列出卷内未被引用的分块。
//
// 用途（用户 2026-11-18）：清理前先出清单、人工逐条判断（`GC()` 会直接删除，不适合评审）。
// 服务端未实现该能力 ⇒ 明确提示「不支持（非无孤儿）」；有在途写入 ⇒ 服务端 503。
func NewCmdVolumeOrphans(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var volume string
	cmd := &cobra.Command{SilenceUsage: true, SilenceErrors: true,
		Use:   "orphans",
		Short: "只读列出卷内未被引用的分块（不删除）",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringVar(&volume, "volume", "", "卷名（必填）")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, cerr := factory.NewClient(cmd)
		if cerr != nil {
			return cerr
		}
		raw, supported, aerr := c.VolumeOrphans(cmd.Context(), volume)
		if aerr != nil {
			return aerr
		}
		if !supported {
			return fmt.Errorf("该卷不支持孤儿审计（不支持，非无孤儿）")
		}
		ios.WriteOutLine("%s", string(raw))
		return nil
	}
	return cmd
}
