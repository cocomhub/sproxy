// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub_test

import (
    "sync"
    "testing"

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
    if got := rt.Lookup("node-1"); got == nil {
        t.Fatal("expected to find node-1")
    }
    if got := rt.Lookup("unknown"); got != nil {
        t.Fatal("expected nil for unknown node")
    }

    rt.Remove("node-1")
    if got := rt.Lookup("node-1"); got != nil {
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
