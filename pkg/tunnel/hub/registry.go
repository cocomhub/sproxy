// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"github.com/cocomhub/sproxy/pkg/plugin"
)

// DHTRegistry 是节点发现实现的插件注册表。
// 默认使用内置的内存 DHT 实现（newMemoryDHT）。
var DHTRegistry = plugin.New("dht", DHT(newMemoryDHT()))

// RegisterDHT 注册一个命名的 DHT 实现到 DHTRegistry（cmd/sproxy 装配 Kademlia 用）。
// priority > 0 时优先于内置内存 DHT（Active 返回最高优先级实现）；同名重复注册
// 以后一次为准（注册顺序不变）。
func RegisterDHT(name string, dht DHT, priority int) {
	DHTRegistry.Register(plugin.Plugin[DHT]{Name: name, Instance: dht, Priority: priority})
}

// NewDHT 创建一个新的内存 DHT 实现，返回 DHT 接口。
func NewDHT() DHT {
	return newMemoryDHT()
}
