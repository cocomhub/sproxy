// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub_test

import (
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestRouteTableAddAndRemove(t *testing.T) {
	rt := hub.NewRouteTable()
	a, b := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	_ = b

	rt.Add("node-1", m)
	if rt.Lookup("node-1") == nil {
		t.Fatal("expected to find node-1")
	}
	if rt.Lookup("unknown") != nil {
		t.Fatal("expected nil for unknown node")
	}

	rt.Remove("node-1")
	if rt.Lookup("node-1") != nil {
		t.Fatal("expected nil after remove")
	}
}

func TestRouteTableConcurrent(t *testing.T) {
	rt := hub.NewRouteTable()
	var wg sync.WaitGroup

	for i := range 10 {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			a, _ := xfertest.Pipe()
			m := mux.New(a, mux.RoleDialer)
			id := hub.NodeID(rune('a' + i))
			rt.Add(id, m)
		}()
	}
	wg.Wait()

	nodes := rt.List()
	if len(nodes) != 10 {
		t.Fatalf("expected 10 nodes, got %d", len(nodes))
	}
}

func TestRouteTableEmptyList(t *testing.T) {
	rt := hub.NewRouteTable()
	nodes := rt.List()
	if len(nodes) != 0 {
		t.Fatalf("expected empty list, got %d", len(nodes))
	}
}

func TestRouteTableAddWithInfo(t *testing.T) {
	rt := hub.NewRouteTable()
	a, b := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	_ = b

	rt.AddWithInfo(hub.NodeInfo{
		ID:        "node-with-info",
		Mux:       m,
		Connected: time.Now(),
		Addr:      "127.0.0.1:8080",
		Token:     "sec-***",
	})

	// Lookup should work
	if rt.Lookup("node-with-info") == nil {
		t.Fatal("expected to find node-with-info")
	}

	// List should include info
	nodes := rt.List()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].Addr != "127.0.0.1:8080" {
		t.Fatalf("expected addr 127.0.0.1:8080, got %s", nodes[0].Addr)
	}
	if nodes[0].Token != "sec-***" {
		t.Fatalf("expected token sec-***, got %s", nodes[0].Token)
	}
	if nodes[0].Connected.IsZero() {
		t.Fatal("expected non-zero Connected time")
	}
}

func TestRouteTableDuplicateReplace(t *testing.T) {
	rt := hub.NewRouteTable()
	a1, b1 := xfertest.Pipe()
	m1 := mux.New(a1, mux.RoleDialer)
	_ = b1

	a2, b2 := xfertest.Pipe()
	m2 := mux.New(a2, mux.RoleDialer)
	_ = b2

	rt.Add("same-node", m1)
	rt.Add("same-node", m2)

	// Should point to m2 now
	if rt.Lookup("same-node") != m2 {
		t.Fatal("expected lookup to return new mux after replace")
	}
}

func TestRouteTableNodeCount(t *testing.T) {
	rt := hub.NewRouteTable()
	if c := rt.NodeCount(); c != 0 {
		t.Fatalf("expected 0, got %d", c)
	}
	rt.Add("a", nil)
	rt.Add("b", nil)
	if c := rt.NodeCount(); c != 2 {
		t.Fatalf("expected 2, got %d", c)
	}
	rt.Remove("a")
	if c := rt.NodeCount(); c != 1 {
		t.Fatalf("expected 1, got %d", c)
	}
}
