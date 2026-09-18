// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// TestMeshRouteTable_100NodeScale：单 hub 100+ 节点规模化——注册/查找/List 压力。
// 用同一 mock mux（xfertest.Pipe 对）注册 150 个节点，验证：
//   - 全部注册成功（NodeCount = 150）
//   - 随机查找命中（Lookup 返回非 nil）
//   - List 返回全部且按 ID 无重复
//   - 并发查找 -race 稳定
func TestMeshRouteTable_100NodeScale(t *testing.T) {
	mrt := NewMeshRouteTable()
	const total = 150
	muxPerNode := make([]*mux.Mux, total)
	for i := range total {
		muxPerNode[i] = newTestMux(t)
		mrt.AddNode("", NodeID(fmt.Sprintf("node-%03d", i)), muxPerNode[i])
	}

	// 全部注册成功。
	if got := mrt.NodeCount(""); got != total {
		t.Fatalf("NodeCount = %d, want %d", got, total)
	}

	// 随机查找命中。
	for i := range total {
		if mrt.Lookup(NodeID(fmt.Sprintf("node-%03d", i))) == nil {
			t.Fatalf("Lookup(node-%03d) = nil, want 命中", i)
		}
	}

	// List 返回全部且无重复。
	list := mrt.List("")
	if len(list) != total {
		t.Fatalf("List len = %d, want %d", len(list), total)
	}
	seen := make(map[NodeID]bool, total)
	for _, n := range list {
		if seen[n.ID] {
			t.Fatalf("List 重复节点: %s", n.ID)
		}
		seen[n.ID] = true
	}

	// 并发查找（-race 验证并发安全）。
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for i := range total {
				_ = mrt.Lookup(NodeID(fmt.Sprintf("node-%03d", i)))
			}
		})
	}
	wg.Wait()
}

// TestMeshRouteTable_100NodeOpen：100+ 节点下中继拨号（Open 流）压力。
// 每个节点注册独立 mux，随机选 30 个节点 Open 流（模拟 relay 拨号），
// 验证 Open 成功率 100%（无超时/无错流）。
func TestMeshRouteTable_100NodeOpen(t *testing.T) {
	mrt := NewMeshRouteTable()
	const total = 120
	muxPerNode := make([]*mux.Mux, total)
	for i := range total {
		muxPerNode[i] = newTestMux(t)
		mrt.AddNode("", NodeID(fmt.Sprintf("node-%03d", i)), muxPerNode[i])
	}

	// 随机选 30 个节点 Open（模拟 relay 拨号压力）。
	const dials = 30
	for i := range dials {
		id := NodeID(fmt.Sprintf("node-%03d", i*4%total))
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		stream, err := mrt.Lookup(id).Open(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Open(node-%s) 失败: %v", id, err)
		}
		if stream == nil {
			t.Fatalf("Open(node-%s) 返回 nil stream", id)
		}
		_ = stream.Close()
	}
}

// TestMeshRouteTable_100NodeRemove：100+ 节点下删除压力（Remove 全部节点）。
// 验证删除后 NodeCount 归零、Lookup 全 miss、重复删除幂等。
func TestMeshRouteTable_100NodeRemove(t *testing.T) {
	mrt := NewMeshRouteTable()
	const total = 100
	muxPerNode := make([]*mux.Mux, total)
	for i := range total {
		muxPerNode[i] = newTestMux(t)
		mrt.AddNode("", NodeID(fmt.Sprintf("node-%03d", i)), muxPerNode[i])
	}
	for i := range total {
		if !mrt.Remove(NodeID(fmt.Sprintf("node-%03d", i))) {
			t.Fatalf("Remove(node-%03d) 返回 false, want true", i)
		}
	}
	if got := mrt.NodeCount(""); got != 0 {
		t.Fatalf("NodeCount after remove = %d, want 0", got)
	}
	if mrt.Lookup(NodeID("node-000")) != nil {
		t.Fatal("Lookup 已删节点应返回 nil")
	}
	// 重复删除幂等。
	if mrt.Remove(NodeID("node-000")) {
		t.Fatal("重复 Remove 应返回 false")
	}
}
