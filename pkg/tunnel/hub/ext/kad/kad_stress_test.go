// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kad

import (
	"fmt"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// TestKademliaDHT_100NodeStress（DoD）：100 节点内存压力测试——注册 100 个节点后
// GetClosestNodes 返回正确数量的 XOR 最近候选、无重复、按 XOR 距离排序，全程不
// panic/不越界。配合 -race 验证并发安全。
//
// 注意：Kademlia 路由表是有界 k-bucket（每桶 bucketSize=20），随机 ID 分布下部分桶
// 会满而拒收新节点，因此表内实际节点数可能少于注册数——这是正确的 Kademlia 行为
// （有界路由表）。"100 节点全量驻留"由无界内存 DHT 压力测试覆盖（hub 包）。
func TestKademliaDHT_100NodeStress(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	defer dht.Close()

	const total = 100
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("node-%03d", i)
		if err := dht.Register(t.Context(), hub.PeerInfo{
			ID:    id,
			Addrs: []string{fmt.Sprintf("192.168.1.%d:9000", i%254+1)},
		}); err != nil {
			t.Fatalf("Register(%s): %v", id, err)
		}
	}

	// GetClosestNodes：请求 20 个，返回 ≤20 个（表内有界）。
	target := "discovery-target"
	closest, err := dht.GetClosestNodes(t.Context(), target, 20)
	if err != nil {
		t.Fatalf("GetClosestNodes: %v", err)
	}
	if len(closest) == 0 || len(closest) > 20 {
		t.Fatalf("GetClosestNodes 应返回 1..20 个候选, got %d", len(closest))
	}

	// 无重复。
	seen := make(map[string]bool, len(closest))
	for _, n := range closest {
		if seen[n.ID] {
			t.Fatalf("候选节点重复: %s", n.ID)
		}
		seen[n.ID] = true
	}

	// 按 XOR 距离升序。
	tgt := NodeIDFromString(target)
	for i := 1; i < len(closest); i++ {
		di := tgt.Xor(NodeIDFromString(closest[i-1].ID))
		dj := tgt.Xor(NodeIDFromString(closest[i].ID))
		if !di.Less(dj) {
			t.Fatalf("候选未按 XOR 距离升序: %s 排在 %s 前", closest[i-1].ID, closest[i].ID)
		}
	}

	// 请求数量超过表内节点数时返回全部（不 panic、不越界）。
	all, err := dht.GetClosestNodes(t.Context(), target, total*2)
	if err != nil {
		t.Fatalf("GetClosestNodes(all): %v", err)
	}
	if len(all) == 0 || len(all) > total {
		t.Fatalf("请求过多应返回表内全部（≤%d）, got %d", total, len(all))
	}

	// Lookup 已入表节点可用（遍历查一个确认在表内的）。
	for _, id := range []string{"node-000", "node-050", "node-099"} {
		if _, lerr := dht.Lookup(t.Context(), id); lerr == nil {
			return // 至少一个在表内且可查
		}
	}
	t.Fatal("100 个注册节点中应有节点入表且可 Lookup")
}

// TestKademliaDHT_CandidateDedup（DoD 候选去重）：重复注册同一节点只保留一份。
func TestKademliaDHT_CandidateDedup(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	defer dht.Close()

	for i := 0; i < 3; i++ {
		if err := dht.Register(t.Context(), hub.PeerInfo{ID: "node-a", Addrs: []string{"addr"}}); err != nil {
			t.Fatalf("Register #%d: %v", i, err)
		}
	}
	closest, err := dht.GetClosestNodes(t.Context(), "target", 10)
	if err != nil {
		t.Fatalf("GetClosestNodes: %v", err)
	}
	if len(closest) != 1 {
		t.Fatalf("重复注册应去重为 1 个候选, got %d", len(closest))
	}
}
