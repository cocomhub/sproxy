// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// quota.go 实现 `quota` 子命令（roadmap 11.8-A3）：展示本 owner 配额水位
// （/api/stats quota 段：usage/max_bytes/watermark）。

import (
	"encoding/json"
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// newCmdQuota 创建 quota 命令。
func newCmdQuota(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "quota",
		Short: "查看本 owner 配额水位（usage/max/watermark）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			quota, err := svc.GetQuota(cmd.Context())
			if err != nil {
				return err
			}
			jsonOut, _ := cmd.Flags().GetBool("json")
			if jsonOut {
				enc := json.NewEncoder(ios.Out)
				enc.SetIndent("", "  ")
				if quota == nil {
					return enc.Encode(map[string]any{"quota": nil})
				}
				return enc.Encode(map[string]any{"quota": quota})
			}
			if quota == nil || quota.MaxBytes <= 0 {
				ios.WriteOutLine("无配额限制（不限量）")
				return nil
			}
			ios.WriteOutLine("已用 %s / 上限 %s（水位 %d%%）",
				formatBytes(quota.Usage), formatBytes(quota.MaxBytes), quota.Watermark)
			return nil
		},
	}
}

// formatBytes 人类可读字节（1 KiB 等）。
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
