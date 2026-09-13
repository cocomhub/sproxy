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
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/quic-go/quic-go"
)

// alpnSproxyQuic 是 QUIC 的 ALPN 协议标识符。
const alpnSproxyQuic = "sproxy-quic"

// QUIC 接收窗口。Send 为阻塞写：窗口过小会让单条大消息在接收端尚未读取时
// 长时间阻塞（xfertest.ConnSuite 的 LargePayload 用例单条消息 1 MiB-1）。
// quic-go 默认单流 512 KiB、连接级 768 KiB（1.5×单流），均小于该体量；
// 这里提升到 4 MiB / 8 MiB，且不超过 quic-go 上限（MaxStreamReceiveWindow
// 默认 6 MiB、MaxConnectionReceiveWindow 默认 15 MiB），连接级窗口不小于
// 单流窗口，避免单流被连接级窗口提前限流。
const (
	// streamReceiveWindow 是单流接收窗口。
	streamReceiveWindow = 4 << 20
	// connReceiveWindow 是连接级接收窗口。
	connReceiveWindow = 8 << 20
)

// announceTimeout 是读取对端宣告魔数的兜底超时：宣告帧由 Dial 在建流后立即发送，
// 该时限只用于兜住「连接后不发任何字节」的未认证对端，避免其占住连接槽直到空闲超时。
const announceTimeout = 10 * time.Second

// maxMessageBytes 是单条消息的最大字节数（与 tcp/ws 传输对齐，1 MiB）。
// 与 announceMagic 配合使宣告帧不可与合法帧混淆：合法帧以 4B 大端长度前缀开头，
// 而任何合法帧的长度都 ≤ maxMessageBytes（1 MiB），故以 4B 大端解读 announceMagic
// 得到的长度的帧不可能合法。上限同时防止恶意超大长度前缀触发巨型分配。
const maxMessageBytes = 1 << 20

// announceMagic 是 Dial 建流后立即发送的流宣告魔数。
//
// QUIC 只在对端发送数据后才向本端宣告 stream —— quic-go 的 OpenStreamSync
// 不产生任何线上信令，对端 AcceptStream 会一直阻塞。若服务端在 Accept 内同步
// 等待 stream，就会与"对端先等待 Accept 返回、再发送业务数据"这一正常用法死锁。
// 故 Dial 建流后立即发送本魔数，服务端 Accept 读到并校验通过后才返回。
//
// 使用固定 8 字节魔数而非零长度帧：零长度帧与"合法空消息"在二进制上同形，
// 会把旧实现发出的空首帧静默吞掉。魔数与合法帧不可混淆（见 maxMessageBytes），
// 校验不通过时 Accept 返回明确错误并关闭连接，不静默丢弃数据。
const announceMagic = "SPROXYQ1"

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
	// conn 为底层 QUIC 连接（仅服务端 Accept 时设置）：Close 时一并关闭，
	// 避免连接在流关闭后仍滞留到空闲超时。
	conn connInterface
	// onClose 是 Close 收尾回调（服务端用于从 listener 的活跃连接表移除自身）。
	onClose func()

	// writeMu 保护 Send 的并发写入（4B 头 + 负载需整体写出）。
	writeMu sync.Mutex
	// closed 用原子标记：Receive 在阻塞读前无锁检查、Close/failConn 可在任意时刻
	// 由其他 goroutine 调用，普通 bool + 锁会引入「Receive 无锁读 / Close 有锁写」
	// 的数据竞争（与 tcp 传输同理）。
	closed atomic.Bool
}

// streamInterface 是 quic.Stream 的最小接口，*quic.Stream 和测试 mock 均满足。
// SetReadDeadline 用于把 ctx 的 deadline/取消兑现为读超时（见 withReadDeadline）。
type streamInterface interface {
	io.Reader
	io.Writer
	io.Closer
	SetReadDeadline(t time.Time) error
}

func (c *quicConn) Send(ctx context.Context, msg []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return xfer.ErrConnClosed
	}
	frame := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(msg)))
	copy(frame[4:], msg)
	// **全或无**（xfer.Conn 契约：消息边界由实现保证）：循环写足 + 出错即关连接。
	// 单次 Write 的短写会留下半截帧，后续帧被追加后对端定界永久错位（详见 tcp.go 同处注释）。
	if err := iostream.WriteFull(c.stream, frame); err != nil {
		_ = c.Close()
		return fmt.Errorf("quic send: %w", err)
	}
	return nil
}

