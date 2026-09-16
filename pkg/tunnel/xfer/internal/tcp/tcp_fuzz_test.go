// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/internal/tcp"
)

// FuzzTcpFraming 检查 TCP 传输层的 4B 长度前缀帧定界在任意字节流下不 panic、
// 长度上界受控（超大声明长度不触发巨型分配）。
//
// 威胁模型（tcp.go 注释）：恶意对端可发送超大长度前缀，若无上界校验会触发巨型
// make([]byte, msgLen) 分配（hub 裸 TCP 中继的 DoS 面）。实现已用 maxMessageBytes
// (1 MiB) 拒收并 fail-closed 关闭连接（错误包装 xfer.ErrConnClosed），本 fuzz 钉住
// 该行为，防未来回归：超限声明的错误必须带 ErrConnClosed（fail-closed），而非
// 继续尝试分配/读取。
func FuzzTcpFraming(f *testing.F) {
	// seed corpus：合法帧 + 边界形状。
	f.Add([]byte{0, 0, 0, 0})                          // 空消息
	f.Add([]byte{0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o'}) // 合法 5B 消息
	f.Add([]byte{0, 0, 0, 0xff, 0xff, 0xff, 0xff})     // 超大声明（> 1 MiB，应被拒）
	f.Add([]byte{0, 0, 0, 5, 'h'})                     // 声明 5 只来 1（部分 body）
	f.Add([]byte{0, 0, 1, 0, 'a', 'b'})                // 声明 256 只来 2
	f.Add([]byte{0, 0, 0, 4})                          // 声明 4 但 body 全缺
	f.Add([]byte{0, 0, 0, 3, 'a', 'b', 'c', 'd'})      // 声明 3 有 4（多余字节留给下一帧）

	f.Fuzz(func(t *testing.T, data []byte) {
		// 内存 conn：bytes.Reader 读完即 EOF，等价「对端写完数据后关闭」，
		// 无网络/goroutine 同步开销，fuzz 迭代极快。
		conn := tcp.FromNetConn(&memConn{r: bytes.NewReader(data)})

		msg, err := conn.Receive(context.Background())
		if err == nil {
			// 解析成功：单条消息不得超传输层上限（防御性断言）。
			if len(msg) > 1<<20 {
				t.Fatalf("Receive returned %d bytes, exceeds 1 MiB transport cap", len(msg))
			}
			return
		}
		// 声明长度 > 实际字节数且**未超限**时 body 读不足，Receive 必须返回错误
		// （io.EOF 或 io.ErrUnexpectedEOF，见 io.ReadFull 语义：0 字节读 → EOF，
		// 部分读 → UnexpectedEOF），不得返回部分消息 + nil。
		if len(data) >= 4 && declaredLen(data) > uint32(len(data)-4) &&
			declaredLen(data) <= 1<<20 && !errorsIsReadShort(err) {
			t.Fatalf("declared %d bytes but only %d present: expected read-short error, got %v",
				declaredLen(data), len(data)-4, err)
		}
		// 超限声明（> 1 MiB）必须 fail-closed：错误包装 xfer.ErrConnClosed（连接已关闭），
		// 而不是继续尝试分配/读取超大 body。
		if len(data) >= 4 && declaredLen(data) > 1<<20 && !errors.Is(err, xfer.ErrConnClosed) {
			t.Fatalf("oversized declaration %d bytes: expected ErrConnClosed (fail-closed), got %v",
				declaredLen(data), err)
		}
		// 其它错误（超限拒收、连接关闭、截断）同样可预期；关键是解析不 panic。
	})
}

// declaredLen 读取 4B 大端长度前缀声明的消息长度。
func declaredLen(data []byte) uint32 {
	if len(data) < 4 {
		return 0
	}
	return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
}

// errorsIsReadShort 判断错误是否为 io.EOF / io.ErrUnexpectedEOF（或其包装）。
// io.ReadFull 语义：0 字节读 → io.EOF；部分读 → io.ErrUnexpectedEOF。
func errorsIsReadShort(err error) bool {
	for err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// memConn 是最小 net.Conn 实现：读侧来自 bytes.Reader（读完即 EOF），写侧丢弃。
type memConn struct {
	r *bytes.Reader
}

func (c *memConn) Read(b []byte) (int, error)         { return c.r.Read(b) }
func (c *memConn) Write(b []byte) (int, error)        { return len(b), nil }
func (c *memConn) Close() error                       { return nil }
func (c *memConn) LocalAddr() net.Addr                { return nil }
func (c *memConn) RemoteAddr() net.Addr               { return nil }
func (c *memConn) SetDeadline(t time.Time) error      { return nil }
func (c *memConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *memConn) SetWriteDeadline(t time.Time) error { return nil }
