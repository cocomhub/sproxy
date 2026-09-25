// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// alerts_reload_test.go 验证告警规则热加载（roadmap 11.1-②，SIGHUP）：
//  1. ReloadRules 原子替换规则集（slices.Clone 快照，读方无撕裂）。
//  2. 保留既有 firing 状态——规则变更不误发恢复通知、不重复告警。
//  3. 阈值类规则（disk_watermark）热加载后下个轮询 tick 生效。

import (
	"context"
	"sync/atomic"
	"testing"
)

// TestAlertEngine_ReloadRules_AtomicSwap 验证 ReloadRules 原子换规则：
// 旧规则 A 触发 fire → ReloadRules(B) → 新事件只走 B（A 不再命中）。
// 变异探针：ReloadRules 若只是引用赋值（不 Clone），外部改新规则切片会穿透引擎 → 红。
func TestAlertEngine_ReloadRules_AtomicSwap(t *testing.T) {
	t.Parallel()
	chA := newFakeAlertChannel("chA")
	chB := newFakeAlertChannel("chB")
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules: []AlertRule{{
			Source: "volume_degraded", Channels: []string{"chA"},
		}},
	}, nil)
	eng.Register(chA)
	eng.Register(chB)
	t.Cleanup(eng.Close)

	// 旧规则 A：degraded → chA。
	eng.OnVolumeDegraded(context.Background(), "vol1", "probe failed")
	select {
	case <-chA.msgs:
	default:
		t.Fatal("旧规则应命中 chA")
	}

	// 外部持有新规则切片——变异（引用赋值不 Clone）后外部修改会穿透引擎。
	rulesB := []AlertRule{{
		Source: "volume_degraded", Channels: []string{"chB"},
	}}
	eng.ReloadRules(rulesB)
	rulesB[0].Channels = []string{"chA"}

	// 新事件：应只走 chB（原子替换后的新规则快照，外部改动不影响引擎）。
	eng.OnVolumeDegraded(context.Background(), "vol2", "probe failed")
	select {
	case <-chB.msgs:
	default:
		t.Fatal("ReloadRules 后新事件应命中 chB（原子替换）")
	}
	select {
	case <-chA.msgs:
		t.Fatal("ReloadRules 后旧渠道 chA 不应再命中")
	default:
	}
}

// TestAlertEngine_ReloadRules_PreservesState 验证 ReloadRules 保留既有 firing 状态：
// 同 key 不重复告警（不误报）、恢复通知仍正常发出（不误发/不吞恢复）。
// 变异探针：ReloadRules 清空 state → 同 key 重复 fire / 恢复被吞 → 红。
func TestAlertEngine_ReloadRules_PreservesState(t *testing.T) {
	t.Parallel()
	chA := newFakeAlertChannel("chA")
	chA2 := newFakeAlertChannel("chA2")
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules: []AlertRule{{
			Source: "volume_degraded", Channels: []string{"chA"},
		}},
	}, nil)
	eng.Register(chA)
	eng.Register(chA2)
	t.Cleanup(eng.Close)

	// 规则 A 触发 fire（vol1 → chA）。
	eng.OnVolumeDegraded(context.Background(), "vol1", "boom")
	select {
	case <-chA.msgs:
	default:
		t.Fatal("首次 degraded 应告警")
	}

	// 热加载同 source 新规则（渠道改 chA2）——state 保留：同 key 不再重复 fire。
	eng.ReloadRules([]AlertRule{{
		Source: "volume_degraded", Channels: []string{"chA2"},
	}})
	eng.OnVolumeDegraded(context.Background(), "vol1", "boom again")
	select {
	case <-chA2.msgs:
		t.Fatal("state 保留：同 key 仍 firing 不应重复告警")
	default:
	}

	// 恢复仍能发出（state 从 firing → ok，跨 ReloadRules 保留）。
	eng.OnVolumeRecovered(context.Background(), "vol1")
	select {
	case <-chA2.msgs:
	default:
		t.Fatal("恢复通知应发出（firing 状态跨 ReloadRules 保留）")
	}
}

// TestAlertEngine_ReloadRules_ThresholdNextTick 验证阈值类规则热加载后下个
// 轮询 tick 生效：磁盘水位阈值 80→90，tick 后 85% 走恢复（新阈值生效）。
// 变异探针：ReloadRules 只换渠道不换阈值 → 85% ≥ 80% 仍 fire（去抖不发）→ chB 收不到 → 红。
func TestAlertEngine_ReloadRules_ThresholdNextTick(t *testing.T) {
	t.Parallel()
	chA := newFakeAlertChannel("chA")
	chB := newFakeAlertChannel("chB")
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules: []AlertRule{{
			Source: "disk_watermark", Threshold: 80, Channels: []string{"chA"},
		}},
	}, nil)
	eng.Register(chA)
	eng.Register(chB)
	t.Cleanup(eng.Close)
	var usage atomic.Int64
	var capacity atomic.Int64
	capacity.Store(1000)
	usage.Store(850) // 85%
	eng.SetDiskUsageReader(func() (used, cap int64) { return usage.Load(), capacity.Load() })

	// 旧阈值 80：85% 触发告警。
	eng.checkDiskWatermark(context.Background())
	select {
	case <-chA.msgs:
	default:
		t.Fatal("85% ≥ 阈值 80% 应触发告警")
	}

	// 热加载新规则：阈值 90 + 渠道 chB。
	eng.ReloadRules([]AlertRule{{
		Source: "disk_watermark", Threshold: 90, Channels: []string{"chB"},
	}})
	// 下个轮询 tick：85% < 90% → 恢复（新阈值生效）。
	eng.checkDiskWatermark(context.Background())
	select {
	case <-chB.msgs:
	default:
		t.Fatal("新阈值 90 应生效：85% 应触发恢复通知到 chB")
	}
	// 再 tick：仍 85% < 90%，state 已 ok → 无新通知。
	eng.checkDiskWatermark(context.Background())
	select {
	case <-chB.msgs:
		t.Fatal("恢复后同水位不应重复通知")
	default:
	}
	select {
	case <-chA.msgs:
		t.Fatal("热加载后旧渠道不应再收到")
	default:
	}
}
