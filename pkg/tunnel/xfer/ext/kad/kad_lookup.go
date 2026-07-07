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

// lookupResult is the result of a single FindNode query.
type lookupResult struct {
	closest []hub.PeerInfo
	err     error
}

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

// Close cleans up resources.
func (d *KademliaDHT) Close() error {
	return nil
}

// Ensure KademliaDHT implements hub.DHT.
var _ hub.DHT = (*KademliaDHT)(nil)
