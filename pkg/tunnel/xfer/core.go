// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

// Flusher 是可选接口：实现该接口的连接可确保之前 Send 的消息已真正写出。
// 某些传输（如 WebSocket 的异步发送通道）在 Close 前若不 Flush，排队中的
// 关键帧会被 Close 掐掉，导致对端只收到 EOF。发送方需要在关闭前保证对端
// 收到某条消息时，可断言 conn.(xfer.Flusher) 后调用。
type Flusher interface {
	// Flush 等待队列中所有已 Send 的消息真正写出，并返回写结果。
	Flush(ctx context.Context) error
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

// ErrConnClosed 是连接关闭后 Send/Receive 应返回的错误。
var ErrConnClosed = fmt.Errorf("xfer: connection closed")

// ErrNoTransport 是找不到传输层时返回的错误（内置兜底使用）。
var ErrNoTransport = fmt.Errorf("xfer: no transport registered")
