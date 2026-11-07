// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// quota.go 实现 `quota` 子命令：展示本 owner 配额水位。
//
// **权威源 = 联邦视图**（片 5 读侧，设计 docs/designs/2026-10-16-quota-volume-layer.md §1.5）：
// 用户配额**按卷持有** ⇒ 用户视图 = 对「该用户可访问的卷」**求和**（`GET /api/quota`，
// 含逐卷明细 ✓）。旧集中树（`/api/stats` 的 `quota` 段：usage/max_bytes/watermark）仍作
// **补充**信息（预警水位），**best-effort** —— 端点不可用不阻塞联邦视图 ✓。

import (
	"encoding/json"
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// newCmdQuota 创建 quota 命令。
func newCmdQuota(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "quota",
		Short: "查看本 owner 配额（跨卷联邦视图 + 逐卷明细）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			ctx := cmd.Context()

			// 联邦视图是**权威源**（片 5）；旧服务端无该端点（404）时不得静默失败 ✗
			// ⇒ **显式告警 + 回落**旧集中树视图（可观测 ✓，禁静默降级 ✗）。
			fed, fedErr := svc.GetFederatedQuota(ctx)
			legacy, _ := svc.GetQuota(ctx) // 旧集中树（预警水位）为**补充**信息 ⇒ best-effort
			if fedErr != nil {
				if legacy == nil {
					return fedErr // 两侧都不可用 ⇒ 上报联邦错误（含状态码，便于诊断）
				}
				ios.WriteErrLine("告警：服务端不支持联邦配额视图（%v）—— 回落到集中树配额", fedErr)
			}

			jsonOut, _ := cmd.Flags().GetBool("json")
			if jsonOut {
				return encodeQuotaJSON(ios, legacy, fed)
			}
			writeQuota(ios, fed, legacy)
			return nil
		},
	}
}

// encodeQuotaJSON 输出 JSON：**向后兼容**保留旧 `quota` 键（消费方脚本不破 ✓），新增
// `federated` 键（跨卷权威视图 ✓）。
func encodeQuotaJSON(ios cli.IOStreams, legacy *client.QuotaStats, fed *client.FederatedQuota) error {
	enc := json.NewEncoder(ios.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"quota": legacy, "federated": fed})
}

// writeQuota 输出人类可读视图：联邦汇总 + 逐卷明细；无联邦视图时回落集中树水位。
func writeQuota(ios cli.IOStreams, fed *client.FederatedQuota, legacy *client.QuotaStats) {
	if fed == nil {
		writeLegacyQuota(ios, legacy)
		return
	}
	writeFederatedQuota(ios, fed)
	if legacy != nil && legacy.MaxBytes > 0 {
		ios.WriteOutLine("（预警水位：已用 %s / 上限 %s（%d%%）—— 集中树补充信息）",
			formatBytes(legacy.Usage), formatBytes(legacy.MaxBytes), legacy.Watermark)
	}
}

// writeLegacyQuota 回落输出（旧集中树视图）。
func writeLegacyQuota(ios cli.IOStreams, legacy *client.QuotaStats) {
	if legacy == nil || legacy.MaxBytes <= 0 {
		ios.WriteOutLine("无配额限制（不限量）")
		return
	}
	ios.WriteOutLine("已用 %s / 上限 %s（水位 %d%%）",
		formatBytes(legacy.Usage), formatBytes(legacy.MaxBytes), legacy.Watermark)
}

// writeFederatedQuota 输出联邦视图（汇总 + 逐卷明细 + 未知/不限提示）。
func writeFederatedQuota(ios cli.IOStreams, fed *client.FederatedQuota) {
	if fed.Unlimited {
		ios.WriteOutLine("已用 %s（含不限卷）", formatBytes(fed.Used))
	} else {
		ios.WriteOutLine("已用 %s / 上限 %s", formatBytes(fed.Used), formatBytes(fed.Capacity))
	}
	for _, v := range fed.Volumes {
		writeVolumeQuotaLine(ios, v)
	}
	if fed.Partial {
		// 未知部分未计入 ⇒ 必须显式提示（禁当完整值 ✗）。
		ios.WriteOutLine("提示：部分可访问卷无每用户配额权威，上述汇总**不完整**。")
	}
}

// writeVolumeQuotaLine 输出单卷明细行（switch 分支：未知 / 不限 / 有上限）。
func writeVolumeQuotaLine(ios cli.IOStreams, v client.VolumeQuota) {
	switch {
	case !v.HasQuota:
		// **未知 ≠ 0**（不得显示成「已用 0」✗）。
		ios.WriteOutLine("  %s  未知（该卷无每用户配额权威）", v.Volume)
	case v.Capacity <= 0:
		ios.WriteOutLine("  %s  %s / 不限", v.Volume, formatBytes(v.Used))
	default:
		ios.WriteOutLine("  %s  %s / %s", v.Volume, formatBytes(v.Used), formatBytes(v.Capacity))
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
