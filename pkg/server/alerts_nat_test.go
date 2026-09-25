// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// alerts_nat_test.go 验证 NAT 穿透失败告警（roadmap 11.1-①）：
//  1. nat_failure source：失败事件 → 告警（FIRING 去抖）；同 peer 后续拨号成功 → 恢复通知。
//  2. key 按 peer 隔离：peer A 失败不影响 peer B 状态；同 peer 重复失败只通知一次。
//  3. 无 nat_failure 规则时事件不通知（source 精确匹配，默认匹配所有规则是错误实现）。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestAlertNATFailure_FireAndRecover 失败事件发告警（state=firing），
// 同 peer 后续成功事件发恢复通知（state=ok，恢复依赖先 firing）。
func TestAlertNATFailure_FireAndRecover(t *testing.T) {
	t.Parallel()
	ch := newFakeAlertChannel("wecom")
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules: []AlertRule{{
			Source: SourceNATFailure, Channels: []string{"wecom"},
		}},
	}, testLogger())
	eng.Register(ch)
	t.Cleanup(eng.Close)

	// 失败事件 → 告警（含 peer 与错误详情）。
	eng.OnNATFailure(context.Background(), "peer-exit", "stun 不可达")
	select {
	case m := <-ch.msgs:
		if !strings.Contains(m, "peer-exit") {
			t.Fatalf("告警应含 peer: %s", m)
		}
		if !strings.Contains(m, "stun 不可达") {
			t.Fatalf("告警应含失败详情: %s", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("失败事件应即时告警")
	}
	// 同 peer 重复失败 → 去抖（state=firing 不重复通知）。
	eng.OnNATFailure(context.Background(), "peer-exit", "stun 不可达")
	select {
	case <-ch.msgs:
		t.Fatal("同 peer 重复失败应去抖")
	case <-time.After(100 * time.Millisecond):
	}
	// 同 peer 后续成功 → 恢复通知。
	eng.OnNATRecovered(context.Background(), "peer-exit")
	select {
	case <-ch.msgs:
	default:
		t.Fatal("拨号成功后应发恢复通知")
	}
}

// TestAlertNATFailure_KeyPerPeer peer 维度独立：peer A 失败不影响 peer B；
// 同 peer 重复失败只通知一次（状态机按 nat_failure\x00<peer> 去抖）。
func TestAlertNATFailure_KeyPerPeer(t *testing.T) {
	t.Parallel()
	ch := newFakeAlertChannel("wecom")
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules: []AlertRule{{
			Source: SourceNATFailure, Channels: []string{"wecom"},
		}},
	}, testLogger())
	eng.Register(ch)
	t.Cleanup(eng.Close)

	eng.OnNATFailure(context.Background(), "peer-a", "打洞失败")
	select {
	case <-ch.msgs:
	default:
		t.Fatal("peer-a 失败应告警")
	}
	// peer-b 失败：独立 key，应再发一条（peer-a 的 firing 不影响 peer-b）。
	eng.OnNATFailure(context.Background(), "peer-b", "打洞失败")
	select {
	case <-ch.msgs:
	default:
		t.Fatal("peer-b 失败应独立告警（per-peer 去抖）")
	}
	// 同 peer 重复失败 → 去抖。
	eng.OnNATFailure(context.Background(), "peer-a", "打洞失败")
	select {
	case <-ch.msgs:
		t.Fatal("peer-a 重复失败应去抖")
	case <-time.After(100 * time.Millisecond):
	}
	// peer-b 恢复不影响 peer-a（peer-a 仍 firing → 不再发恢复通知）。
	eng.OnNATRecovered(context.Background(), "peer-b")
	select {
	case <-ch.msgs:
	default:
		t.Fatal("peer-b 恢复应发恢复通知")
	}
	eng.OnNATRecovered(context.Background(), "peer-b")
	select {
	case <-ch.msgs:
		t.Fatal("peer-b 已 ok 的重复恢复应 no-op")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestAlertNATFailure_NoRuleSilent 未配置 nat_failure 规则时事件零通知
// （source 精确匹配；把 rulesFor 改成匹配所有规则是错误实现，本测试拦截）。
func TestAlertNATFailure_NoRuleSilent(t *testing.T) {
	t.Parallel()
	ch := newFakeAlertChannel("wecom")
	// 只有 disk_watermark 规则，没有 nat_failure。
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules: []AlertRule{{
			Source: "disk_watermark", Threshold: 80, Channels: []string{"wecom"},
		}},
	}, testLogger())
	eng.Register(ch)
	t.Cleanup(eng.Close)

	eng.OnNATFailure(context.Background(), "peer-exit", "stun 不可达")
	select {
	case <-ch.msgs:
		t.Fatal("无 nat_failure 规则时失败事件不应通知")
	case <-time.After(100 * time.Millisecond):
	}
	eng.OnNATRecovered(context.Background(), "peer-exit")
	select {
	case <-ch.msgs:
		t.Fatal("无 nat_failure 规则时恢复事件不应通知")
	case <-time.After(100 * time.Millisecond):
	}
}
