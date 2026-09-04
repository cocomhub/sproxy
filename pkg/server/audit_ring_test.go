// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sync"
	"testing"
	"time"
)

// auditEvt 构造一个带指定 action/actor/mesh/TS 的测试审计事件。
func auditEvt(action, actor, mesh string, ts time.Time) AuditEvent {
	return AuditEvent{
		Action: action, Actor: actor, Mesh: mesh,
		ObjectType: "file", Object: "f.txt", Result: AuditResultSuccess,
		Detail: "d", TS: ts,
	}
}

// TestAuditRing_KeepsAllWithinCapacity 验证 N 条事件在容量内全保留。
func TestAuditRing_KeepsAllWithinCapacity(t *testing.T) {
	ring := NewAuditRing(3)
	for i := range 3 {
		ring.Add(auditEvt("delete", "a", "m", time.Unix(int64(i), 0)))
	}
	if got := ring.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
	evts := ring.Recent(10, AuditFilter{})
	if len(evts) != 3 {
		t.Fatalf("Recent 返回 %d 条, want 3", len(evts))
	}
	// 倒序：最新在前。
	for i := range 3 {
		wantTS := time.Unix(int64(2-i), 0)
		if !evts[i].TS.Equal(wantTS) {
			t.Errorf("Recent[%d].TS = %v, want %v（最新在前）", i, evts[i].TS, wantTS)
		}
	}
}

// TestAuditRing_DropsOldestBeyondCapacity 验证 N+1 条时丢弃最旧（N 条全保留）。
func TestAuditRing_DropsOldestBeyondCapacity(t *testing.T) {
	ring := NewAuditRing(3)
	for i := range 4 {
		ring.Add(auditEvt("delete", "a", "m", time.Unix(int64(i), 0)))
	}
	if got := ring.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
	evts := ring.Recent(10, AuditFilter{})
	if len(evts) != 3 {
		t.Fatalf("Recent 返回 %d 条, want 3", len(evts))
	}
	if evts[2].TS != time.Unix(1, 0) {
		t.Errorf("最旧事件应为 ts=1（0 已被丢弃），got %v", evts[2].TS)
	}
}

// TestAuditRing_DisabledNoop 验证 capacity<=0 时 Add/Recent/Len 安全可用（no-op）。
func TestAuditRing_DisabledNoop(t *testing.T) {
	ring := NewAuditRing(0)
	ring.Add(auditEvt("delete", "a", "m", time.Now()))
	if got := ring.Len(); got != 0 {
		t.Fatalf("disabled ring Len() = %d, want 0", got)
	}
	if evts := ring.Recent(10, AuditFilter{}); len(evts) != 0 {
		t.Fatalf("disabled ring Recent 返回 %d 条, want 0", len(evts))
	}
	ring2 := NewAuditRing(-5)
	ring2.Add(auditEvt("delete", "a", "m", time.Now()))
	if got := ring2.Len(); got != 0 {
		t.Fatalf("negative ring Len() = %d, want 0", got)
	}
}

// TestAuditRing_CapacityAndLen 验证 Capacity() 与 Len() 语义。
func TestAuditRing_CapacityAndLen(t *testing.T) {
	ring := NewAuditRing(64)
	if ring.Capacity() != 64 {
		t.Fatalf("Capacity() = %d, want 64", ring.Capacity())
	}
	if ring.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", ring.Len())
	}
}

// TestAuditRing_FilterAction 验证 action 精确过滤（空字段不过滤）。
func TestAuditRing_FilterAction(t *testing.T) {
	ring := NewAuditRing(8)
	ring.Add(auditEvt("delete", "a", "m1", time.Unix(1, 0)))
	ring.Add(auditEvt("rename", "b", "m2", time.Unix(2, 0)))
	ring.Add(auditEvt("delete", "b", "m2", time.Unix(3, 0)))

	evts := ring.Recent(10, AuditFilter{Action: "delete"})
	if len(evts) != 2 {
		t.Fatalf("action=delete 过滤后 %d 条, want 2", len(evts))
	}
	for _, e := range evts {
		if e.Action != "delete" {
			t.Errorf("过滤结果含非 delete 事件: %+v", e)
		}
	}
	// 倒序：先返回最新的 delete（ts=3），再是 ts=1。
	if evts[0].TS != time.Unix(3, 0) || evts[1].TS != time.Unix(1, 0) {
		t.Errorf("过滤结果倒序错误: %v", evts)
	}
}

