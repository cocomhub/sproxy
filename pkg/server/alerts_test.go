// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// alerts_test.go 验证阈值告警引擎（roadmap P1 阈值告警）：
//  1. 状态机去抖：FIRING 只在首次触发发通知；恢复（OK）发恢复通知。
//  2. 磁盘水位：超阈值触发（80%），恢复后（<80%）发恢复。
//  3. 事件驱动：degraded 卷 / 同步失败 / 登录锁定 → 即时告警。
//  4. rules 路由：source 匹配 → channels 分发（复用 NotifyCenter）。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAlertChannel 记录 AlertMessage 的假渠道。
type fakeAlertChannel struct {
	name string
	msgs chan string // 收到的告警描述（并发安全）
}

func newFakeAlertChannel(name string) *fakeAlertChannel {
	return &fakeAlertChannel{name: name, msgs: make(chan string, 64)}
}

func (f *fakeAlertChannel) Name() string { return f.name }

func (f *fakeAlertChannel) Send(ctx context.Context, m NotifyMessage) error {
	f.msgs <- m.Title + "|" + m.Text
	return nil
}

// TestAlertEngine_DiskWatermark 磁盘水位 80% 触发 + 恢复通知。
func TestAlertEngine_DiskWatermark(t *testing.T) {
	t.Parallel()
	ch := newFakeAlertChannel("wecom")
	// 注入磁盘水位读取器（可调）。
	var usage atomic.Int64
	var capacity atomic.Int64
	capacity.Store(1000)
	usage.Store(850) // 85% > 80% → FIRING

	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules: []AlertRule{{
			Source: "disk_watermark", Threshold: 80, Channels: []string{"wecom"},
		}},
	}, nil)
	eng.Register(ch)
	t.Cleanup(eng.Close)
	eng.SetDiskUsageReader(func() (used, cap int64) { return usage.Load(), capacity.Load() })

	// 首次触发（FIRING → 通知）。
	eng.checkDiskWatermark(context.Background())
	select {
	case <-ch.msgs:
	default:
		t.Fatal("85% 应触发磁盘水位告警")
	}
	// 同水位再触发 → 去抖（不发）。
	eng.checkDiskWatermark(context.Background())
	select {
	case <-ch.msgs:
		t.Fatal("同水位二次触发应去抖")
	default:
	}
	// 恢复（60%）→ 恢复通知。
	usage.Store(600)
	eng.checkDiskWatermark(context.Background())
	select {
	case <-ch.msgs:
	default:
		t.Fatal("恢复应发恢复通知")
	}
}

// TestAlertEngine_EventDrivenDegraded 卷 degraded 事件驱动告警。
func TestAlertEngine_EventDrivenDegraded(t *testing.T) {
	t.Parallel()
	ch := newFakeAlertChannel("wecom")
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules: []AlertRule{{
			Source: "volume_degraded", Channels: []string{"wecom"},
		}},
	}, nil)
	eng.Register(ch)
	t.Cleanup(eng.Close)

	eng.OnVolumeDegraded(context.Background(), "backup", "probe failed")
	select {
	case m := <-ch.msgs:
		if !strings.Contains(m, "backup") {
			t.Fatalf("degraded 告警应含卷名: %s", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("degraded 应即时告警")
	}
	// 重复同卷 degraded → 去抖。
	eng.OnVolumeDegraded(context.Background(), "backup", "probe failed")
	select {
	case <-ch.msgs:
		t.Fatal("同卷重复 degraded 应去抖")
	case <-time.After(100 * time.Millisecond):
	}
	// 恢复 → 恢复通知。
	eng.OnVolumeRecovered(context.Background(), "backup")
	select {
	case <-ch.msgs:
	default:
		t.Fatal("卷恢复应发恢复通知")
	}
}
