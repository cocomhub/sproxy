// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdVolumes 创建 volumes 子命令：列出当前 owner 可见的卷（name/mode/capacity/usage/allowed）。
// 数据来自服务端 GET /api/volumes（per-owner ACL）。
func NewCmdVolumes(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volumes",
		Short: "列出可用的存储卷",
		Long: `列出当前凭据可见的存储卷及其用量。

每个卷显示名称、ACL 模式、容量上限（0=不限）、当前已用与是否允许写入。
缺省单卷配置列出默认卷（default）；多卷配置列出当前凭据可见的全部卷。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			vols, err := svc.Volumes(cmd.Context())
			if err != nil {
				ios.WriteErrLine("获取卷列表失败: %v", err)
				return fmt.Errorf("获取卷列表失败: %w", err)
			}
			printVolumes(ios.Out, cmd, vols)
			return nil
		},
	}
	return cmd
}

// printVolumes 按 --json 决定输出：JSON 直接输出 volumes 数组；文本人类可读表格。
func printVolumes(w io.Writer, cmd *cobra.Command, vols []client.VolumeInfo) {
	useJSON, _ := cmd.Flags().GetBool("json")
	if useJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"volumes": vols})
		return
	}
	if len(vols) == 0 {
		fmt.Fprintln(w, "无可见卷")
		return
	}
	fmt.Fprintf(w, "%-16s  %-8s  %-14s  %-14s  %s\n", "名称", "模式", "容量", "已用", "允许")
	for _, v := range vols {
		capTxt := capacityText(v.Capacity)
		usageTxt := usageText(v.Capacity, v.Usage)
		allowedTxt := "是"
		if !v.Allowed {
			allowedTxt = "否"
		}
		fmt.Fprintf(w, "%-16s  %-8s  %-14s  %-14s  %s\n", v.Name, v.Mode, capTxt, usageTxt, allowedTxt)
	}
}

// capacityText 把卷容量上限格式化为人类可读（0 = 不限）。
func capacityText(capacity int64) string {
	if capacity <= 0 {
		return "不限"
	}
	return client.FormatByte(float64(capacity))
}

// usageText 把卷用量格式化为人类可读；容量>0 时附占比。
func usageText(capacity, usage int64) string {
	if usage <= 0 {
		return "0 B"
	}
	base := client.FormatByte(float64(usage))
	if capacity > 0 {
		pct := float64(usage) / float64(capacity) * 100
		return fmt.Sprintf("%s (%.1f%%)", base, pct)
	}
	return base
}
