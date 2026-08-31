// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

// addTURNRESTFlags 为命令注册 --turn-rest 系列 flag（动态 TURN REST 短期凭证，
// coturn 标准）。与 --turn/--turn-user/--turn-pass 静态凭据并存，REST 优先。
func addTURNRESTFlags(cmd *cobra.Command) {
	cmd.Flags().String("turn-rest", "",
		"TURN REST API 短期凭证端点（如 https://turn.example.com/turn；http 仅限 loopback）；配 --turn 使用，REST 优先于 --turn-user/--turn-pass")
	cmd.Flags().String("turn-rest-user", "", "TURN REST API 认证用户名（与 --turn-rest 配合，透传给服务端）")
	cmd.Flags().String("turn-rest-service", "", "TURN REST API 可选 service 参数（与 --turn-rest 配合，透传给服务端）")
}

// applyTURNRESTFlags 应用 --turn-rest 系列 flag：--turn-rest 为空时保持现状（不覆盖）。
// 返回错误表示配置非法（fail-closed，命令终止，不静默忽略）。
func applyTURNRESTFlags(cmd *cobra.Command) error {
	restURL, _ := cmd.Flags().GetString("turn-rest")
	if restURL == "" {
		return nil
	}
	restUser, _ := cmd.Flags().GetString("turn-rest-user")
	restService, _ := cmd.Flags().GetString("turn-rest-service")
	if err := webrtc.SetTURNRESTURL(restURL, restUser, restService); err != nil {
		return fmt.Errorf("--turn-rest 配置无效: %w", err)
	}
	return nil
}
