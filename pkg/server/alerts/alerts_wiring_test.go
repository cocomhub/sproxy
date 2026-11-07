// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package alerts

// alerts_wiring_test.go 覆盖装配入口与建议器装配：
//  1. NewFromConfig：未启用 / 无规则 → nil；启用且有规则 → 引擎（规则快照接入）。
//  2. AdoptNotifyChannels：nil 中心 no-op；已注册渠道并入（规则按名命中，多渠道各自收到）。
//  3. NewAIAdvisorFromConfig：未启用 → nil；启用但 key 解析为空 → nil + Warn；
//     启用且 key 来自环境变量 → 装好 LLM 网关。

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/server/notify"
)

// TestNewFromConfig 装配入口：未启用或无规则时 nil（零回归），否则返回引擎。
func TestNewFromConfig(t *testing.T) {
	t.Parallel()
	rules := []config.AlertRule{{Source: "volume_degraded", Channels: []string{"wecom"}}}
	tests := []struct {
		name    string
		cfg     config.AlertConfig
		wantNil bool
	}{
		{name: "未启用", cfg: config.AlertConfig{Enabled: false, Rules: rules}, wantNil: true},
		{name: "无规则", cfg: config.AlertConfig{Enabled: true}, wantNil: true},
		{name: "启用", cfg: config.AlertConfig{Enabled: true, Rules: rules}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := NewFromConfig(tc.cfg, testLogger())
			if tc.wantNil {
				if eng != nil {
					t.Fatalf("应返回 nil，got %+v", eng)
				}
				return
			}
			if eng == nil {
				t.Fatal("启用且有规则时应返回引擎")
			}
			t.Cleanup(eng.Close)
			if len(eng.rules) != len(tc.cfg.Rules) {
				t.Fatalf("规则应接入引擎：want %d，got %d", len(tc.cfg.Rules), len(eng.rules))
			}
		})
	}
}

// TestEngine_AdoptNotifyChannels nil 中心 no-op；非 nil 时渠道并入引擎，
// 规则的多渠道各自收到通知；重复并入不覆盖既有渠道实例。
func TestEngine_AdoptNotifyChannels(t *testing.T) {
	t.Parallel()
	eng := NewEngine(config.AlertConfig{
		Enabled: true,
		Rules: []config.AlertRule{{
			Source:   "volume_degraded",
			Channels: []string{"wecom", "webhook"},
		}},
	}, testLogger())
	t.Cleanup(eng.Close)

	eng.AdoptNotifyChannels(nil) // no-op，不 panic

	nc := notify.NewNotifyCenter(config.NotifyConfig{}, testLogger())
	t.Cleanup(nc.Close)
	wecom := newMessageRecorder("wecom")
	webhook := newMessageRecorder("webhook")
	nc.Register(wecom)
	nc.Register(webhook)
	eng.AdoptNotifyChannels(nc)

	eng.OnVolumeDegraded(context.Background(), "vol1", "probe failed")
	wecom.recv(t)
	webhook.recv(t)

	// 重复并入：渠道已在引擎注册 → Register 返回 false，不覆盖（仍是同一实例）。
	eng.AdoptNotifyChannels(nc)
	eng.OnVolumeDegraded(context.Background(), "vol2", "probe failed")
	wecom.recv(t)
	webhook.recv(t)
}

// TestNewAIAdvisorFromConfig_Disabled 未启用 → nil（零回归）。
func TestNewAIAdvisorFromConfig_Disabled(t *testing.T) {
	t.Parallel()
	if got := NewAIAdvisorFromConfig(config.AIAdvisorConfig{Enabled: false}, testLogger()); got != nil {
		t.Fatalf("enabled=false 应返回 nil，got %+v", got)
	}
}

// TestNewAIAdvisorFromConfig_EmptyKeyDegrades 启用但 api_key_ref 解析为空 →
// Warn（可观测降级）+ nil（gate=nil 语义）。用从未设置的变量名，避免写环境。
func TestNewAIAdvisorFromConfig_EmptyKeyDegrades(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	got := NewAIAdvisorFromConfig(config.AIAdvisorConfig{
		Enabled:   true,
		APIKeyRef: "SPROXY_TEST_ALERTS_ADVISOR_UNSET_KEY",
	}, logger)
	if got != nil {
		t.Fatalf("key 解析为空应降级为 nil，got %+v", got)
	}
	if logLine := buf.String(); !strings.Contains(logLine, "SPROXY_TEST_ALERTS_ADVISOR_UNSET_KEY") {
		t.Fatalf("降级应记 Warn（含 api_key_ref）：%s", logLine)
	}
}

// TestNewAIAdvisorFromConfig_KeyFromEnv 启用且 key 来自环境变量 → 网关装配成功
// （Enabled() 为 true）。t.Setenv 使本用例串行（R18 豁免条件 ①）。
func TestNewAIAdvisorFromConfig_KeyFromEnv(t *testing.T) {
	t.Setenv("SPROXY_TEST_ALERTS_ADVISOR_KEY", "sk-example-test-key")

	got := NewAIAdvisorFromConfig(config.AIAdvisorConfig{
		Enabled:   true,
		Provider:  "openai",
		APIKeyRef: "SPROXY_TEST_ALERTS_ADVISOR_KEY",
		Model:     "test-model",
		Timeout:   time.Second,
	}, testLogger())
	if got == nil {
		t.Fatal("key 解析成功应返回建议器")
	}
	if !got.Enabled() {
		t.Fatal("key 解析成功时网关应装配（Enabled()=true）")
	}
}