// TestAuditRing_FilterActorMesh 验证 actor/mesh 精确过滤 + 组合。
func TestAuditRing_FilterActorMesh(t *testing.T) {
	ring := NewAuditRing(8)
	ring.Add(auditEvt("delete", "alice", "mesh-a", time.Unix(1, 0)))
	ring.Add(auditEvt("delete", "bob", "mesh-a", time.Unix(2, 0)))
	ring.Add(auditEvt("delete", "alice", "mesh-b", time.Unix(3, 0)))

	if evts := ring.Recent(10, AuditFilter{Actor: "alice"}); len(evts) != 2 {
		t.Fatalf("actor=alice 过滤后 %d 条, want 2", len(evts))
	}
	if evts := ring.Recent(10, AuditFilter{Mesh: "mesh-a"}); len(evts) != 2 {
		t.Fatalf("mesh=mesh-a 过滤后 %d 条, want 2", len(evts))
	}
	// 组合：alice + mesh-a → 仅 ts=1 一条。
	evts := ring.Recent(10, AuditFilter{Actor: "alice", Mesh: "mesh-a"})
	if len(evts) != 1 || evts[0].TS != time.Unix(1, 0) {
		t.Fatalf("组合过滤错误: %+v", evts)
	}
}

// TestAuditRing_FilterSince 验证 Since 过滤保留 evt.TS.After(since)。
func TestAuditRing_FilterSince(t *testing.T) {
	ring := NewAuditRing(8)
	ring.Add(auditEvt("delete", "a", "m", time.Unix(100, 0)))
	ring.Add(auditEvt("delete", "a", "m", time.Unix(200, 0)))
	// since=150 → 保留 ts=200。
	evts := ring.Recent(10, AuditFilter{Since: time.Unix(150, 0)})
	if len(evts) != 1 {
		t.Fatalf("since 过滤后 %d 条, want 1", len(evts))
	}
	if evts[0].TS != time.Unix(200, 0) {
		t.Errorf("since 过滤保留下的事件 ts=%v, want 200", evts[0].TS)
	}
	// 全量通过：since 零值不过滤。
	if evts := ring.Recent(10, AuditFilter{}); len(evts) != 2 {
		t.Fatalf("无 since 应返回全部 2 条, got %d", len(evts))
	}
}

// TestAuditRing_LimitUpperBound 验证 limit 限制返回条数（上限截断）。
func TestAuditRing_LimitUpperBound(t *testing.T) {
	ring := NewAuditRing(10)
	for i := range 10 {
		ring.Add(auditEvt("delete", "a", "m", time.Unix(int64(i), 0)))
	}
	evts := ring.Recent(3, AuditFilter{})
	if len(evts) != 3 {
		t.Fatalf("limit=3 返回 %d 条, want 3", len(evts))
	}
	if evts[0].TS != time.Unix(9, 0) {
		t.Errorf("limit 截断后最新应在前（ts=9）, got %v", evts[0].TS)
	}
	// limit>容量 → 全量。
	if evts := ring.Recent(100, AuditFilter{}); len(evts) != 10 {
		t.Fatalf("limit>容量 返回 %d 条, want 10", len(evts))
	}
}

// TestAuditRing_RecentLimitLeZero 验证 limit<=0 时防御兜底返回至多 50 条。
func TestAuditRing_RecentLimitLeZero(t *testing.T) {
	ring := NewAuditRing(80)
	for i := range 80 {
		ring.Add(auditEvt("delete", "a", "m", time.Unix(int64(i), 0)))
	}
	evts := ring.Recent(0, AuditFilter{})
	if len(evts) != 50 {
		t.Fatalf("limit=0 防御兜底应返回 50 条, got %d", len(evts))
	}
	// 依旧倒序。
	if evts[0].TS != time.Unix(79, 0) {
		t.Errorf("limit=0 兜底返回最新应在前（ts=79）, got %v", evts[0].TS)
	}
}

// TestAuditRing_ConcurrentRace 并发 Add + Recent 在 -race 下无竞态、长度正确。
func TestAuditRing_ConcurrentRace(t *testing.T) {
	ring := NewAuditRing(100)
	const writers = 8
	const perWriter = 200

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWriter {
				ring.Add(auditEvt("delete", "a", "m", time.Now()))
				if i%7 == 0 {
					_ = ring.Recent(10, AuditFilter{})
					_ = ring.Len()
				}
			}
		}(w)
	}
	wg.Wait()

	if got := ring.Len(); got != 100 {
		t.Fatalf("并发后 Len() = %d, want 100（环形封顶）", got)
	}
}
