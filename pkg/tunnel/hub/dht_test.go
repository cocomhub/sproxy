// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"sync"
	"testing"
)

func TestDhtRegisterAndLookup(t *testing.T) {
	dht := newMemoryDHT()
	ctx := t.Context()

	// Register a node
	if err := dht.Register(ctx, PeerInfo{ID: "node-1", Addrs: []string{"192.168.1.10:9000"}, Meta: map[string]string{"region": "us-east"}}); err != nil {
		t.Fatal(err)
	}

	// Lookup should find it
	node, err := dht.Lookup(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if node.ID != "node-1" {
		t.Fatalf("expected ID node-1, got %s", node.ID)
	}
	if len(node.Addrs) == 0 || node.Addrs[0] != "192.168.1.10:9000" {
		t.Fatalf("expected addr 192.168.1.10:9000, got %v", node.Addrs)
	}
	if node.Meta["region"] != "us-east" {
		t.Fatalf("expected meta region us-east, got %s", node.Meta["region"])
	}

	// Lookup unknown node
	_, err = dht.Lookup(ctx, "unknown")
	if err == nil {
		t.Fatal("expected error for unknown node")
	}
}

func TestDhtRegisterOverwrite(t *testing.T) {
	dht := newMemoryDHT()
	ctx := t.Context()

	dht.Register(ctx, PeerInfo{ID: "node-1", Addrs: []string{"192.168.1.10:9000"}})
	dht.Register(ctx, PeerInfo{ID: "node-1", Addrs: []string{"10.0.0.1:9001"}, Meta: map[string]string{"region": "eu-west"}})

	node, err := dht.Lookup(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(node.Addrs) != 1 || node.Addrs[0] != "10.0.0.1:9001" {
		t.Fatalf("expected updated addr 10.0.0.1:9001, got %v", node.Addrs)
	}
	if node.Meta["region"] != "eu-west" {
		t.Fatalf("expected meta region eu-west after overwrite, got %s", node.Meta["region"])
	}
}

func TestDhtGetClosestNodes(t *testing.T) {
	dht := newMemoryDHT()
	ctx := t.Context()

	// Register nodes with IDs that sort lexicographically
	dht.Register(ctx, PeerInfo{ID: "alpha", Addrs: []string{"addr1"}})
	dht.Register(ctx, PeerInfo{ID: "bravo", Addrs: []string{"addr2"}})
	dht.Register(ctx, PeerInfo{ID: "charlie", Addrs: []string{"addr3"}})
	dht.Register(ctx, PeerInfo{ID: "delta", Addrs: []string{"addr4"}})
	dht.Register(ctx, PeerInfo{ID: "echo", Addrs: []string{"addr5"}})

	// Get 2 closest nodes to "bravo"
	closest, err := dht.GetClosestNodes(ctx, "bravo", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(closest) != 2 {
		t.Fatalf("expected 2 closest nodes, got %d", len(closest))
	}
	if closest[0].ID != "alpha" && closest[1].ID != "alpha" {
		t.Fatal("expected alpha to be among the 2 closest nodes to bravo")
	}

	// Get more nodes than available
	closest, err = dht.GetClosestNodes(ctx, "zzzz", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(closest) != 5 {
		t.Fatalf("expected 5 nodes (capped at total), got %d", len(closest))
	}

	// Get closest on empty DHT
	empty := newMemoryDHT()
	closest, err = empty.GetClosestNodes(ctx, "anything", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(closest) != 0 {
		t.Fatalf("expected 0 nodes from empty DHT, got %d", len(closest))
	}

	// Request 0 nodes
	closest, err = dht.GetClosestNodes(ctx, "bravo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(closest) != 0 {
		t.Fatalf("expected 0 nodes when n=0, got %d", len(closest))
	}
}

func TestDhtGetClosestNodesSelfExcluded(t *testing.T) {
	dht := newMemoryDHT()
	ctx := t.Context()

	dht.Register(ctx, PeerInfo{ID: "alpha", Addrs: []string{"addr1"}})
	dht.Register(ctx, PeerInfo{ID: "beta", Addrs: []string{"addr2"}})
	dht.Register(ctx, PeerInfo{ID: "gamma", Addrs: []string{"addr3"}})

	// Get closest to "beta" — "beta" itself should not be in the result
	closest, err := dht.GetClosestNodes(ctx, "beta", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range closest {
		if n.ID == "beta" {
			t.Fatal("expected target node itself to be excluded from closest nodes")
		}
	}
	if len(closest) != 2 {
		t.Fatalf("expected 2 closest nodes (excluding self), got %d", len(closest))
	}
}

func TestDhtConcurrent(t *testing.T) {
	dht := newMemoryDHT()
	ctx := t.Context()

	var wg sync.WaitGroup
	n := 50

	// Concurrent registration
	for i := range n {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			id := "node-" + string(rune('a'+i))
			dht.Register(ctx, PeerInfo{ID: id, Addrs: []string{"addr"}})
		}()
	}
	wg.Wait()

	// Concurrent lookup
	for i := range n {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			id := "node-" + string(rune('a'+i))
			_, err := dht.Lookup(ctx, id)
			if err != nil {
				t.Errorf("expected to find %s", id)
			}
		}()
	}
	wg.Wait()

	// Verify all nodes were registered
	for i := range n {
		id := "node-" + string(rune('a'+i))
		_, err := dht.Lookup(ctx, id)
		if err != nil {
			t.Fatalf("expected to find %s after concurrent registration", id)
		}
	}
}

func TestDhtBootstrapNoop(t *testing.T) {
	dht := newMemoryDHT()
	err := dht.Bootstrap(t.Context(), nil)
	if err != nil {
		t.Fatalf("expected no error from memory DHT Bootstrap, got %v", err)
	}
}

func TestDhtLookupWithMeta(t *testing.T) {
	dht := newMemoryDHT()
	ctx := t.Context()

	meta := map[string]string{
		"version": "1.0.0",
		"region":  "ap-southeast-1",
		"role":    "relay",
	}
	dht.Register(ctx, PeerInfo{ID: "node-meta", Addrs: []string{"10.0.0.1:9000"}, Meta: meta})

	node, err := dht.Lookup(ctx, "node-meta")
	if err != nil {
		t.Fatal(err)
	}
	if node.Meta["version"] != meta["version"] {
		t.Fatalf("expected meta version %s, got %s", meta["version"], node.Meta["version"])
	}
	if node.Meta["region"] != meta["region"] {
		t.Fatalf("expected meta region %s, got %s", meta["region"], node.Meta["region"])
	}
	if node.Meta["role"] != meta["role"] {
		t.Fatalf("expected meta role %s, got %s", meta["role"], node.Meta["role"])
	}
}

func TestDhtRegisterNilMeta(t *testing.T) {
	dht := newMemoryDHT()
	ctx := t.Context()

	dht.Register(ctx, PeerInfo{ID: "nil-meta", Addrs: []string{"addr"}})

	node, err := dht.Lookup(ctx, "nil-meta")
	if err != nil {
		t.Fatal(err)
	}
	if node.Meta == nil {
		t.Fatal("expected non-nil Meta after lookup, got nil")
	}
	if len(node.Meta) != 0 {
		t.Fatalf("expected empty Meta, got %v", node.Meta)
	}
}

func TestMemoryDHT_Close(t *testing.T) {
	dht := newMemoryDHT()
	if err := dht.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewDHT(t *testing.T) {
	dht := NewDHT()
	if dht == nil {
		t.Fatal("NewDHT() returned nil")
	}
	// 验证返回的是 memoryDHT，实现了 Close
	if err := dht.Close(); err != nil {
		t.Fatal(err)
	}
}