// Receive 阻塞接收一条消息：先读 4B 长度前缀，再读消息体。
//
// ctx 通过 stream 的读 deadline 兑现（与 tcp 传输的 SetReadDeadline 语义一致）：
// 有 deadline 时以该 deadline 限读，ctx 取消时由 watcher goroutine 立即解除阻塞；
// 读结束后清理 deadline。读取因 ctx 结束时返回 ctx 的错误。
func (c *quicConn) Receive(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.closed.Load() {
		return nil, xfer.ErrConnClosed
	}
	var msg []byte
	err := withReadDeadline(ctx, c.stream, 0, func() error {
		lenBuf := make([]byte, 4)
		n, rerr := io.ReadFull(c.stream, lenBuf)
		if rerr != nil {
			if n > 0 {
				// 长度前缀被部分消费：流已错位，必须废弃连接（与 tcp 传输一致）。
				c.failConn()
			}
			return fmt.Errorf("quic recv length: %w", rerr)
		}
		msgLen := binary.BigEndian.Uint32(lenBuf)
		if msgLen > maxMessageBytes {
			// 帧协议破坏：废弃连接并返回 ErrConnClosed，避免巨型分配，也避免上层
			// mux readLoop 把已错位的流当瞬时错误重试而读入垃圾帧（同 tcp failConn）。
			c.failConn()
			return fmt.Errorf("quic recv: message too large: %d bytes (max %d): %w", msgLen, maxMessageBytes, xfer.ErrConnClosed)
		}
		body := make([]byte, msgLen)
		if _, rerr := io.ReadFull(c.stream, body); rerr != nil {
			// 长度前缀已被消费，body 读取失败意味着流错位，必须废弃连接。
			c.failConn()
			return fmt.Errorf("quic recv body: %w", rerr)
		}
		msg = body
		return nil
	})
	if err != nil {
		// 读被 ctx 的 deadline/取消中断时，返回 ctx 的错误（对上层更明确）。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return msg, nil
}

// withReadDeadline 在 f 执行期间把 ctx 的 deadline/取消映射为 stream 的读 deadline，
// 使阻塞读能被 ctx 兑现（对齐 tcp 传输的 SetReadDeadline 语义）：
//   - ctx 有 deadline → 直接作为读 deadline；无 deadline 且 fallback > 0 → 用
//     now+fallback 兜底（用于未认证对端的宣告读取，防其不发字节占住连接）；
//   - ctx 可取消（Done() != nil）→ watcher goroutine 在取消时把 deadline 设为当前时刻
//     立即解除阻塞，f 返回后 watcher 退出（不泄漏）；
//   - 结束后把 deadline 复位为零值，避免残留 deadline 影响后续长连接数据面。
func withReadDeadline(ctx context.Context, stream streamInterface, fallback time.Duration, f func() error) error {
	deadline := time.Time{}
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	} else if fallback > 0 {
		deadline = time.Now().Add(fallback)
	}
	if !deadline.IsZero() {
		_ = stream.SetReadDeadline(deadline)
	}
	done := make(chan struct{})
	watcherDone := make(chan struct{})
	if ctx.Done() != nil {
		go func() {
			defer close(watcherDone)
			select {
			case <-ctx.Done():
				_ = stream.SetReadDeadline(time.Now())
			case <-done:
			}
		}()
	} else {
		close(watcherDone)
	}

	err := f()

	close(done)
	// 等 watcher 退出再复位 deadline，避免两者并发写 deadline 的竞态。
	<-watcherDone
	_ = stream.SetReadDeadline(time.Time{})
	return err
}

// failConn 废弃连接（幂等）：帧协议破坏（超长长度前缀、流错位）时调用，
// 关闭 stream 与底层 QUIC 连接，使后续 Send/Receive 立即返回 ErrConnClosed，
// 且阻塞中的读被解除。与 tcp 传输的 failConn 语义一致。
func (c *quicConn) failConn() {
	_ = c.Close()
}

