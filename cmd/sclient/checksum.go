// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdChecksum 创建 checksum 命令族（台账运维面）。
func NewCmdChecksum(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checksum",
		Short: "checksum 台账运维（从对象 meta 重建）",
	}
	cmd.AddCommand(NewCmdChecksumRebuild(factory, ios))
	return cmd
}

// NewCmdChecksumRebuild 创建 `checksum rebuild` 命令：从对象 meta sidecar 的 TotalSHA256
// 重建 checksum 台账（台账部分缺失/被裁剪时用；台账全空时 list 会自动重建，无需手动执行）。
func NewCmdChecksumRebuild(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var owner string
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "从对象 meta 重建 checksum 台账（可指定 --owner）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			n, rerr := svc.RebuildChecksums(cmd.Context(), owner)
			if rerr != nil {
				return rerr
			}
			ios.WriteOutLine(fmt.Sprintf("checksum 台账已重建：%d 条", n))
			return nil
		},
	}
	cmd.Flags().StringVar(&owner, "owner", "", "目标 owner（默认 = 当前请求主体）")
	return cmd
}
