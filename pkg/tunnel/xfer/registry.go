// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfer

import (
	"context"

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
)

// TransportRegistry 是传输层实现的插件注册表。
// 内置实现（internal/tcp、internal/http）在 init() 中以低优先级注册。
// 外部插件（ext/ws、ext/quic 等）以高优先级注册覆盖。
var TransportRegistry = plugin.New[*Transport]("xfer", emptyTransport())

func emptyTransport() *Transport {
	return &Transport{
		Name:   "builtin",
		Dial:   func(_ context.Context, _ string) (Conn, error) { return nil, ErrNoTransport },
		Listen: func(_ context.Context, _ string) (Listener, error) { return nil, ErrNoTransport },
	}
}

// Register 注册一个 Transport 到全局注册表。
// 兼容旧的 xfer.Register 调用方式。
func Register(t *Transport) {
	TransportRegistry.Register(plugin.Plugin[*Transport]{
		Name:     t.Name,
		Instance: t,
		Priority: 0,
	})
}

// Get 按名字查找已注册的 Transport。
// 未找到时返回 nil。
func Get(name string) *Transport {
	t, _ := TransportRegistry.Get(name)
	return t
}