// Close 关闭连接：关闭 stream（发送 FIN）并关闭底层 QUIC 连接（若有），
// 解除任何阻塞中的读/写。可安全多次调用。
func (c *quicConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}

	err := c.stream.Close()
	if c.conn != nil {
		if cerr := c.conn.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if c.onClose != nil {
		c.onClose()
	}
	return err
}

// quicListener 是 QuicListener 所需的底层 QUIC listener 的最小接口。
// 将 *quic.Listener（具体类型）解耦为接口，方便 QuicListener 的 Addr/Close/Accept 单元测试。
type quicListener interface {
	Accept(ctx context.Context) (connInterface, error)
	Addr() net.Addr
	Close() error
}

// connInterface 是 *quic.Conn 的最小接口，用于 Accept 后获取 stream / 关闭连接。
type connInterface interface {
	AcceptStream(ctx context.Context) (streamInterface, error)
	Close() error
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

func (a *quicConnAdapter) Close() error {
	return a.conn.CloseWithError(0, "closed")
}

// QuicListener 实现 xfer.Listener，包装 quicListener。
type QuicListener struct {
	ln      quicListener
	closeCh chan struct{}
	closeMu sync.Once

	// connMu 保护 conns / closed；conns 记录本 listener 已 accept、尚未关闭的连接，
	// Close() 时统一清理，避免连接在监听器关闭后滞留到空闲超时。
	connMu sync.Mutex
	conns  map[*quicConn]struct{}
	closed bool
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
			// 未通过宣告校验的连接一律关闭：不能让未认证对端的连接/流滞留到空闲超时。
			_ = qconn.Close()
			ch <- result{nil, err}
			return
		}
		if err := discardAnnounce(ctx, stream); err != nil {
			_ = stream.Close()
			_ = qconn.Close()
			ch <- result{nil, err}
			return
		}
		qc := &quicConn{stream: stream, conn: qconn}
		qc.onClose = func() { l.untrack(qc) }
		if !l.track(qc) {
			// listener 已关闭：立刻清理该连接（Close 的清理已跑过，不会再有第二次）。
			_ = qc.Close()
			ch <- result{nil, xfer.ErrConnClosed}
			return
		}
		ch <- result{qc, nil}
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

// track 登记已 accept 的连接；listener 已关闭时返回 false（调用方须自行清理）。
func (l *QuicListener) track(c *quicConn) bool {
	l.connMu.Lock()
	defer l.connMu.Unlock()
	if l.closed {
		return false
	}
	if l.conns == nil {
		l.conns = make(map[*quicConn]struct{})
	}
	l.conns[c] = struct{}{}
	return true
}

func (l *QuicListener) untrack(c *quicConn) {
	l.connMu.Lock()
	defer l.connMu.Unlock()
	delete(l.conns, c)
}

// closeAccepted 关闭所有已 accept、尚未关闭的连接（Close 收尾）。
func (l *QuicListener) closeAccepted() {
	l.connMu.Lock()
	l.closed = true
	conns := make([]*quicConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.conns = nil
	l.connMu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
}

// discardAnnounce 读取并校验 Dial 侧发送的流宣告魔数（见 announceMagic）。
// 读取受 ctx 约束（无 deadline 时用 announceTimeout 兜底），避免「连接后不发任何
// 字节」的未认证对端占住连接槽；校验失败返回明确错误（不静默丢弃数据），
// 调用方负责关闭连接与流。
func discardAnnounce(ctx context.Context, stream streamInterface) error {
	buf := make([]byte, len(announceMagic))
	err := withReadDeadline(ctx, stream, announceTimeout, func() error {
		if _, rerr := io.ReadFull(stream, buf); rerr != nil {
			return fmt.Errorf("quic announce: %w", rerr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if string(buf) != announceMagic {
		return fmt.Errorf("quic announce: 非法的宣告魔数 %x", buf)
	}
	return nil
}

// Close 关闭监听器：除关闭底层 listener 外，还关闭所有已 accept 的连接，
// 不残留到空闲超时。可安全多次调用。
func (l *QuicListener) Close() error {
	l.closeMu.Do(func() {
		close(l.closeCh)
	})
	err := l.ln.Close()
	l.closeAccepted()
	return err
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
	// 立即宣告 stream（见 announceMagic），否则服务端 Accept 会阻塞到首次业务发送。
	if _, err := stream.Write([]byte(announceMagic)); err != nil {
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
