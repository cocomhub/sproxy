// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package alerts

// alerts_poll_test.go 覆盖水位轮询的生命周期与边界：
//  1. NewEngine 的轮询间隔归一（<=0 → 60s 默认；>0 原样）。
//  2. Start/Close：短 tick 轮询磁盘 + 配额水位（同一 tick 内两个检查）；
//     poll<=0 的防御守卫不启动 goroutine；Close 幂等。
//  3. checkDiskWatermark/checkQuotaWatermarks：无 reader、容量非法（<=0）、
//     无匹配规则、单 owner 容量非法被跳过等边界。
//  4. ownerListSnapshot：未注入 → nil；已注入 → 回调结果。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/config"
)

// TestNewEngine_PollInterval 轮询间隔归一：<=0 → 60s；显式值原样保留。
func TestNewEngine_PollInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "零值取默认", in: 0, want: 60 * time.Second},
		{name: "负值取默认", in: -time.Second, want: 60 * time.Second},
		{name: "显式值保留", in: 250 * time.Millisecond, want: 250 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := NewEngine(config.AlertConfig{Enabled: true, PollInterval: tc.in}, testLogger())
			t.Cleanup(eng.Close)
			if eng.poll != tc.want {
				t.Fatalf("poll 应为 %v，got %v", tc.want, eng.poll)
			}
		})
	}
}

// TestEngine_StartPollsWatermarks 短 tick 轮询：同一 tick 内检查磁盘与配额水位
// （磁盘 85% ≥ 80%、owner alice 90% ≥ 80%），Close 幂等。
func TestEngine_StartPollsWatermarks(t *testing.T) {
	t.Parallel()
	diskCh := newMessageRecorder("disk")
	quotaCh := newMessageRecorder("quota")
	var diskCalls atomic.Int64
	eng := NewEngine(config.AlertConfig{
		Enabled:      true,
		PollInterval: time.Millisecond,
		Rules: []config.AlertRule{
			{Source: "disk_watermark", Threshold: 80, Channels: []string{"disk"}},
			{Source: "quota_watermark", Threshold: 80, Channels: []string{"quota"}},
		},
	}, testLogger())
	eng.Register(diskCh)
	eng.Register(quotaCh)
	eng.SetDiskUsageReader(func() (used, capVal int64) {
		diskCalls.Add(1)
		return 850, 1000
	})
	eng.SetQuotaUsageReader(func(owner string) (used, capVal int64) { return 900, 1000 })
	eng.SetOwnerList(func() []string { return []string{"alice"} })

	eng.Start()
	diskMsg := diskCh.recv(t)
	if !strings.Contains(diskMsg.Text, "85%") {
		t.Fatalf("磁盘水位告警应含 85%%：%s", diskMsg.Text)
	}
	quotaMsg := quotaCh.recv(t)
	if quotaMsg.Object != "alice" {
		t.Fatalf("配额告警 Object 应为 owner，got %q", quotaMsg.Object)
	}
	if !strings.Contains(quotaMsg.Text, "90%") {
		t.Fatalf("配额告警应含 90%%：%s", quotaMsg.Text)
	}

	eng.Close()
	eng.Close() // 幂等：已关闭时直接返回
	if diskCalls.Load() == 0 {
		t.Fatal("轮询 tick 应读取磁盘水位")
	}
}

// TestEngine_StartGuardZeroPoll poll<=0 的防御守卫不启动轮询 goroutine。
// 构造器把 <=0 归一为 60s，故只能用白盒改字段命中该守卫。
func TestEngine_StartGuardZeroPoll(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	eng := NewEngine(config.AlertConfig{
		Enabled: true,
		Rules:   []config.AlertRule{{Source: "disk_watermark", Threshold: 80}},
	}, testLogger())
	eng.SetDiskUsageReader(func() (used, capVal int64) {
		calls.Add(1)
		return 900, 1000
	})
	eng.poll = 0
	eng.Start()
	eng.Close()
	if calls.Load() != 0 {
		t.Fatalf("poll<=0 时不应启动轮询，读取次数 %d", calls.Load())
	}
}

// TestEngine_CheckDiskWatermark_NoReader 未注入磁盘水位读取器 → 直接返回（无告警）。
func TestEngine_CheckDiskWatermark_NoReader(t *testing.T) {
	t.Parallel()
	ch := newMessageRecorder("wecom")
	eng := NewEngine(config.AlertConfig{
		Enabled: true,
		Rules:   []config.AlertRule{{Source: "disk_watermark", Threshold: 80, Channels: []string{"wecom"}}},
	}, testLogger())
	eng.Register(ch)
	t.Cleanup(eng.Close)

	eng.checkDiskWatermark(context.Background())
	ch.expectSilent(t)
}

