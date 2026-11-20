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

// metaAuditClient 是轮询所需的最小客户端能力（便于测试替换）。
type metaAuditClient interface {
	VolumeMetaAudit(ctx context.Context, volume string, refresh bool) ([]byte, int, error)
}

// NewCmdVolumeMetaAudit 创建 `volume meta-audit`：只读审计元数据层（file/dir meta 可解密性、
// 墓碑数、存活 meta 引用的分块是否齐全）。异步：默认轮询到完成。
func NewCmdVolumeMetaAudit(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var volume string
	var refresh, wait bool
	var interval time.Duration
	cmd := &cobra.Command{SilenceUsage: true, SilenceErrors: true,
		Use:   "meta-audit",
		Short: "只读审计元数据（墓碑/损坏/缺失分块），不清数据",
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
		return pollMetaAudit(cmd, ios, c, volume, refresh, wait, interval)
	}
	return cmd
}

// pollMetaAudit 轮询后台元数据审计直到完成（`wait=false` 只触发一次）。
func pollMetaAudit(
	cmd *cobra.Command, ios cli.IOStreams, c metaAuditClient,
	volume string, refresh, wait bool, interval time.Duration,
) error {
	for {
		body, code, err := c.VolumeMetaAudit(cmd.Context(), volume, refresh)
		if err != nil {
			return err
		}
		if code == http.StatusNotImplemented {
			return fmt.Errorf("该卷不支持元数据审计（不支持，非无异常）")
		}
		ios.WriteOutLine("%s", string(body))
		if code == http.StatusOK || !wait {
			return nil
		}
		refresh = false
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(interval):
		}
	}
}
