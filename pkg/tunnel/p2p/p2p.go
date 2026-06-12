// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package p2p provides a peer-to-peer transport layer that combines DHT discovery
// with WebRTC transport and stream multiplexing into a complete P2P link.
package p2p

import (
	"context"
	"fmt"
	"sync"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// P2PNode represents a peer-to-peer node that uses DHT for discovery,
// WebRTC for transport, and mux for stream multiplexing.
type P2PNode struct {
	ID  string
	DHT *hub.DHT

	listener xfer.Listener

	acceptCh chan *mux.Mux
	mu       sync.Mutex
	done     chan struct{}
}

// NewP2PNode creates a new P2P node with the given ID and DHT.
func NewP2PNode(id string, dht *hub.DHT) *P2PNode {
	return &P2PNode{
		ID:       id,
		DHT:      dht,
		acceptCh: make(chan *mux.Mux, 16),
		done:     make(chan struct{}),
	}
}

// Dial looks up targetID in the DHT, establishes a WebRTC connection, and
// returns a mux.Mux (RoleDialer) for stream multiplexing.
func (n *P2PNode) Dial(ctx context.Context, targetID string) (*mux.Mux, error) {
	node, ok := n.DHT.Lookup(targetID)
	if !ok {
		return nil, fmt.Errorf("p2p: target %q not found in DHT", targetID)
	}

	transport := xfer.Get("webrtc")
	if transport == nil {
		return nil, fmt.Errorf("p2p: webrtc transport not registered")
	}

	conn, err := transport.Dial(ctx, node.Addr)
	if err != nil {
		return nil, fmt.Errorf("p2p: dial %q (%s): %w", targetID, node.Addr, err)
	}

	return mux.New(conn, mux.RoleDialer), nil
}

// Listen starts listening for incoming P2P connections on the given address,
// registers this node in the DHT, and launches an accept loop. Callers
// retrieve new connections via Accept.
func (n *P2PNode) Listen(ctx context.Context, addr string) error {
	transport := xfer.Get("webrtc")
	if transport == nil {
		return fmt.Errorf("p2p: webrtc transport not registered")
	}

	listener, err := transport.Listen(ctx, addr)
	if err != nil {
		return fmt.Errorf("p2p: listen on %s: %w", addr, err)
	}
	n.listener = listener

	// Register self in DHT so peers can discover this node.
	n.DHT.Register(n.ID, addr, nil)

	go n.acceptLoop(ctx)
	return nil
}

// Accept blocks until a new P2P connection is established, then returns the
// corresponding mux.Mux (RoleListener).
func (n *P2PNode) Accept(ctx context.Context) (*mux.Mux, error) {
	select {
	case m := <-n.acceptCh:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.done:
		return nil, fmt.Errorf("p2p: node closed")
	}
}

// Close shuts down the node, closing the listener and all pending accepts.
func (n *P2PNode) Close() error {
	n.mu.Lock()
	select {
	case <-n.done:
		n.mu.Unlock()
		return nil
	default:
		close(n.done)
	}
	n.mu.Unlock()

	if n.listener != nil {
		return n.listener.Close()
	}
	return nil
}

// acceptLoop runs in a goroutine, accepting incoming transport connections,
// wrapping each in a mux.Mux (RoleListener), and delivering them via acceptCh.
func (n *P2PNode) acceptLoop(ctx context.Context) {
	for {
		conn, err := n.listener.Accept(ctx)
		if err != nil {
			return
		}
		m := mux.New(conn, mux.RoleListener)
		select {
		case n.acceptCh <- m:
		case <-n.done:
			m.Close()
			return
		}
	}
}
