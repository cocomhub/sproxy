// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdVolumeOrphans 创建 `volume orphans` 命令：只读审计卷内未被引用的分块。
//
// 异步（2026-11-20）：服务端把审计放后台跑（生产约 1800 次外部往返，分钟级到小时级），
// 命令默认**轮询到完成**（`--wait=false` 则只触发并立即返回）。
func NewCmdVolumeOrphans(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var volume string
	var refresh, wait bool
	var interval time.Duration
	cmd := &cobra.Command{SilenceUsage: true, SilenceErrors: true,
		Use:   "orphans",
		Short: "只读审计卷内未被引用的分块（后台任务，不删除）",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringVar(&volume, "volume", "", "卷名（必填）")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "强制重新审计（否则复用上次结果）")
	cmd.Flags().BoolVar(&wait, "wait", true, "轮询直到审计完成")
	cmd.Flags().DurationVar(&interval, "interval", 10*time.Second, "轮询间隔")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, cerr := factory.NewClient(cmd)
		if cerr != nil {
			return cerr
		}
		return pollOrphanAudit(cmd, ios, c, volume, refresh, wait, interval)
	}
	return cmd
}

// orphanAuditorClient 是轮询所需的最小客户端能力（便于测试替换）。
type orphanAuditorClient interface {
	VolumeOrphans(ctx context.Context, volume string, refresh bool) ([]byte, int, error)
}

// pollOrphanAudit 轮询后台审计直到完成（`wait=false` 只触发一次）。
func pollOrphanAudit(
	cmd *cobra.Command, ios cli.IOStreams, c orphanAuditorClient,
	volume string, refresh, wait bool, interval time.Duration,
) error {
	for {
		body, code, err := c.VolumeOrphans(cmd.Context(), volume, refresh)
		if err != nil {
			return err
		}
		if code == http.StatusNotImplemented {
			return fmt.Errorf("该卷不支持孤儿审计（不支持，非无孤儿）")
		}
		ios.WriteOutLine("%s", string(body))
		if code == http.StatusOK || !wait {
			return nil
		}
		refresh = false // 只在首轮请求刷新，后续轮询只查状态
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(interval):
		}
	}
}
