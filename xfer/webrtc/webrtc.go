// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package xferwebrtc 提供基于 WebRTC DataChannel 的 xfer.Conn 传输层实现。
//
// 使用 pion/webrtc 库，通过 WebRTC DataChannel（SCTP 可靠有序）建立 P2P 连接。
// 在 init() 中自动注册到 xfer 全局注册表，名字为 "webrtc"。
package xferwebrtc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/pion/webrtc/v4"
)

func init() {
	xfer.Register(&xfer.Transport{
		Name:   "webrtc",
		Dial:   Dial,
		Listen: Listen,
	})
}

// webrtcConn 将 WebRTC DataChannel 包装为 xfer.Conn。
type webrtcConn struct {
	dc       *webrtc.DataChannel
	mu       sync.Mutex
	closed   atomic.Bool
	readCh   chan []byte
	closeCh  chan struct{}
}

func newWebrtcConn(dc *webrtc.DataChannel) *webrtcConn {
	c := &webrtcConn{
		dc:      dc,
		readCh:  make(chan []byte, 64),
		closeCh: make(chan struct{}),
	}

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		cp := make([]byte, len(msg.Data))
		copy(cp, msg.Data)
		select {
		case c.readCh <- cp:
		default:
			// 读缓冲区满，丢弃（流控由上层 mux 处理）
		}
	})

	dc.OnClose(func() {
		c.closed.Store(true)
		close(c.closeCh)
	})

	return c
}

// Send 发送一条二进制消息。
func (c *webrtcConn) Send(ctx context.Context, msg []byte) error {
	if c.closed.Load() {
		return xfer.ErrConnClosed
	}
	err := c.dc.Send(msg)
	if err != nil {
		return fmt.Errorf("webrtc send: %w", err)
	}
	return nil
}

// Receive 阻塞接收一条消息。
func (c *webrtcConn) Receive(ctx context.Context) ([]byte, error) {
	select {
	case msg := <-c.readCh:
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closeCh:
		return nil, xfer.ErrConnClosed
	}
}

// Close 关闭 DataChannel。
func (c *webrtcConn) Close() error {
	c.closed.Store(true)
	return c.dc.Close()
}

// webrtcListener 实现 xfer.Listener。
// 通过 WebRTC 信令通道（WebSocket 复用）接受新的 DataChannel 连接。
type webrtcListener struct {
	connCh  chan xfer.Conn
	closeCh chan struct{}
}

// Accept 阻塞等待一个新的 WebRTC DataChannel 连接。
func (l *webrtcListener) Accept(ctx context.Context) (xfer.Conn, error) {
	select {
	case c := <-l.connCh:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closeCh:
		return nil, xfer.ErrConnClosed
	}
}

// Close 关闭监听器。
func (l *webrtcListener) Close() error {
	close(l.closeCh)
	return nil
}

// Dial 创建到远端 WebRTC DataChannel 的连接。
// addr 是信令地址，用于交换 SDP 和 ICE 信息。
// TODO: 实现完整的信令交换流程
func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	_ = addr
	// WebRTC 拨号需要先通过信令通道交换 SDP Offer/Answer 和 ICE Candidate。
	// 当前为占位实现，返回 xfer.ErrConnClosed。
	// 完整实现需要：
	//   1. 创建 PeerConnection
	//   2. 创建 DataChannel
	//   3. 通过信令通道（WebSocket）交换 SDP
	//   4. 等待 DataChannel 打开
	//   5. 包装为 webrtcConn 返回
	return nil, fmt.Errorf("webrtc: dial not yet implemented, use ws transport instead")
}

// Listen 创建 WebRTC 监听器。
// addr 是信令监听地址。
// TODO: 实现完整的信令监听流程
func Listen(ctx context.Context, addr string) (xfer.Listener, error) {
	_ = addr
	// WebRTC 监听需要：
	//   1. 创建 PeerConnection
	//   2. 设置 OnDataChannel 回调
	//   3. 通过信令通道接收 Offer SDP
	//   4. 应答 SDP Answer
	//   5. 等待 DataChannel 打开
	return nil, fmt.Errorf("webrtc: listen not yet implemented, use ws transport instead")
}

// 信令通道常量（待实现）
// 复用现有 WebSocket 信令通道：
//   - Hub 端 WebSocket 升级路径 /ws/signal
//   - SDP 格式：JSON {type: "offer"|"answer"|"candidate", sdp: string, candidate: string}
//   - ICE 服务器：内置 STUN (stun:stun.l.google.com:19302)，可选 TURN