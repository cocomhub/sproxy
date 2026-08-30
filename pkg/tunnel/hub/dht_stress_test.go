// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"fmt"
	"testing"
)

// TestMemoryDHT_100NodeStress（DoD）：内存 DHT 无界 map——100 节点全部驻留，
// GetClosestNodes 返回全部、无重复、确定性排序（词法）。配合 -race 验证并发安全。
func TestMemoryDHT_100NodeStress(t *testing.T) {
	dht := newMemoryDHT()
	defer dht.Close()

	const total = 100
	for i := range total {
		id := fmt.Sprintf("node-%03d", i)
		if err := dht.Register(t.Context(), PeerInfo{
			ID:    id,
			Addrs: []string{fmt.Sprintf("192.168.1.%d:9000", i%254+1)},
		}); err != nil {
			t.Fatalf("Register(%s): %v", id, err)
		}
	}

	// 全部驻留（无界 map）。
	all, err := dht.GetClosestNodes(t.Context(), "target", total)
	if err != nil {
		t.Fatalf("GetClosestNodes: %v", err)
	}
	if len(all) != total {
		t.Fatalf("内存 DHT 应驻留全部 %d 个节点, got %d", total, len(all))
	}

	// 无重复。
	seen := make(map[string]bool, len(all))
	for _, n := range all {
		if seen[n.ID] {
			t.Fatalf("候选重复: %s", n.ID)
		}
		seen[n.ID] = true
	}

	// 确定性排序（词法，memoryDHT 实现）。
	for i := 1; i < len(all); i++ {
		if all[i-1].ID >= all[i].ID {
			t.Fatalf("候选未按 ID 排序: %s 在 %s 前", all[i-1].ID, all[i].ID)
		}
	}

	// 请求超过总数时返回全部。
	over, err := dht.GetClosestNodes(t.Context(), "target", total*2)
	if err != nil {
		t.Fatalf("GetClosestNodes(over): %v", err)
	}
	if len(over) != total {
		t.Fatalf("请求过多应返回全部 %d, got %d", total, len(over))
	}
}
