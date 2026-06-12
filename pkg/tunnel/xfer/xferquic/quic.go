// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package xferquic 提供基于 QUIC 的 xfer.Conn 传输层实现。
//
// 使用 quic-go 库，将 QUIC stream 包装为 xfer.Conn 接口。
// 采用 4 字节大端长度前缀帧定界（与 tcp 传输相同）。
// 在 init() 中自动注册到 xfer 全局注册表，名字为 "quic"。
//
// # Windows 兼容性
//
// quic-go 在 Windows 平台使用 UDP 协议。Windows 防火墙、防病毒软件或
// 组策略可能阻止本地 UDP 通信，导致 QUIC 握手超时（DialAddr/Listen 挂起）。
// 这是 quic-go / Windows 环境的已知问题，非本项目代码缺陷。
//
// 参考：
//   - https://github.com/quic-go/quic-go/wiki/UDP-&-Windows
//   - https://github.com/golang/go/issues/49161
//
// 解决方案：
//   - 在 Linux/macOS 运行测试（已验证正常）
//   - Windows 上尝试关闭防火墙或添加 UDP 入站规则
//   - 使用 `go test -run TestQuicRegistration` 仅测试注册逻辑
//
// TODO: 待 quic-go 对 Windows UDP 的兼容性改善后，可移除上述限制。
package xferquic

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/quic-go/quic-go"
)

func init() {
	xfer.Register(&xfer.Transport{
		Name:   "quic",
		Dial:   Dial,
		Listen: Listen,
	})
}

// quicConn 包装 quic.Stream 为 xfer.Conn，使用 4B 大端长度前缀定界。
type quicConn struct {
	stream quic.Stream
	mu     sync.Mutex
	closed bool
}

func (c *quicConn) Send(ctx context.Context, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return xfer.ErrConnClosed
	}
	frame := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(msg)))
	copy(frame[4:], msg)
	_, err := c.stream.Write(frame)
	if err != nil {
		return fmt.Errorf("quic send: %w", err)
	}
	return nil
}

func (c *quicConn) Receive(ctx context.Context) ([]byte, error) {
	if c.closed {
		return nil, xfer.ErrConnClosed
	}
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(c.stream, lenBuf); err != nil {
		return nil, fmt.Errorf("quic recv length: %w", err)
	}
	msgLen := binary.BigEndian.Uint32(lenBuf)
	msg := make([]byte, msgLen)
	if _, err := io.ReadFull(c.stream, msg); err != nil {
		return nil, fmt.Errorf("quic recv body: %w", err)
	}
	return msg, nil
}

func (c *quicConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.stream.Close()
}

// QuicListener 实现 xfer.Listener，基于 quic-go Listener。
type QuicListener struct {
	ln      *quic.Listener
	closeCh chan struct{}
}

func (l *QuicListener) Addr() string {
	return l.ln.Addr().String()
}

func (l *QuicListener) Accept(ctx context.Context) (xfer.Conn, error) {
	type result struct {
		conn xfer.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		qconn, err := l.ln.Accept(ctx)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		stream, err := qconn.AcceptStream(ctx)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{&quicConn{stream: stream}, nil}
	}()
	select {
	case r := <-ch:
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closeCh:
		return nil, xfer.ErrConnClosed
	}
}

func (l *QuicListener) Close() error {
	close(l.closeCh)
	return l.ln.Close()
}

// Dial 建立 QUIC 连接到 addr 并打开双向 stream。
// addr 格式：host:port（如 "127.0.0.1:9000"）。
func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"sproxy-quic"},
	}
	qconn, err := quic.DialAddr(ctx, addr, tlsConf, &quic.Config{
		HandshakeIdleTimeout: 30 * time.Second,
		MaxIdleTimeout:       60 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("quic dial: %w", err)
	}
	stream, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		qconn.CloseWithError(0, "stream failed")
		return nil, fmt.Errorf("quic open stream: %w", err)
	}
	return &quicConn{stream: stream}, nil
}

// Listen 在 addr 启动 QUIC 监听器。
// addr 格式：host:port（如 "127.0.0.1:9000"）。
func Listen(ctx context.Context, addr string) (xfer.Listener, error) {
	cert, err := selfSignedCert()
	if err != nil {
		return nil, fmt.Errorf("quic cert: %w", err)
	}
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"sproxy-quic"},
	}
	ln, err := quic.ListenAddr(addr, tlsConf, &quic.Config{
		HandshakeIdleTimeout: 30 * time.Second,
		MaxIdleTimeout:       60 * time.Second,
		MaxIncomingStreams:   1000,
	})
	if err != nil {
		return nil, fmt.Errorf("quic listen: %w", err)
	}
	return &QuicListener{ln: ln, closeCh: make(chan struct{})}, nil
}

func selfSignedCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"sproxy-quic"}},
		NotBefore:             now,
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	return tls.X509KeyPair(certPEM, keyPEM)
}
