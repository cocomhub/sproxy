// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
)

// TestEventBridge_ReloadOnFileChange 验证事件总线 → 副本索引失效桥接。
func TestEventBridge_ReloadOnFileChange(t *testing.T) {
	t.Parallel()
	bus := NewEventBus()
	svc := &fakeIndexService{applyRev: true}
	b := newEventIndexBridge(bus, svc, slog.Default())
	b.Start()
	defer b.Stop()
	// 发布 upload 事件（ownerA/rel）→ 桥接应触发 ReloadIndex（rev 幂等由领域校验）。
	bus.Publish(files.EventUpload, "ownerA", "a.txt", 10)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if svc.invalidated.Load() > 0 {
			// 计数已到：等待 lastInvalidated 可见（atomic 独立字段，消除 CI 并行下
			// Load 窗口竞态——先 Add 后 Store 同 goroutine，但测试两字段分读）。
			time.Sleep(20 * time.Millisecond)
			if svc.invalidatedOwner() != "ownerA" {
				t.Fatalf("owner = %s, want ownerA", svc.invalidatedOwner())
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("事件发布后 InvalidateIndex 未被调用")
}

// TestEventBridge_OnlyFileActions 验证非文件事件（读面）不触发。
func TestEventBridge_OnlyFileActions(t *testing.T) {
	t.Parallel()
	bus := NewEventBus()
	svc := &fakeIndexService{applyRev: true}
	b := newEventIndexBridge(bus, svc, slog.Default())
	b.Start()
	defer b.Stop()
	bus.Publish("unknown.action", "ownerA", "x.txt", 1)
	time.Sleep(200 * time.Millisecond)
	if svc.reloaded.Load() != 0 {
		t.Fatalf("非文件事件不应触发 reload: %d", svc.reloaded.Load())
	}
}
