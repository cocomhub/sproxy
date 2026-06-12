// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"fmt"
	"maps"
	"math"
	"sort"
	"sync"
)

// DHTNode represents a node discovered via the DHT.
type DHTNode struct {
	ID   string
	Addr string
	Meta map[string]string
}

// DHT is a simple in-memory DHT node discovery table.
// It is thread-safe and uses a map to store nodes keyed by ID.
// This is a skeleton intended to be replaced by a real Kademlia DHT later.
type DHT struct {
	mu    sync.RWMutex
	nodes map[string]DHTNode
}

// NewDHT creates a new, empty DHT.
func NewDHT() *DHT {
	return &DHT{
		nodes: make(map[string]DHTNode),
	}
}

// Register adds or updates a node in the DHT.
// If a node with the same ID already exists, it is overwritten.
func (d *DHT) Register(id, addr string, meta map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m := make(map[string]string, len(meta))
	maps.Copy(m, meta)
	d.nodes[id] = DHTNode{
		ID:   id,
		Addr: addr,
		Meta: m,
	}
}

// Lookup retrieves a node by ID. Returns the node and true if found.
func (d *DHT) Lookup(id string) (DHTNode, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	node, ok := d.nodes[id]
	return node, ok
}

// GetClosestNodes returns the n closest nodes to targetID, excluding the
// target node itself. Closeness is determined by simple lexicographic
// (string) comparison of IDs, simulating a Kademlia-style distance metric.
// Returns fewer than n nodes if the DHT has fewer nodes.
func (d *DHT) GetClosestNodes(targetID string, n int) []DHTNode {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if n <= 0 || len(d.nodes) == 0 {
		return nil
	}

	type kv struct {
		id   string
		node DHTNode
	}

	sorted := make([]kv, 0, len(d.nodes))
	for id, node := range d.nodes {
		if id == targetID {
			continue
		}
		sorted = append(sorted, kv{id: id, node: node})
	}

	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].id < sorted[j].id
	})

	end := int(math.Min(float64(n), float64(len(sorted))))
	result := make([]DHTNode, end)
	for i := range end {
		result[i] = sorted[i].node
	}
	return result
}

// Bootstrap connects to a seed node to join the DHT network.
// This is a skeleton: it always returns an error indicating the feature
// is not yet implemented.
func (d *DHT) Bootstrap(bootstrapAddr string) error {
	return fmt.Errorf("not yet implemented")
}
