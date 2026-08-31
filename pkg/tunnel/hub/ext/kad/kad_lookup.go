// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kad

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// findNodeFunc is a function that queries a remote node for nodes closest to a target.
type findNodeFunc func(ctx context.Context, target NodeID, remote hub.PeerInfo) ([]hub.PeerInfo, error)

// Lookup performs an iterative Kademlia FindNode lookup for the target ID.
// It returns the k closest nodes to the target.
func (k *Kademlia) Lookup(ctx context.Context, target NodeID, findNode findNodeFunc) ([]hub.PeerInfo, error) {
	// Start with the closest nodes from the local routing table
	closest := k.FindClosest(target, bucketSize)
	if len(closest) == 0 {
		return nil, nil
	}

	queried := make(map[string]bool)

	for step := 0; step < maxLookupSteps; step++ {
		// Select α unqueried closest nodes
		toQuery := selectAlpha(closest, queried, alpha)
		if len(toQuery) == 0 {
			break
		}

		// Mark selected as queried
		for _, n := range toQuery {
			queried[n.ID] = true
		}

		// Query them concurrently
		type queryResult struct {
			peers []hub.PeerInfo
		}
		results := make(chan queryResult, len(toQuery))

		for _, node := range toQuery {
			go func(n hub.PeerInfo) {
				peers, err := findNode(ctx, target, n)
				if err != nil {
					return
				}
				results <- queryResult{peers: peers}
			}(node)
		}

		// Collect results
		newNodes := false
		for range toQuery {
			select {
			case r := <-results:
				for _, peer := range r.peers {
					if !queried[peer.ID] {
						queried[peer.ID] = true
						closest = append(closest, peer)
						newNodes = true
						k.Insert(peer)
					}
				}
			case <-ctx.Done():
				return closest, ctx.Err()
			}
		}

		// Sort by XOR distance to target
		sort.Slice(closest, func(i, j int) bool {
			di := target.Xor(NodeIDFromString(closest[i].ID))
			dj := target.Xor(NodeIDFromString(closest[j].ID))
			return di.Less(dj)
		})

		// Trim to k
		if len(closest) > bucketSize {
			closest = closest[:bucketSize]
		}

		if !newNodes {
			break
		}
	}

	return closest, nil
}

// selectAlpha selects up to α unqueried nodes from the closest list.
func selectAlpha(closest []hub.PeerInfo, queried map[string]bool, n int) []hub.PeerInfo {
	var result []hub.PeerInfo
	for _, node := range closest {
		if len(result) >= n {
			break
		}
		if !queried[node.ID] {
			result = append(result, node)
		}
	}
	return result
}

// KademliaDHT wraps Kademlia to implement the hub.DHT interface.
type KademliaDHT struct {
	kad    *Kademlia
	lookup findNodeFunc
}

// NewDHT creates a new Kademlia DHT that implements hub.DHT.
// id is the local node's identity string.
// lookup is the function to query remote nodes (can be nil for standalone use).
func NewDHT(id string, lookup findNodeFunc, logger *slog.Logger) *KademliaDHT {
	return &KademliaDHT{
		kad:    NewKademlia(id, logger),
		lookup: lookup,
	}
}

// Register adds a node to the routing table.
func (d *KademliaDHT) Register(_ context.Context, info hub.PeerInfo) error {
	d.kad.Insert(info)
	return nil
}

// Remove removes a node from the routing table (idempotent).
func (d *KademliaDHT) Remove(_ context.Context, nodeID string) error {
	d.kad.Remove(nodeID)
	return nil
}

// Lookup finds a specific node by ID.
func (d *KademliaDHT) Lookup(ctx context.Context, nodeID string) (hub.PeerInfo, error) {
	target := NodeIDFromString(nodeID)

	// Check local routing table first
	closest := d.kad.FindClosest(target, 1)
	for _, n := range closest {
		if n.ID == nodeID {
			return n, nil
		}
	}

	// If we have a lookup function, try iterative lookup
	if d.lookup != nil {
		closest, err := d.kad.Lookup(ctx, target, d.lookup)
		if err != nil {
			return hub.PeerInfo{}, err
		}
		for _, n := range closest {
			if n.ID == nodeID {
				return n, nil
			}
		}
	}

	return hub.PeerInfo{}, fmt.Errorf("kad: node %q not found", nodeID)
}

// GetClosestNodes returns the k closest nodes to the target ID.
func (d *KademliaDHT) GetClosestNodes(_ context.Context, targetID string, n int) ([]hub.PeerInfo, error) {
	target := NodeIDFromString(targetID)
	return d.kad.FindClosest(target, n), nil
}

// Bootstrap connects to seed nodes to join the DHT network.
// For now, it just inserts the seed nodes into the routing table.
func (d *KademliaDHT) Bootstrap(_ context.Context, seeds []string) error {
	for _, seed := range seeds {
		info := hub.PeerInfo{
			ID:    NodeIDFromString(seed).Hex(),
			Addrs: []string{seed},
		}
		d.kad.Insert(info)
	}
	return nil
}

// EnablePersistence 启用 k-bucket 持久化（委托 Kademlia）：先 Load 恢复已有快照
// （缓存语义，路由表仍 hub 权威），此后 Register/Remove 变更经去抖异步落盘。
// path 为空是 no-op（零行为变更）。Load 的 I/O 错误（非损坏/缺失类）原样返回。
func (d *KademliaDHT) EnablePersistence(path string) error {
	return d.kad.EnablePersistence(path)
}

// PersistFile 返回当前持久化文件路径（"" 表示持久化关闭）。
func (d *KademliaDHT) PersistFile() string {
	return d.kad.PersistFile()
}

// Close 同步落盘未持久化的 k-bucket 变更（去抖窗口内的最后变更不丢失），再清理
// 资源。持久化关闭时是 no-op。
func (d *KademliaDHT) Close() error {
	if err := d.kad.FlushPersist(); err != nil {
		return fmt.Errorf("kad: 关闭时持久化 k-bucket 失败: %w", err)
	}
	return nil
}

// Ensure KademliaDHT implements hub.DHT.
var _ hub.DHT = (*KademliaDHT)(nil)
