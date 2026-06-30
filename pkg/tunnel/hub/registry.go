// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"github.com/cocomhub/sproxy/pkg/plugin"
)

// DHTRegistry 是节点发现实现的插件注册表。
// 默认使用内置的内存 DHT 实现（newMemoryDHT）。
var DHTRegistry = plugin.New("dht", DHT(newMemoryDHT()))

// NewDHT 创建一个新的内存 DHT 实现，返回 DHT 接口。
func NewDHT() DHT {
	return newMemoryDHT()
}
