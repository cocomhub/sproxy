// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

// cleanupTURNREST 复位 webrtc 子模块的 TURN REST 全局配置（防跨测试污染）。
func cleanupTURNREST(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = webrtc.SetTURNRESTURL("", "", "") })
}

// TestApplyTURNRESTFlags_NoFlagNoop 验证未指定 --turn-rest 时是 no-op（返回 nil，
// 不覆盖现有 REST 配置）。
func TestApplyTURNRESTFlags_NoFlagNoop(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	addTURNRESTFlags(cmd)
	if err := applyTURNRESTFlags(cmd); err != nil {
		t.Fatalf("未指定 --turn-rest 应为 no-op，实际返回错误: %v", err)
	}
}

// TestApplyTURNRESTFlags_ValidURL 验证合法 https 端点通过并装配（fail-open）。
func TestApplyTURNRESTFlags_ValidURL(t *testing.T) {
	cleanupTURNREST(t)
	cmd := &cobra.Command{Use: "test"}
	addTURNRESTFlags(cmd)
	if err := cmd.Flags().Set("turn-rest", "https://turn.example.com/turn"); err != nil {
		t.Fatalf("Set turn-rest: %v", err)
	}
	if err := cmd.Flags().Set("turn-rest-user", "api-user"); err != nil {
		t.Fatalf("Set turn-rest-user: %v", err)
	}
	if err := applyTURNRESTFlags(cmd); err != nil {
		t.Fatalf("合法 --turn-rest 应通过: %v", err)
	}
}

// TestApplyTURNRESTFlags_InvalidURLFails 验证非 loopback 明文 http 端点 fail-closed：
// 命令终止（返回错误），不静默忽略。
func TestApplyTURNRESTFlags_InvalidURLFails(t *testing.T) {
	cleanupTURNREST(t)
	cmd := &cobra.Command{Use: "test"}
	addTURNRESTFlags(cmd)
	if err := cmd.Flags().Set("turn-rest", "http://turn.example.com/turn"); err != nil {
		t.Fatalf("Set turn-rest: %v", err)
	}
	if err := cmd.Flags().Set("turn-rest-user", "u"); err != nil {
		t.Fatalf("Set turn-rest-user: %v", err)
	}
	err := applyTURNRESTFlags(cmd)
	if err == nil {
		t.Fatal("非 loopback 明文 http 应 fail-closed 返回错误")
	}
	if !strings.Contains(err.Error(), "--turn-rest") {
		t.Fatalf("错误应包含 --turn-rest 上下文，got: %v", err)
	}
}
