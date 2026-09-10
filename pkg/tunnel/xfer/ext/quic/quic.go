// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package quic 提供基于 QUIC 的 xfer.Conn 传输层实现。
//
// 使用 quic-go 库，将 QUIC stream 包装为 xfer.Conn 接口。
// 采用 4 字节大端长度前缀帧定界（与 tcp 传输相同）。
// 在 init() 中自动注册到 xfer.TransportRegistry，名字为 "quic"。
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
// 测试建议：
//   - 在 Linux/macOS 运行测试（已验证正常）
//   - Windows 上尝试关闭防火墙或添加 UDP 入站规则
//   - 使用 `go test -run TestQuicRegistration` 测试注册逻辑
//
// Note: 待 quic-go 对 Windows UDP 的兼容性改善后，可移除本限制 (https://github.com/quic-go/quic-go/wiki/UDP-&-Windows)。 //nolint:godox
package quic

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/quic-go/quic-go"
)

// alpnSproxyQuic 是 QUIC 的 ALPN 协议标识符。
const alpnSproxyQuic = "sproxy-quic"

// QUIC 接收窗口。Send 为阻塞写：窗口过小（quic-go 默认单流 512 KiB、
// 连接级 768 KiB）时，单条大消息会在接收端尚未读取时长时间阻塞。
// 提高到下列值以匹配隧道单条消息（mux 帧）的常见体量。
const (
	// streamReceiveWindow 是单流接收窗口。
	streamReceiveWindow = 4 << 20
	// connReceiveWindow 是连接级接收窗口，不小于单流窗口，
	// 否则单流会被连接级窗口提前限流。
	connReceiveWindow = 8 << 20
)

// announceFrame 是 Dial 打开 stream 后立即发送的流宣告帧（4B 大端零长度）。
//
// QUIC 只在对端发送数据后才向本端宣告 stream —— quic-go 的 OpenStreamSync
// 不产生任何线上信令，对端 AcceptStream 会一直阻塞。若服务端在 Accept 内同步
// 等待 stream，就会与"对端先等待 Accept 返回、再发送业务数据"这一正常用法死锁。
// 故 Dial 建流后立即发送一个零长度宣告帧，服务端 Accept 读到并校验后即返回，
// 业务消息流不受影响。
var announceFrame = []byte{0, 0, 0, 0}

func init() {
	xfer.Register(&xfer.Transport{
		Name:   "quic",
		Dial:   Dial,
		Listen: Listen,
	})
}

// quicConn 包装 quic.Stream 为 xfer.Conn，使用 4B 大端长度前缀帧。
type quicConn struct {
	stream streamInterface
	mu     sync.Mutex
	closed bool
}

// streamInterface 是 quic.Stream 的最小接口，*quic.Stream 和测试 mock 均满足。
type streamInterface interface {
	io.Reader
	io.Writer
	io.Closer
}

