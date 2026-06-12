// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package xfer 定义传输层抽象接口和注册表。
//
// xfer 层是 sproxy tunnel 系统的最底层抽象，提供消息式双向连接接口。
// 任何传输层（WebSocket、gRPC 双向流、QUIC 流、HTTP POST）都可通过
// 实现 Conn 接口接入上层多路复用系统。
//
// 使用方式：
//
//	// 在传输层实现的 init() 中注册
//	func init() {
//	    xfer.Register(&xfer.Transport{
//	        Name: "ws",
//	        Dial: wsDial,
//	        Listen: wsListen,
//	    })
//	}
//
//	// 在应用层按名字获取
//	t := xfer.Get("ws")
//	if t != nil {
//	    conn, err := t.Dial(ctx, "ws://example.com/ws")
//	}
package xfer
