// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package hub 提供中继网络的核心类型与接口。
package hub

import "context"

// PeerInfo 描述 DHT 中一个发现节点的网络位置信息。
type PeerInfo struct {
	ID    string
	Addrs []string // xfer transport 地址列表，如 ["tcp://192.168.1.1:9000"]
	Meta  map[string]string
}

// DHT 定义节点发现的最低接口。
// 内置实现是简单的线程安全内存 map；ext/kad 提供完整的 Kademlia。
type DHT interface {
	// Register 将本节点注册到 DHT 网络。
	Register(ctx context.Context, node PeerInfo) error

	// Lookup 按节点 ID 查找目标节点信息。
	Lookup(ctx context.Context, nodeID string) (PeerInfo, error)

	// GetClosestNodes 返回距离目标 ID 最近的 N 个节点。
	// 距离算法由各实现定义（内置：词法排序；Kademlia：XOR 距离）。
	GetClosestNodes(ctx context.Context, nodeID string, n int) ([]PeerInfo, error)

	// Bootstrap 连接到已知种子节点，加入 DHT 网络。
	Bootstrap(ctx context.Context, seeds []string) error

	// Close 退出 DHT 网络，释放资源。
	Close() error
}
