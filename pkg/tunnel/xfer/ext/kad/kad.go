// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package kad implements a Kademlia Distributed Hash Table for node discovery.
//
// It provides a full Kademlia routing table with XOR-distance-based bucket
// management, iterative FindNode lookup, and the hub.DHT interface.
//
// The implementation uses only the standard library. Node IDs are SHA-256
// hashes of the node's identity string.
package kad

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"math/bits"
	"sort"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

const (
	// keyBits is the number of bits in a Kademlia node ID (SHA-256).
	keyBits = 256

	// bucketSize is the maximum number of nodes per k-bucket.
	bucketSize = 20

	// alpha is the concurrency factor for iterative lookups.
	alpha = 3

	// maxLookupSteps is the maximum number of iterative lookup rounds.
	maxLookupSteps = 16
)

// NodeID is a 256-bit Kademlia node identifier.
type NodeID [32]byte

// NodeIDFromString creates a NodeID by SHA-256 hashing the input string.
func NodeIDFromString(s string) NodeID {
	return sha256.Sum256([]byte(s))
}

// NodeIDFromHex parses a hex-encoded NodeID.
func NodeIDFromHex(s string) (NodeID, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return NodeID{}, err
	}
	var id NodeID
	copy(id[:], b[:32])
	return id, nil
}

// Hex returns the hex-encoded representation of the NodeID.
func (n NodeID) Hex() string {
	return hex.EncodeToString(n[:])
}

// Xor returns the XOR distance between two NodeIDs.
func (n NodeID) Xor(other NodeID) NodeID {
	var result NodeID
	for i := range n {
		result[i] = n[i] ^ other[i]
	}
	return result
}

// PrefixLen returns the number of leading zero bits in the XOR distance.
// This determines the k-bucket index.
func (n NodeID) PrefixLen() int {
	for i := 0; i < len(n); i++ {
		if n[i] != 0 {
			return i*8 + bits.LeadingZeros8(n[i])
		}
	}
	return keyBits
}

// Less compares two NodeIDs lexicographically.
func (n NodeID) Less(other NodeID) bool {
	for i := range n {
		if n[i] != other[i] {
			return n[i] < other[i]
		}
	}
	return false
}

// kadNode is a node in the routing table.
type kadNode struct {
	info     hub.PeerInfo
	lastSeen time.Time
	online   bool
}

// Bucket is a Kademlia k-bucket containing up to bucketSize nodes.
type Bucket struct {
	mu    sync.Mutex
	nodes []*kadNode
}

// newBucket creates an empty bucket.
func newBucket() *Bucket {
	return &Bucket{
		nodes: make([]*kadNode, 0, bucketSize),
	}
}

// addNode adds or updates a node in the bucket.
// Returns true if the node was added, false if the bucket is full and the node was rejected.
func (b *Bucket) addNode(node *kadNode) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Update existing node
	for i, n := range b.nodes {
		if n.info.ID == node.info.ID {
			b.nodes[i].lastSeen = node.lastSeen
			b.nodes[i].online = node.online
			b.nodes[i].info = node.info
			// Move to front (most recently seen)
			b.moveToFront(i)
			return true
		}
	}

	// Add new node if bucket not full
	if len(b.nodes) < bucketSize {
		b.nodes = append(b.nodes, node)
		return true
	}

	// Bucket is full — check if the first (least recently seen) node is still online
	// If it's offline, replace it; otherwise reject the new node.
	first := b.nodes[0]
	if !first.online {
		b.nodes[0] = node
		b.moveToFront(0)
		return true
	}
	return false
}

// removeNode removes a node by ID.
func (b *Bucket) removeNode(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, n := range b.nodes {
		if n.info.ID == id {
			b.nodes = append(b.nodes[:i], b.nodes[i+1:]...)
			return
		}
	}
}

// getNodes returns a copy of all nodes in the bucket.
func (b *Bucket) getNodes() []*kadNode {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]*kadNode, len(b.nodes))
	copy(result, b.nodes)
	return result
}

// moveToFront moves the node at index i to the front (most recently seen).
func (b *Bucket) moveToFront(i int) {
	node := b.nodes[i]
	copy(b.nodes[1:], b.nodes[:i])
	b.nodes[0] = node
}

// Kademlia implements the Kademlia DHT routing table.
type Kademlia struct {
	id      NodeID
	buckets [keyBits]*Bucket
	logger  *slog.Logger
	mu      sync.RWMutex
}

// NewKademlia creates a new Kademlia instance with the given node ID.
func NewKademlia(id string, logger *slog.Logger) *Kademlia {
	k := &Kademlia{
		id:     NodeIDFromString(id),
		logger: defaultLogger(logger),
	}
	for i := range k.buckets {
		k.buckets[i] = newBucket()
	}
	return k
}

// NodeID returns this node's Kademlia ID.
func (k *Kademlia) NodeID() NodeID {
	return k.id
}

// bucketIndex returns the bucket index for the given NodeID (XOR distance).
func (k *Kademlia) bucketIndex(target NodeID) int {
	dist := k.id.Xor(target)
	pl := dist.PrefixLen()
	if pl >= keyBits {
		return keyBits - 1
	}
	return pl
}

// Insert adds or updates a node in the appropriate k-bucket.
func (k *Kademlia) Insert(info hub.PeerInfo) {
	node := &kadNode{
		info:     info,
		lastSeen: time.Now(),
		online:   true,
	}
	idx := k.bucketIndex(NodeIDFromString(info.ID))
	k.buckets[idx].addNode(node)
}

// Remove removes a node from the routing table.
func (k *Kademlia) Remove(id string) {
	idx := k.bucketIndex(NodeIDFromString(id))
	k.buckets[idx].removeNode(id)
}

// FindClosest returns the k closest nodes to the target ID from the routing table.
// It searches from the closest bucket outward and returns nodes sorted by XOR distance.
func (k *Kademlia) FindClosest(target NodeID, n int) []hub.PeerInfo {
	seen := make(map[string]bool)
	var all []hub.PeerInfo

	idx := k.bucketIndex(target)

	// Search outward from the closest bucket
	for i := 0; i < keyBits; i++ {
		bidx := idx + i
		if bidx < keyBits {
			k.collectBucket(bidx, &all, &seen)
		}
		if i > 0 {
			bidx2 := idx - i
			if bidx2 >= 0 {
				k.collectBucket(bidx2, &all, &seen)
			}
		}
	}

	// Sort by XOR distance to target
	sort.Slice(all, func(i, j int) bool {
		di := target.Xor(NodeIDFromString(all[i].ID))
		dj := target.Xor(NodeIDFromString(all[j].ID))
		return di.Less(dj)
	})

	if len(all) > n {
		all = all[:n]
	}
	return all
}

// collectBucket collects nodes from a single bucket.
func (k *Kademlia) collectBucket(bidx int, result *[]hub.PeerInfo, seen *map[string]bool) {
	nodes := k.buckets[bidx].getNodes()
	for _, node := range nodes {
		if (*seen)[node.info.ID] {
			continue
		}
		(*seen)[node.info.ID] = true
		*result = append(*result, node.info)
	}
}

// defaultLogger returns a logger that discards output if nil.
func defaultLogger(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.NewTextHandler(nil, nil))
}
