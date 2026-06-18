// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mockdht_test

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil/mockdht"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

func TestMockDHT_RegisterAndLookup(t *testing.T) {
	dht := mockdht.New()
	info := hub.PeerInfo{ID: "node-a", Addrs: []string{"pipe://addr"}}
	if err := dht.Register(context.Background(), info); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	got, err := dht.Lookup(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if got.ID != "node-a" {
		t.Fatalf("expected ID 'node-a', got %q", got.ID)
	}
	if dht.RegisterCalls != 1 || dht.LookupCalls != 1 {
		t.Fatal("call counts mismatch")
	}
}

func TestMockDHT_LookupNotFound(t *testing.T) {
	dht := mockdht.New()
	_, err := dht.Lookup(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown peer")
	}
}

func TestMockDHT_InjectError(t *testing.T) {
	dht := mockdht.New()
	dht.LookupFn = func(_ context.Context, _ string) (hub.PeerInfo, error) {
		return hub.PeerInfo{}, mockdht.ErrPeerNotFound
	}
	_, err := dht.Lookup(context.Background(), "any")
	if err != mockdht.ErrPeerNotFound {
		t.Fatalf("expected ErrPeerNotFound, got %v", err)
	}
}

func TestMockDHT_DoubleClose(t *testing.T) {
	dht := mockdht.New()
	if err := dht.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dht.Close(); err != nil {
		t.Fatal(err)
	}
	if dht.CloseCalls != 2 {
		t.Fatalf("expected 2 Close calls, got %d", dht.CloseCalls)
	}
}
