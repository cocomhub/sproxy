// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// metrics_port_test.go 验证独立指标端口配置（roadmap 6.x P1 残余）：
//  1. MetricsPort > 0 时装配监听（由 cmd/sproxy 起——此处测 config 默认值）。
//  2. MetricsPort 默认 0（关闭零回归）。

import "testing"

// TestMetricsPort_DefaultOff 默认 0（关闭零回归）。
func TestMetricsPort_DefaultOff(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.MetricsPort != 0 {
		t.Fatalf("MetricsPort 默认 = %d, want 0", cfg.MetricsPort)
	}
}

// TestMetricsPort_ConfigSet 显式设置生效。
func TestMetricsPort_ConfigSet(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.MetricsPort = 9090
	if cfg.MetricsPort != 9090 {
		t.Fatalf("MetricsPort = %d, want 9090", cfg.MetricsPort)
	}
}
