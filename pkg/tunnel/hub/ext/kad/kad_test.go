// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kad

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

func TestNodeIDFromString(t *testing.T) {
	id1 := NodeIDFromString("node-1")
	id2 := NodeIDFromString("node-1")
	id3 := NodeIDFromString("node-2")

	if id1 != id2 {
		t.Fatal("same input should produce same NodeID")
	}
	if id1 == id3 {
		t.Fatal("different input should produce different NodeID")
	}
}

func TestNodeIDXor(t *testing.T) {
	a := NodeIDFromString("alpha")
	b := NodeIDFromString("beta")
	xor := a.Xor(b)

	// XOR is commutative
	xor2 := b.Xor(a)
	if xor != xor2 {
		t.Fatal("XOR should be commutative")
	}

	// Self XOR is zero
	zero := a.Xor(a)
	var expected NodeID
	if zero != expected {
		t.Fatal("self XOR should be zero")
	}
}

func TestNodeIDPrefixLen(t *testing.T) {
	// Two identical IDs should have prefix length = keyBits
	a := NodeIDFromString("same")
	dist := a.Xor(a)
	if dist.PrefixLen() != keyBits {
		t.Fatalf("expected prefixLen=%d for self, got %d", keyBits, dist.PrefixLen())
	}

	// Two different IDs should have a smaller prefix length
	b := NodeIDFromString("different")
	dist2 := a.Xor(b)
	if dist2.PrefixLen() >= keyBits {
		t.Fatal("expected prefixLen < keyBits for different IDs")
	}
}

func TestBucketAddNode(t *testing.T) {
	b := newBucket()

	// Add a node
	node := &kadNode{
		info: hub.PeerInfo{ID: "node-1", Addrs: []string{"addr1"}},
	}
	if !b.addNode(node) {
		t.Fatal("expected addNode to succeed")
	}
	if len(b.nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(b.nodes))
	}

	// Update existing node
	node2 := &kadNode{
		info: hub.PeerInfo{ID: "node-1", Addrs: []string{"addr2"}},
	}
	if !b.addNode(node2) {
		t.Fatal("expected addNode to update existing node")
	}
	if len(b.nodes) != 1 {
		t.Fatalf("expected 1 node after update, got %d", len(b.nodes))
	}
	if b.nodes[0].info.Addrs[0] != "addr2" {
		t.Fatalf("expected updated addr, got %v", b.nodes[0].info.Addrs)
	}
}

func TestBucketFull(t *testing.T) {
	b := newBucket()

	// Fill the bucket
	for i := 0; i < bucketSize; i++ {
		id := string(rune('a' + i))
		node := &kadNode{
			info:   hub.PeerInfo{ID: id, Addrs: []string{"addr"}},
			online: true,
		}
		if !b.addNode(node) {
			t.Fatalf("expected addNode %d to succeed", i)
		}
	}

	// Bucket is full with online nodes, new node should be rejected
	newNode := &kadNode{
		info:   hub.PeerInfo{ID: "new", Addrs: []string{"addr"}},
		online: true,
	}
	if b.addNode(newNode) {
		t.Fatal("expected addNode to reject when bucket is full with online nodes")
	}
	if len(b.nodes) != bucketSize {
		t.Fatalf("expected bucket size %d, got %d", bucketSize, len(b.nodes))
	}
}

func TestBucketReplaceOffline(t *testing.T) {
	b := newBucket()

	// Fill the bucket with offline nodes
	for i := 0; i < bucketSize; i++ {
		id := string(rune('a' + i))
		node := &kadNode{
			info:   hub.PeerInfo{ID: id, Addrs: []string{"addr"}},
			online: false,
		}
		b.addNode(node)
	}

	// Replace the first (oldest) offline node
	newNode := &kadNode{
		info:   hub.PeerInfo{ID: "new", Addrs: []string{"addr"}},
		online: true,
	}
	if !b.addNode(newNode) {
		t.Fatal("expected addNode to replace oldest offline node")
	}
	if len(b.nodes) != bucketSize {
		t.Fatalf("expected bucket size %d, got %d", bucketSize, len(b.nodes))
	}
	// New node should be at the front
	if b.nodes[0].info.ID != "new" {
		t.Fatalf("expected new node at front, got %s", b.nodes[0].info.ID)
	}
}

func TestKademliaInsert(t *testing.T) {
	k := NewKademlia("local-node", nil)

	// Insert a node
	k.Insert(hub.PeerInfo{ID: "remote-node", Addrs: []string{"192.168.1.1:9000"}})

	closest := k.FindClosest(NodeIDFromString("remote-node"), 5)
	if len(closest) != 1 {
		t.Fatalf("expected 1 node, got %d", len(closest))
	}
	if closest[0].ID != "remote-node" {
		t.Fatalf("expected remote-node, got %s", closest[0].ID)
	}
}

func TestKademliaFindClosest(t *testing.T) {
	k := NewKademlia("local-node", nil)

	// Insert multiple nodes
	nodes := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	for _, n := range nodes {
		k.Insert(hub.PeerInfo{ID: n, Addrs: []string{"addr"}})
	}

	// Find closest to a target
	closest := k.FindClosest(NodeIDFromString("target"), 3)
	if len(closest) != 3 {
		t.Fatalf("expected 3 closest nodes, got %d", len(closest))
	}
}

func TestKademliaInsertAndRemove(t *testing.T) {
	k := NewKademlia("local-node", nil)

	k.Insert(hub.PeerInfo{ID: "node-1", Addrs: []string{"addr"}})
	k.Remove("node-1")

	closest := k.FindClosest(NodeIDFromString("node-1"), 5)
	if len(closest) != 0 {
		t.Fatalf("expected 0 nodes after remove, got %d", len(closest))
	}
}

func TestKademliaDHT_Register(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	defer dht.Close()

	err := dht.Register(t.Context(), hub.PeerInfo{ID: "node-1", Addrs: []string{"addr"}})
	if err != nil {
		t.Fatal(err)
	}

	info, err := dht.Lookup(t.Context(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "node-1" {
		t.Fatalf("expected node-1, got %s", info.ID)
	}
}

func TestKademliaDHT_GetClosestNodes(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	defer dht.Close()

	dht.Register(t.Context(), hub.PeerInfo{ID: "a", Addrs: []string{"addr"}})
	dht.Register(t.Context(), hub.PeerInfo{ID: "b", Addrs: []string{"addr"}})
	dht.Register(t.Context(), hub.PeerInfo{ID: "c", Addrs: []string{"addr"}})

	closest, err := dht.GetClosestNodes(t.Context(), "target", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(closest) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(closest))
	}
}

func TestKademliaDHT_LookupNotFound(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	defer dht.Close()

	_, err := dht.Lookup(t.Context(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent node")
	}
}

func TestKademliaDHT_Bootstrap(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	defer dht.Close()

	err := dht.Bootstrap(t.Context(), []string{"seed1", "seed2"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestKademliaDHT_Close(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	if err := dht.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectAlpha(t *testing.T) {
	closest := []hub.PeerInfo{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}
	queried := map[string]bool{"a": true, "c": true}

	selected := selectAlpha(closest, queried, 2)
	if len(selected) != 2 {
		t.Fatalf("expected 2 selected, got %d", len(selected))
	}
	if selected[0].ID != "b" || selected[1].ID != "d" {
		t.Fatalf("expected b and d, got %v", selected)
	}
}
