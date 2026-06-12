// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"fmt"
	"maps"
	"math"
	"sort"
	"sync"
)

// memoryDHT 是一个生产可用但去中心化颗粒度最低的 DHT 实现。
// 适合单机或小规模固定节点拓扑，不依赖任何外部发现协议。
type memoryDHT struct {
	mu    sync.RWMutex
	nodes map[string]PeerInfo
}

// newMemoryDHT 创建新的内存 DHT。
func newMemoryDHT() *memoryDHT {
	return &memoryDHT{nodes: make(map[string]PeerInfo)}
}

func (d *memoryDHT) Register(_ context.Context, node PeerInfo) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	info := PeerInfo{
		ID:    node.ID,
		Addrs: append([]string(nil), node.Addrs...),
		Meta:  make(map[string]string, len(node.Meta)),
	}
	maps.Copy(info.Meta, node.Meta)
	d.nodes[node.ID] = info
	return nil
}

func (d *memoryDHT) Lookup(_ context.Context, nodeID string) (PeerInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	node, ok := d.nodes[nodeID]
	if !ok {
		return PeerInfo{}, fmt.Errorf("dht: node %q not found", nodeID)
	}
	return node, nil
}

func (d *memoryDHT) GetClosestNodes(_ context.Context, targetID string, n int) ([]PeerInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if n <= 0 || len(d.nodes) == 0 {
		return nil, nil
	}

	type kv struct {
		id   string
		node PeerInfo
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
	result := make([]PeerInfo, end)
	for i := range end {
		result[i] = sorted[i].node
	}
	return result, nil
}

func (d *memoryDHT) Bootstrap(_ context.Context, _ []string) error {
	return nil
}

func (d *memoryDHT) Close() error {
	return nil
}