func (c *quicConn) Send(ctx context.Context, msg []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

// quicListener 是 QuicListener 所需的底层 QUIC listener 的最小接口。
// 将 *quic.Listener（具体类型）解耦为接口，方便 QuicListener 的 Addr/Close/Accept 单元测试。
type quicListener interface {
	Accept(ctx context.Context) (connInterface, error)
	Addr() net.Addr
	Close() error
}

// connInterface 是 *quic.Conn 的最小接口，用于 Accept 后获取 stream。
type connInterface interface {
	AcceptStream(ctx context.Context) (streamInterface, error)
}

// quicListenerAdapter 适配 *quic.Listener 到 quicListener 接口。
type quicListenerAdapter struct {
	ln *quic.Listener
}

func (a *quicListenerAdapter) Accept(ctx context.Context) (connInterface, error) {
	conn, err := a.ln.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return &quicConnAdapter{conn: conn}, nil
}

func (a *quicListenerAdapter) Addr() net.Addr { return a.ln.Addr() }

func (a *quicListenerAdapter) Close() error { return a.ln.Close() }

// quicConnAdapter 适配 *quic.Conn 到 connInterface 接口。
type quicConnAdapter struct {
	conn *quic.Conn
}

func (a *quicConnAdapter) AcceptStream(ctx context.Context) (streamInterface, error) {
	stream, err := a.conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// QuicListener 实现 xfer.Listener，包装 quicListener。
type QuicListener struct {
	ln      quicListener
	closeCh chan struct{}
	closeMu sync.Once
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
		if err := discardAnnounce(stream); err != nil {
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

// discardAnnounce 读取并校验 Dial 侧发送的流宣告帧（见 announceFrame）。
func discardAnnounce(stream streamInterface) error {
	buf := make([]byte, len(announceFrame))
	if _, err := io.ReadFull(stream, buf); err != nil {
		return fmt.Errorf("quic announce: %w", err)
	}
	if !bytes.Equal(buf, announceFrame) {
		return fmt.Errorf("quic announce: 非法的宣告帧 %x", buf)
	}
	return nil
}

func (l *QuicListener) Close() error {
	l.closeMu.Do(func() {
		close(l.closeCh)
	})
	return l.ln.Close()
}

// DialTLSConfig 根据 addr 构建 TLS 配置，供 Dial 使用。
// 从 addr 提取 host 设置 ServerName，支持通过 SPROXY_QUIC_CA_CERT
// 环境变量指定 CA 证书文件路径验证服务端证书。
// 未设置环境变量时使用系统默认 CA 池。
func DialTLSConfig(addr string) (*tls.Config, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("quic dial: %w", err)
	}
	tlsConf := &tls.Config{
		ServerName: host,
		NextProtos: []string{alpnSproxyQuic},
	}
	if caPath := os.Getenv("SPROXY_QUIC_CA_CERT"); caPath != "" {
		caCert, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("quic dial: CA 证书读取失败: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("quic dial: 无效的 CA 证书")
		}
		tlsConf.RootCAs = pool
	}
	return tlsConf, nil
}

// Dial 建立 QUIC 连接到 addr 并打开双向 stream。
// addr 格式：host:port（如 "127.0.0.1:9000"）。
func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	tlsConf, err := DialTLSConfig(addr)
	if err != nil {
		return nil, err
	}
	qconn, err := quic.DialAddr(ctx, addr, tlsConf, &quic.Config{
		HandshakeIdleTimeout:           30 * time.Second,
		MaxIdleTimeout:                 60 * time.Second,
		InitialStreamReceiveWindow:     streamReceiveWindow,
		InitialConnectionReceiveWindow: connReceiveWindow,
	})
	if err != nil {
		return nil, fmt.Errorf("quic dial: %w", err)
	}
	stream, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		_ = qconn.CloseWithError(0, "stream failed")
		return nil, fmt.Errorf("quic open stream: %w", err)
	}
	// 立即宣告 stream（见 announceFrame），否则服务端 Accept 会阻塞到首次业务发送。
	if _, err := stream.Write(announceFrame); err != nil {
		_ = qconn.CloseWithError(0, "announce failed")
		return nil, fmt.Errorf("quic announce: %w", err)
	}
	return &quicConn{stream: stream}, nil
}

// Listen 在 addr 启用 QUIC 监听器。
// addr 格式：host:port（如 "127.0.0.1:9000"）。
//
// 监听证书按环境变量选择：
//   - 同时设置 SPROXY_QUIC_CERT_FILE 与 SPROXY_QUIC_KEY_FILE → 用 tls.LoadX509KeyPair 加载；
//   - 两者都未设置 → 回落开发用临时自签证书（selfSignedCert）；
//   - 只设置其一 → 返回错误（fail-closed，不静默回落）。
//
// 生产环境应显式配置证书，并通过 Dial 侧同源的 SPROXY_QUIC_CA_CERT
// （或系统 CA 池）完成真实校验。
func Listen(ctx context.Context, addr string) (xfer.Listener, error) {
	cert, err := listenCert()
	if err != nil {
		return nil, fmt.Errorf("quic cert: %w", err)
	}
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{alpnSproxyQuic},
	}
	ln, err := quic.ListenAddr(addr, tlsConf, &quic.Config{
		HandshakeIdleTimeout:           30 * time.Second,
		MaxIdleTimeout:                 60 * time.Second,
		MaxIncomingStreams:             1000,
		InitialStreamReceiveWindow:     streamReceiveWindow,
		InitialConnectionReceiveWindow: connReceiveWindow,
	})
	if err != nil {
		return nil, fmt.Errorf("quic listen: %w", err)
	}
	return &QuicListener{ln: &quicListenerAdapter{ln: ln}, closeCh: make(chan struct{})}, nil
}

// listenCert 按 SPROXY_QUIC_CERT_FILE / SPROXY_QUIC_KEY_FILE 选择监听证书。
// 两者都未设置时回落开发用临时自签证书；只设置其一时返回错误（fail-closed）。
func listenCert() (tls.Certificate, error) {
	certFile := os.Getenv("SPROXY_QUIC_CERT_FILE")
	keyFile := os.Getenv("SPROXY_QUIC_KEY_FILE")
	switch {
	case certFile == "" && keyFile == "":
		return selfSignedCert()
	case certFile == "" || keyFile == "":
		return tls.Certificate{}, errors.New("SPROXY_QUIC_CERT_FILE 与 SPROXY_QUIC_KEY_FILE 必须同时设置")
	default:
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("加载证书/私钥失败: %w", err)
		}
		return cert, nil
	}
}

// selfSignedCert 生成开发用临时自签证书。
// 证书带 localhost / 127.0.0.1 / ::1 SAN，并具备 CA 属性（IsCA + KeyUsageCertSign），
// 以便在被显式 pin 为信任根时同样能通过校验。
func selfSignedCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{alpnSproxyQuic}},
		NotBefore:             now,
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		IsCA:                  true,
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