// TestEngine_CheckDiskWatermark_InvalidCapacity 容量 <=0（未知/未装配）→ 跳过分位计算与告警。
func TestEngine_CheckDiskWatermark_InvalidCapacity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cap  int64
	}{
		{name: "零容量", cap: 0},
		{name: "负容量", cap: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := newMessageRecorder("wecom")
			eng := NewEngine(config.AlertConfig{
				Enabled: true,
				Rules:   []config.AlertRule{{Source: "disk_watermark", Threshold: 80, Channels: []string{"wecom"}}},
			}, testLogger())
			eng.Register(ch)
			t.Cleanup(eng.Close)
			eng.SetDiskUsageReader(func() (used, capVal int64) { return 100, tc.cap })

			eng.checkDiskWatermark(context.Background())
			ch.expectSilent(t)
		})
	}
}

// TestEngine_OwnerListSnapshot 未注入 → nil；已注入 → 回调结果。
func TestEngine_OwnerListSnapshot(t *testing.T) {
	t.Parallel()
	eng := NewEngine(config.AlertConfig{Enabled: true}, testLogger())
	t.Cleanup(eng.Close)

	if got := eng.ownerListSnapshot(); got != nil {
		t.Fatalf("未注入 owner 列表时应返回 nil，got %#v", got)
	}

	eng.SetOwnerList(func() []string { return []string{"alice", "bob"} })
	got := eng.ownerListSnapshot()
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("owner 列表应来自注入回调，got %#v", got)
	}
}

// TestEngine_CheckQuotaWatermarks_NoReader 未注入配额水位读取器 → 直接返回。
func TestEngine_CheckQuotaWatermarks_NoReader(t *testing.T) {
	t.Parallel()
	ch := newMessageRecorder("wecom")
	eng := NewEngine(config.AlertConfig{
		Enabled: true,
		Rules:   []config.AlertRule{{Source: "quota_watermark", Threshold: 80, Channels: []string{"wecom"}}},
	}, testLogger())
	eng.Register(ch)
	t.Cleanup(eng.Close)

	eng.checkQuotaWatermarks(context.Background(), []string{"alice"})
	ch.expectSilent(t)
}

// TestEngine_CheckQuotaWatermarks_NoRules 有 reader 但无 quota_watermark 规则 → 不做任何分发。
func TestEngine_CheckQuotaWatermarks_NoRules(t *testing.T) {
	t.Parallel()
	ch := newMessageRecorder("wecom")
	eng := NewEngine(config.AlertConfig{
		Enabled: true,
		Rules:   []config.AlertRule{{Source: "disk_watermark", Threshold: 80, Channels: []string{"wecom"}}},
	}, testLogger())
	eng.Register(ch)
	t.Cleanup(eng.Close)
	eng.SetQuotaUsageReader(func(owner string) (used, capVal int64) { return 900, 1000 })

	eng.checkQuotaWatermarks(context.Background(), []string{"alice"})
	ch.expectSilent(t)
}

// TestEngine_CheckQuotaWatermarks_SkipsInvalidCapacity 单 owner 容量 <=0 被跳过，
// 不影响后续 owner 的告警判定（各 owner 独立）。
func TestEngine_CheckQuotaWatermarks_SkipsInvalidCapacity(t *testing.T) {
	t.Parallel()
	ch := newMessageRecorder("wecom")
	eng := NewEngine(config.AlertConfig{
		Enabled: true,
		Rules:   []config.AlertRule{{Source: "quota_watermark", Threshold: 80, Channels: []string{"wecom"}}},
	}, testLogger())
	eng.Register(ch)
	t.Cleanup(eng.Close)
	eng.SetQuotaUsageReader(func(owner string) (used, capVal int64) {
		switch owner {
		case "zero":
			return 100, 0
		case "negative":
			return 100, -5
		default:
			return 900, 1000
		}
	})

	eng.checkQuotaWatermarks(context.Background(), []string{"zero", "negative", "alice"})
	msg := ch.recv(t)
	if msg.Object != "alice" {
		t.Fatalf("只有容量有效的 owner 应告警，got Object=%q", msg.Object)
	}
	ch.expectSilent(t)
}
