// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mockdht 提供 hub.DHT 的内存测试替身，供非 Kademlia 测试使用。
package mockdht

import (
	"context"
	"errors"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// MockDHT 实现 hub.DHT，可控 Lookup/Register 返回。
type MockDHT struct {
	RegisterFn    func(ctx context.Context, info hub.PeerInfo) error
	LookupFn      func(ctx context.Context, nodeID string) (hub.PeerInfo, error)
	CloseFn       func() error
	RegisterCalls int
	LookupCalls   int
	CloseCalls    int
	peers         map[string]hub.PeerInfo
}

// New 返回一个初始化的 *MockDHT，内部持有空 peers map。
func New() *MockDHT {
	return &MockDHT{peers: make(map[string]hub.PeerInfo)}
}

// Register 实现 hub.DHT.Register。
func (m *MockDHT) Register(ctx context.Context, info hub.PeerInfo) error {
	m.RegisterCalls++
	if m.RegisterFn != nil {
		return m.RegisterFn(ctx, info)
	}
	m.peers[info.ID] = info
	return nil
}

// Lookup 实现 hub.DHT.Lookup。
func (m *MockDHT) Lookup(ctx context.Context, nodeID string) (hub.PeerInfo, error) {
	m.LookupCalls++
	if m.LookupFn != nil {
		return m.LookupFn(ctx, nodeID)
	}
	if info, ok := m.peers[nodeID]; ok {
		return info, nil
	}
	return hub.PeerInfo{}, ErrPeerNotFound
}

// GetClosestNodes 实现 hub.DHT.GetClosestNodes。
func (m *MockDHT) GetClosestNodes(ctx context.Context, nodeID string, n int) ([]hub.PeerInfo, error) {
	return nil, nil
}

// Bootstrap 实现 hub.DHT.Bootstrap。
func (m *MockDHT) Bootstrap(ctx context.Context, seeds []string) error {
	return nil
}

// Close 实现 hub.DHT.Close。
func (m *MockDHT) Close() error {
	m.CloseCalls++
	if m.CloseFn != nil {
		return m.CloseFn()
	}
	return nil
}

// ErrPeerNotFound 是默认 Lookup 行为在未找到节点时返回的错误。
var ErrPeerNotFound = errors.New("mockdht: peer not found")
