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

import (
	"context"
	"fmt"
	"io"
)

// Conn 是双向保序消息连接。
//
// 设计要点：
//   - Send(ctx, msg) 发送一条消息，远端 Receive 返回相同的 msg 内容
//   - 每条消息是独立的 []byte，消息边界由实现保证，上层无需定界
//   - 未使用 net.Conn 是因为它面向字节流而非消息，
//     且缺少 context.Context 支持（取消/超时需要额外包装）
//
// 典型实现：
//   - WebSocket：原生消息协议，直接映射
//   - gRPC 双向流：Send/Recv 原生消息
//   - HTTP POST：一次 Send+Receive 包装为一次 HTTP 往返
//   - TCP：需要额外帧定界包装
type Conn interface {
	// Send 发送一条消息。ctx 用于超时和取消。
	Send(ctx context.Context, msg []byte) error

	// Receive 阻塞接收一条消息。ctx 用于超时和取消。
	Receive(ctx context.Context) ([]byte, error)

	// Close 关闭连接。关闭后 Send/Receive 应返回 ErrConnClosed。
	io.Closer
}

// Listener 接受来自远端的连接（Hub/Server 端使用）。
type Listener interface {
	// Accept 阻塞等待一个新的 Conn 连接。
	Accept(ctx context.Context) (Conn, error)

	// Close 关闭监听器。
	io.Closer
}

// Transport 是传输层实现的注册单元。
type Transport struct {
	// Name 是传输层的唯一标识，用于注册表和配置引用。
	// 约定使用小写简称：如 "http"、"ws"、"grpc"、"quic"。
	Name string

	// Dial 创建一个到远端的新连接（客户端/Node 端使用）。
	Dial func(ctx context.Context, addr string) (Conn, error)

	// Listen 开始监听，返回 Listener（服务端/Hub 端使用）。
	Listen func(ctx context.Context, addr string) (Listener, error)
}

// registry 是全局传输层注册表，由 Register 在 init() 中填充。
var registry = make(map[string]*Transport)

// Register 注册一个 Transport 到全局注册表。
// 由传输层子模块在 init() 中调用。
// 重复注册同名 Transport 会 panic。
func Register(t *Transport) {
	if t == nil {
		panic("xfer: Register called with nil Transport")
	}
	if t.Name == "" {
		panic("xfer: Register called with empty Name")
	}
	if _, exists := registry[t.Name]; exists {
		panic(fmt.Sprintf("xfer: duplicate transport name: %s", t.Name))
	}
	registry[t.Name] = t
}

// Get 按名字查找已注册的 Transport。
// 未找到时返回 nil。
func Get(name string) *Transport {
	return registry[name]
}

// ErrConnClosed 是连接关闭后 Send/Receive 应返回的错误。
var ErrConnClosed = fmt.Errorf("xfer: connection closed")
