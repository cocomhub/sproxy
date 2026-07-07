// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kad

import (
	"context"
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// NewDHTNode creates a new Kademlia DHT node with the given identity.
// This is the primary factory function users call to instantiate a DHT node.
// id is the local node's identity string (used to derive the Kademlia NodeID via SHA-256).
// lookup is the function to query remote nodes for iterative FindNode (can be nil for standalone).
func NewDHTNode(id string, lookup func(ctx context.Context, target NodeID, remote hub.PeerInfo) ([]hub.PeerInfo, error), logger *slog.Logger) hub.DHT {
	return NewDHT(id, lookup, logger)
}
