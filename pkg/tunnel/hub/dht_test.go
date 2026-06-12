// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"sync"
	"testing"
)

func TestDhtRegisterAndLookup(t *testing.T) {
	dht := NewDHT()

	// Register a node
	dht.Register("node-1", "192.168.1.10:9000", map[string]string{"region": "us-east"})

	// Lookup should find it
	node, ok := dht.Lookup("node-1")
	if !ok {
		t.Fatal("expected to find node-1")
	}
	if node.ID != "node-1" {
		t.Fatalf("expected ID node-1, got %s", node.ID)
	}
	if node.Addr != "192.168.1.10:9000" {
		t.Fatalf("expected addr 192.168.1.10:9000, got %s", node.Addr)
	}
	if node.Meta["region"] != "us-east" {
		t.Fatalf("expected meta region us-east, got %s", node.Meta["region"])
	}

	// Lookup unknown node
	_, ok = dht.Lookup("unknown")
	if ok {
		t.Fatal("expected false for unknown node")
	}
}

func TestDhtRegisterOverwrite(t *testing.T) {
	dht := NewDHT()

	dht.Register("node-1", "192.168.1.10:9000", nil)
	dht.Register("node-1", "10.0.0.1:9001", map[string]string{"region": "eu-west"})

	node, ok := dht.Lookup("node-1")
	if !ok {
		t.Fatal("expected to find node-1 after overwrite")
	}
	if node.Addr != "10.0.0.1:9001" {
		t.Fatalf("expected updated addr 10.0.0.1:9001, got %s", node.Addr)
	}
	if node.Meta["region"] != "eu-west" {
		t.Fatalf("expected meta region eu-west after overwrite, got %s", node.Meta["region"])
	}
}

func TestDhtGetClosestNodes(t *testing.T) {
	dht := NewDHT()

	// Register nodes with IDs that sort lexicographically
	dht.Register("alpha", "addr1", nil)
	dht.Register("bravo", "addr2", nil)
	dht.Register("charlie", "addr3", nil)
	dht.Register("delta", "addr4", nil)
	dht.Register("echo", "addr5", nil)

	// Get 2 closest nodes to "bravo"
	closest := dht.GetClosestNodes("bravo", 2)
	if len(closest) != 2 {
		t.Fatalf("expected 2 closest nodes, got %d", len(closest))
	}
	// "alpha" and "bravo" itself but self excluded, so alpha and charlie
	if closest[0].ID != "alpha" && closest[1].ID != "alpha" {
		t.Fatal("expected alpha to be among the 2 closest nodes to bravo")
	}

	// Get more nodes than available
	closest = dht.GetClosestNodes("zzzz", 100)
	if len(closest) != 5 {
		t.Fatalf("expected 5 nodes (capped at total), got %d", len(closest))
	}

	// Get closest on empty DHT
	empty := NewDHT()
	closest = empty.GetClosestNodes("anything", 3)
	if len(closest) != 0 {
		t.Fatalf("expected 0 nodes from empty DHT, got %d", len(closest))
	}

	// Request 0 nodes
	closest = dht.GetClosestNodes("bravo", 0)
	if len(closest) != 0 {
		t.Fatalf("expected 0 nodes when n=0, got %d", len(closest))
	}
}

func TestDhtGetClosestNodesSelfExcluded(t *testing.T) {
	dht := NewDHT()

	dht.Register("alpha", "addr1", nil)
	dht.Register("beta", "addr2", nil)
	dht.Register("gamma", "addr3", nil)

	// Get closest to "beta" — "beta" itself should not be in the result
	closest := dht.GetClosestNodes("beta", 3)
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
	dht := NewDHT()

	var wg sync.WaitGroup
	n := 50

	// Concurrent registration
	for i := range n {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			id := "node-" + string(rune('a'+i))
			dht.Register(id, "addr", nil)
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
			_, ok := dht.Lookup(id)
			if !ok {
				t.Errorf("expected to find %s", id)
			}
		}()
	}
	wg.Wait()

	// Verify all nodes were registered
	for i := range n {
		id := "node-" + string(rune('a'+i))
		_, ok := dht.Lookup(id)
		if !ok {
			t.Fatalf("expected to find %s after concurrent registration", id)
		}
	}
}

func TestDhtBootstrapNotImplemented(t *testing.T) {
	dht := NewDHT()
	err := dht.Bootstrap("seed.example.com:9000")
	if err == nil {
		t.Fatal("expected error from Bootstrap, got nil")
	}
	if err.Error() != "not yet implemented" {
		t.Fatalf("expected 'not yet implemented', got %q", err.Error())
	}
}

func TestDhtLookupWithMeta(t *testing.T) {
	dht := NewDHT()

	meta := map[string]string{
		"version": "1.0.0",
		"region":  "ap-southeast-1",
		"role":    "relay",
	}
	dht.Register("node-meta", "10.0.0.1:9000", meta)

	node, ok := dht.Lookup("node-meta")
	if !ok {
		t.Fatal("expected to find node-meta")
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
	dht := NewDHT()

	dht.Register("nil-meta", "addr", nil)

	node, ok := dht.Lookup("nil-meta")
	if !ok {
		t.Fatal("expected to find nil-meta")
	}
	if node.Meta == nil {
		t.Fatal("expected non-nil Meta after lookup, got nil")
	}
	if len(node.Meta) != 0 {
		t.Fatalf("expected empty Meta, got %v", node.Meta)
	}
}
