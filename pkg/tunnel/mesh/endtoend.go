// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// EndToEndOptions 配置端到端加密（与 SK 解耦：密钥 = 端到端身份派生，中间节点 X
// 即使持有集群 SK 也无法派生会话密钥——X 只透传密文，读不到明文）。
type EndToEndOptions struct {
	// Enabled 启用端到端加密。
	Enabled bool
	// Identity 是本端长时身份（Ed25519 密钥对）。握手时向对端证明持有私钥
	// （proof of possession），并参与 ECDH 会话密钥派生。
	Identity *tunnel.Identity
	// PeerFingerprints 是对端身份指纹 pinning 白名单（"sha256:<64hex>"）。
	// 非空时握手 fail-closed 校验对端指纹，不匹配或对端无身份即拒绝——
	// 与 remote_read_listener 的"无 pin 拒绝"一致，绝不回退静态密钥。
	// 任一元素为空字符串 → 校验失败（fail-closed，防 DeriveRemoteStaticKey panic）。
	PeerFingerprints []string
	// Handler 是 T 侧（ServeE2EListener 的 listener）处理解密后 HTTP 请求的处理器。
	// 端到端"数据面"= 隧道 HTTP 请求-响应交换（复用 tunnel.Tunnel 语义）。
	// 为空时 ServeE2EListener 回显请求体（echo，测试/诊断）。
	Handler http.Handler
	// DialAddr 是 L 侧（DialE2E）在外层连接首部写入的 dial 指令目标地址
	// （[4B len][{"dial":"addr"}]，复用 hub.DialRequest 帧语义）。非空时 DialE2E
	// 先写 dial 指令再建隧道——X 侧 ServeE2ERelay 据此出口拨号到 T（多跳
	// via-direct 形态）。空 = 不写（直连 T 场景，T 侧直接 ServeE2EListener）。
	DialAddr string
	// HandshakeTimeout 覆写隧道握手超时（0 = 默认 30s）。
	HandshakeTimeout time.Duration
}

// DialE2E 是 L 侧端到端加密拨号：在外层数据面连接 outer 之上建隧道（dialer
// 角色），返回 E2EConn。对端（T）身份须在 opts.PeerFingerprints 白名单中，
// 否则握手 fail-closed 失败（不回退静态密钥）。
//
// 安全语义：会话密钥 = ECDH(L身份私钥, T身份公钥)（tunnel.Tunnel 的 C-1 握手，
// 静态密钥参与派生）。X（外层数据面的中间节点）只透传下层 mux 流密文字节，
// 无 L/T 私钥无法派生会话密钥——X 即使持有集群 SK 也读不到明文。
func DialE2E(ctx context.Context, outer net.Conn, opts EndToEndOptions) (*E2EConn, error) {
	if !opts.Enabled {
		return nil, fmt.Errorf("endtoend: EndToEndOptions.Enabled 必须为 true")
	}
	if err := validatePeerFingerprints(opts.PeerFingerprints); err != nil {
		return nil, err
	}
	if outer == nil {
		return nil, fmt.Errorf("endtoend: 外层数据面连接为空")
	}
	if opts.Identity == nil {
		return nil, fmt.Errorf("endtoend: 缺少本端身份（Identity 必填）")
	}
	// 多跳（via-direct）形态：先写 dial 指令，X 侧 ServeE2ERelay 据此出口拨号到 T。
	if opts.DialAddr != "" {
		if err := writeE2EDialFrame(outer, opts.DialAddr); err != nil {
			return nil, fmt.Errorf("endtoend: 写 dial 指令失败: %w", err)
		}
	}
	// 静态密钥由**对端指纹**派生（与 pkg/remote 的 dialer 侧一致）：对端（listener
	// 侧）用自己指纹派生同一值，dialer 侧用对端指纹派生——远端只读面已验证此
	// 约定（pkg/remote/client.go:255 DeriveRemoteStaticKey(pins[0])）。
	staticKey := tunnel.DeriveRemoteStaticKey(opts.PeerFingerprints[0])
	// 把外层 net.Conn 包装为 xfer.Conn（mux 的载体）。
	xc := xferFromNetConn(outer)
	m := mux.New(xc, mux.RoleDialer)
	tunOpts := []tunnel.TunnelOption{
		tunnel.WithIdentity(opts.Identity),
		tunnel.WithPeerFingerprints(opts.PeerFingerprints),
	}
	if opts.HandshakeTimeout > 0 {
		tunOpts = append(tunOpts, tunnel.WithHandshakeTimeout(opts.HandshakeTimeout))
	}
	tun := tunnel.NewTunnel(m, staticKey, tunOpts...)
	return &E2EConn{tun: tun, mux: m, outer: outer}, nil
}

// ServeE2EListener 是 T 侧（listener 角色）端到端加密接受：在外层数据面连接
// outer 上建隧道（listener 角色），进入 accept 循环前同步握手（fail-closed：
// 对端指纹不在白名单、或无身份、或不知静态密钥 → 返回错误，绝不回退）。
//
// 安全语义：与 DialE2E 对称。调用方（T）配置自己的 Identity + 白名单
// （PeerFingerprints = 允许的 L 指纹），X 只透传密文。
func ServeE2EListener(ctx context.Context, outer net.Conn, opts EndToEndOptions) error {
	if !opts.Enabled {
		return fmt.Errorf("endtoend: EndToEndOptions.Enabled 必须为 true")
	}
	if err := validatePeerFingerprints(opts.PeerFingerprints); err != nil {
		return err
	}
	if outer == nil {
		return fmt.Errorf("endtoend: 外层数据面连接为空")
	}
	if opts.Identity == nil {
		return fmt.Errorf("endtoend: 缺少本端身份（Identity 必填）")
	}
	handler := opts.Handler
	if handler == nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		})
	}
	// 静态密钥由**本端指纹**派生（与 pkg/server/remote_read_listener.go:96 一致：
	// listener 用自己指纹派生）。dialer 侧用对端指纹（PeerFingerprints[0]）派生
	// 同一值——两端约定一致才能握手成功。
	staticKey := tunnel.DeriveRemoteStaticKey(opts.Identity.Fingerprint())
	xc := xferFromNetConn(outer)
	m := mux.New(xc, mux.RoleListener)
	tunOpts := []tunnel.TunnelOption{
		tunnel.WithIdentity(opts.Identity),
		tunnel.WithPeerFingerprints(opts.PeerFingerprints),
	}
	if opts.HandshakeTimeout > 0 {
		tunOpts = append(tunOpts, tunnel.WithHandshakeTimeout(opts.HandshakeTimeout))
	}
	tun := tunnel.NewTunnel(m, staticKey, tunOpts...)
	return tun.Serve(ctx, handler)
}

// validatePeerFingerprints 校验对端指纹白名单：非空 + 每个元素非空字符串。
// fail-closed：空元素会让 DeriveRemoteStaticKey("") panic，此处改为返回错误。
func validatePeerFingerprints(fps []string) error {
	if len(fps) == 0 {
		return fmt.Errorf("endtoend: 缺少对端指纹 pin（PeerFingerprints 必填，fail-closed）")
	}
	for _, fp := range fps {
		if strings.TrimSpace(fp) == "" {
			return fmt.Errorf("endtoend: 对端指纹白名单含空元素（fail-closed，拒绝启动）")
		}
	}
	return nil
}

// E2EConn 是 L 侧端到端加密连接的对外视图：在隧道之上提供 HTTP 请求-响应交换
// （与 tunnel.Tunnel 语义一致；Do 触发首次握手）。
type E2EConn struct {
	tun   *tunnel.Tunnel
	mux   *mux.Mux
	outer net.Conn
}

// Do 发送一次 HTTP 请求并经隧道读回响应（首次调用触发 ECDH 握手，fail-closed）。
func (c *E2EConn) Do(ctx context.Context, method, urlPath, body string) (string, error) {
	if c == nil || c.tun == nil {
		return "", fmt.Errorf("endtoend: 隧道未初始化")
	}
	req, err := http.NewRequestWithContext(ctx, method, urlPath, strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("endtoend: 构造请求失败: %w", err)
	}
	resp, err := c.tun.Do(req)
	if err != nil {
		return "", fmt.Errorf("endtoend: 隧道请求失败: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("endtoend: 读响应失败: %w", err)
	}
	return string(out), nil
}

// Close 关闭隧道与底层连接。
func (c *E2EConn) Close() error {
	if c == nil {
		return nil
	}
	if c.mux != nil {
		_ = c.mux.Close()
	}
	if c.outer != nil {
		return c.outer.Close()
	}
	return nil
}

// ServeE2ERelay 是 X 侧密文中继：在中间节点 X 上把来自 L 的外层连接（端到端
// 隧道密文，mux 帧层字节）原样透传到 X→T 出口连接。X **不建隧道、不解密**——
// 纯字节 pump，L/T 的 ECDH 会话密钥 X 无法派生，X 只见密文。
//
// 链路：L ⇄(外层数据面连接，DialE2E 写入 dial 帧 + mux 流)⇄ X ⇄(出口拨号 TCP)⇄ T。
// 首帧 = [4B len][{"dial":"addr"}]（DialRequest，复用 hub 中继帧语义）；X 裸读该
// 帧后按 dialPolicy 校验/解析目标地址，net.DialTimeout 建立 X→T 出口连接，然后
// iostream.Pump 双向泵送外层连接剩余密文字节。dialPolicy 拒绝 → 返回错误
// （不拨号、不泵）。
//
// dialPolicy 语义：func(addr) (resolved, ok)——ok=false 拒绝（不拨号）；
// ok=true 且 resolved 非空时用 resolved 拨号（调用方应解析主机名为 IP:port，
// 防 DNS rebinding TOCTOU）；ok=true 且 resolved 为空时回退原始 addr 拨号。
// dialPolicy == nil → fail-closed 返回错误（不 panic）。
//
// 返回时：出口拨号失败返回错误；泵送自然结束返回 nil；ctx 取消返回 ctx.Err()。
func ServeE2ERelay(ctx context.Context, outer net.Conn, dialPolicy func(addr string) (string, bool)) error {
	if outer == nil {
		return fmt.Errorf("endtoend: X 侧外层连接为空")
	}
	if dialPolicy == nil {
		return fmt.Errorf("endtoend: X 侧出口拨号策略为空（fail-closed）")
	}
	// 读首帧：dial 指令（与 relay/leaf.go 的 dOK 分支同语义）。
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(outer, lenBuf); err != nil {
		return fmt.Errorf("endtoend: 读 dial 帧长度失败: %w", err)
	}
	metaLen := binary.BigEndian.Uint32(lenBuf)
	if metaLen == 0 || metaLen > maxDialFrameBytes {
		return fmt.Errorf("endtoend: 非法 dial 帧长度 %d", metaLen)
	}
	meta := make([]byte, metaLen)
	if _, err := io.ReadFull(outer, meta); err != nil {
		return fmt.Errorf("endtoend: 读 dial 帧失败: %w", err)
	}
	var d hub.DialRequest
	if err := json.Unmarshal(meta, &d); err != nil || d.Dial == "" {
		return fmt.Errorf("endtoend: 非法 dial 帧（需 {\"dial\":\"addr\"}）")
	}
	// dialPolicy 校验 + 解析（返回实际应拨地址，防 DNS rebinding TOCTOU）。
	resolved, ok := dialPolicy(d.Dial)
	if !ok {
		return fmt.Errorf("endtoend: 出口拨号目标未通过拨号策略: %s", d.Dial)
	}
	dialAddr := resolved
	if dialAddr == "" {
		// 回退语义（文档化）：dialPolicy 返回 ("", true) 时回退原始目标地址。
		// 调用方应在 dialPolicy 内完成主机名解析（返回解析后的 IP:port）——
		// 这里仅在调用方选择不解析时回退，防 DNS rebinding 依赖调用方。
		dialAddr = d.Dial
	}
	remote, err := net.DialTimeout("tcp", dialAddr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("endtoend: X 出口拨号失败: %w", err)
	}
	defer remote.Close()
	// 纯字节泵送（密文透传，X 不接触明文）：外层连接剩余字节（mux 帧层）⇄ 出口。
	// 关闭语义由 iostream.Pump 半关闭处理。
	iostream.Pump(outer, remote, iostream.PumpGrace)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return nil
}

// maxDialFrameBytes 是 X 侧 dial 帧长度上限（对齐 tunnel.MaxMetadataBytes 语义，
// 防恶意超大长度前缀触发巨型分配）。
const maxDialFrameBytes = 1 << 20

// e2eStreamAAD 是字节流形态加密帧的 GCM 额外认证数据（域分离：与 tunnel.Tunnel
// 的 HTTP 隧道帧区分，防跨协议重放）。
const e2eStreamAAD = "mesh-e2e-stream-v1"

// validatePeerFingerprintsOptional 是字节流形态的 pin 校验（区别于 HTTP 形态的
// validatePeerFingerprints——后者要求 pin 必填 fail-closed）：空列表放行（纯 ECDH，
// 防窃听不防 MITM，由显式 pinning 补足）；非空时每个元素必须非空（防
// DeriveRemoteStaticKey("") panic，fail-closed）。
func validatePeerFingerprintsOptional(fps []string) error {
	for _, fp := range fps {
		if strings.TrimSpace(fp) == "" {
			return fmt.Errorf("endtoend: 对端指纹白名单含空元素（fail-closed，拒绝启动）")
		}
	}
	return nil
}

// DialE2EStream 是 L 侧端到端加密**字节流**拨号：在外层数据面连接 outer 上
// 写 e2e dial 帧（[4B len][{"dial":addr,"e2e":true}]），执行 ECDH 握手，返回
// 加密的 net.Conn（AES-256-GCM 字节流）。
//
// 与 HTTP 隧道形态（DialE2E）的区别：不复用 mux/tunnel.Tunnel（避免 mux-over-mux），
// 直接在外层连接上做握手 + 分块加密，返回的 net.Conn 承载任意字节流（mesh connect
// 服务访问、socks/http-proxy 出口等）。对端（T）身份须在 opts.PeerFingerprints
// 白名单中，否则握手 fail-closed（不回退）。Identity 可选：nil 时纯 ECDH
// （staticKey nil，防窃听不防 MITM——由显式 pinning 补足）。
//
// 安全语义：会话密钥 = ECDH(L私钥, T公钥)（X25519 临时密钥 + HKDF），staticKey
// （Identity + pin 非空时由身份指纹派生）参与派生（C-1 静态绑定）。X/hub 只透传
// 密文，无 L/T 私钥无法派生会话密钥——即使持有集群 SK 也读不到明文（与 SK 解耦）。
func DialE2EStream(ctx context.Context, outer net.Conn, addr string, opts EndToEndOptions) (net.Conn, error) {
	if !opts.Enabled {
		return nil, fmt.Errorf("endtoend: EndToEndOptions.Enabled 必须为 true")
	}
	if err := validatePeerFingerprintsOptional(opts.PeerFingerprints); err != nil {
		return nil, err
	}
	if outer == nil {
		return nil, fmt.Errorf("endtoend: 外层数据面连接为空")
	}
	// 写 e2e dial 帧（[4B len][{"dial":addr,"e2e":true}]）。
	head, err := json.Marshal(hub.DialRequest{Dial: addr, E2E: true})
	if err != nil {
		return nil, fmt.Errorf("endtoend: 序列化 e2e dial 帧失败: %w", err)
	}
	if werr := writeLenFrame(outer, head); werr != nil {
		return nil, fmt.Errorf("endtoend: 写 e2e dial 帧失败: %w", werr)
	}
	// 静态密钥由对端指纹派生（pin 非空时）；纯 ECDH（无 pin/无身份）时 nil。
	var staticKey []byte
	if len(opts.PeerFingerprints) > 0 {
		staticKey = tunnel.DeriveRemoteStaticKey(opts.PeerFingerprints[0])
	}
	hctx, hcancel := handshakeCtx(ctx, opts.HandshakeTimeout)
	defer hcancel()
	sessionKey, _, err := tunnel.PerformHandshakeConn(hctx, outer, true, opts.Identity, opts.PeerFingerprints, staticKey)
	if err != nil {
		return nil, fmt.Errorf("endtoend: ECDH 握手失败: %w", err)
	}
	sc, err := newE2EStreamConn(outer, sessionKey)
	if err != nil {
		return nil, err
	}
	return sc, nil
}

// ServeE2EStream 是 T 侧端到端加密**字节流**接受：读 e2e dial 帧（校验 e2e:true），
// 执行 ECDH 握手，返回解密后的 net.Conn（调用方 pump 到本地服务）。
//
// 与 DialE2EStream 对称：本端（T）配置自己的 Identity + 白名单（PeerFingerprints =
// 允许的 L 指纹）。非 e2e dial 帧（无 e2e:true）→ fail-closed 报错（不当作普通流）。
func ServeE2EStream(ctx context.Context, outer net.Conn, opts EndToEndOptions) (net.Conn, error) {
	if !opts.Enabled {
		return nil, fmt.Errorf("endtoend: EndToEndOptions.Enabled 必须为 true")
	}
	if err := validatePeerFingerprintsOptional(opts.PeerFingerprints); err != nil {
		return nil, err
	}
	if outer == nil {
		return nil, fmt.Errorf("endtoend: 外层数据面连接为空")
	}
	// 读 e2e dial 帧（校验 e2e:true——fail-closed，非 e2e 帧拒绝）。
	head, err := readLenFrame(outer, maxDialFrameBytes)
	if err != nil {
		return nil, fmt.Errorf("endtoend: 读 e2e dial 帧失败: %w", err)
	}
	var d hub.DialRequest
	if uerr := json.Unmarshal(head, &d); uerr != nil || d.Dial == "" {
		return nil, fmt.Errorf("endtoend: 非法 e2e dial 帧（需 {\"dial\":addr,\"e2e\":true}）")
	}
	if !d.E2E {
		return nil, fmt.Errorf("endtoend: 非 e2e dial 帧（缺少 e2e:true 标记），拒绝按加密流处理")
	}
	// 静态密钥由本端指纹派生（Identity 非空时）；纯 ECDH 时 nil。
	var staticKey []byte
	if opts.Identity != nil {
		staticKey = tunnel.DeriveRemoteStaticKey(opts.Identity.Fingerprint())
	}
	hctx, hcancel := handshakeCtx(ctx, opts.HandshakeTimeout)
	defer hcancel()
	sessionKey, _, err := tunnel.PerformHandshakeConn(hctx, outer, false, opts.Identity, opts.PeerFingerprints, staticKey)
	if err != nil {
		return nil, fmt.Errorf("endtoend: ECDH 握手失败: %w", err)
	}
	sc, err := newE2EStreamConn(outer, sessionKey)
	if err != nil {
		return nil, err
	}
	return sc, nil
}

// handshakeCtx 为 E2E 握手构造带超时的 ctx（0 = 调用方 ctx 原样，不额外设超时）。
func handshakeCtx(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// writeLenFrame 在 w 上写 [4B 大端长度][payload] 帧。
func writeLenFrame(w io.Writer, payload []byte) error {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(payload)))
	for _, chunk := range [][]byte{lenBuf, payload} {
		if err := iostream.WriteFull(w, chunk); err != nil {
			return err
		}
	}
	return nil
}

// readLenFrame 从 r 读 [4B 大端长度][payload] 帧，payload 长度上限 maxLen。
func readLenFrame(r io.Reader, maxLen uint32) ([]byte, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf)
	if n == 0 || n > maxLen {
		return nil, fmt.Errorf("非法帧长度 %d（上限 %d）", n, maxLen)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// e2eStreamConn 是端到端加密字节流连接：外层 net.Conn 之上做 AES-256-GCM 分块
// 加解密（复用 tunnel.StreamEncryptor/StreamDecryptor 的单帧 API，帧格式
// [4B 密文长度][nonce|密文|tag]）。实现 net.Conn（LocalAddr/RemoteAddr/Deadline
// 透传外层；Read/Write 加解密）。
//
// 并发安全：Read 与 Write 各自独立串行化（readMu/writeMu），允许单读单写并发
// （与 net.Conn 常见语义一致）；同一方向多次调用由锁串行。
type e2eStreamConn struct {
	outer net.Conn
	enc   *tunnel.StreamEncryptor
	dec   *tunnel.StreamDecryptor

	readMu  sync.Mutex
	readBuf bytes.Buffer // 解密后待读明文（单帧缓存）
	writeMu sync.Mutex
}

func newE2EStreamConn(outer net.Conn, sessionKey []byte) (*e2eStreamConn, error) {
	enc, err := tunnel.NewStreamEncryptor(sessionKey, tunnel.DefaultChunkSize)
	if err != nil {
		return nil, fmt.Errorf("endtoend: 创建加密器失败: %w", err)
	}
	dec, err := tunnel.NewStreamDecryptor(sessionKey, tunnel.DefaultChunkSize+64)
	if err != nil {
		return nil, fmt.Errorf("endtoend: 创建解密器失败: %w", err)
	}
	return &e2eStreamConn{outer: outer, enc: enc, dec: dec}, nil
}

// Read 读取解密后的明文。首次调用在内部缓冲解出的一帧明文（最多 DefaultChunkSize），
// 逐次返回；缓冲耗尽时读下一帧。对端关闭 → EOF。
func (c *e2eStreamConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.readBuf.Len() > 0 {
		return c.readBuf.Read(p)
	}
	// 解出一帧明文到内部缓冲。
	var nw int
	var err error
	for {
		nw, err = c.dec.DecryptChunk(c.outer, &c.readBuf, []byte(e2eStreamAAD))
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, io.EOF
			}
			return 0, err
		}
		if nw > 0 {
			break
		}
	}
	return c.readBuf.Read(p)
}

// Write 把明文加密为一帧写入外层连接（与 tunnel.Tunnel 密文帧格式一致：
// [4B 密文长度][nonce|密文|tag]）。每次 Write 独立成帧（块大小 = len(p)）。
func (c *e2eStreamConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.enc.EncryptChunk(p, c.outer, []byte(e2eStreamAAD)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close 关闭外层连接（释放底层资源）。
func (c *e2eStreamConn) Close() error {
	if c == nil || c.outer == nil {
		return nil
	}
	return c.outer.Close()
}

// LocalAddr 透传外层连接地址。
func (c *e2eStreamConn) LocalAddr() net.Addr                { return c.outer.LocalAddr() }
func (c *e2eStreamConn) RemoteAddr() net.Addr               { return c.outer.RemoteAddr() }
func (c *e2eStreamConn) SetDeadline(t time.Time) error      { return c.outer.SetDeadline(t) }
func (c *e2eStreamConn) SetReadDeadline(t time.Time) error  { return c.outer.SetReadDeadline(t) }
func (c *e2eStreamConn) SetWriteDeadline(t time.Time) error { return c.outer.SetWriteDeadline(t) }

// writeE2EDialFrame 在 L 侧外层连接首部写 dial 指令帧
// （[4B len][{"dial":"addr"}]，复用 hub.DialRequest 语义）。
// X 侧 ServeE2ERelay 先读该帧再出口拨号，与 hub 中继的 relay_stream 写帧同构。
func writeE2EDialFrame(outer net.Conn, addr string) error {
	b, err := json.Marshal(hub.DialRequest{Dial: addr})
	if err != nil {
		return err
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(b)))
	for _, chunk := range [][]byte{lenBuf, b} {
		if err := iostream.WriteFull(outer, chunk); err != nil {
			return err
		}
	}
	return nil
}

// xferFromNetConn 把 net.Conn 包装为 xfer.Conn（mux 载体）。
// 复用内置 TCP 传输的 FromNetConn：4B 长度前缀帧定界 + 写超时兜底 + 读上限，
// 语义与 mesh 既有 webrtc/hub 中继数据面一致（Y 一期 AD-6 同款适配）。
var xferFromNetConn = builtin.FromNetConn

// ServeE2ERelayStream 是 X 侧端到端加密字节流的中继（二期 via-node 形态）：
// 读 e2e dial 帧（[4B len][{"dial":addr,"e2e":true}]），按 dialPolicy 出口拨号，
// 然后**把 e2e dial 帧原样写回出口连接**（T 侧 ServeE2EStream 据此 DetectE2E），
// 再双向泵送剩余密文字节。X 不建隧道、不解密——纯字节泵，T 与 L 之间的
// ECDH 握手/加密完全透传（X 即使持有 SK 也读不到明文）。
//
// 与 HTTP 形态 ServeE2ERelay 的区别：后者消费 dial 帧（X 是目标，T 在其后
// 建 mux 隧道）；本函数透传 dial 帧（X 是纯中转，T 需显式 e2e 标记才能
// DetectE2E——字节流形态无 mux 协议头，T 无法从裸字节自行识别 E2E）。
func ServeE2ERelayStream(ctx context.Context, outer net.Conn, dialPolicy func(addr string) (string, bool)) error {
	if outer == nil {
		return fmt.Errorf("endtoend: X 侧外层连接为空")
	}
	if dialPolicy == nil {
		return fmt.Errorf("endtoend: X 侧出口拨号策略为空（fail-closed）")
	}
	// 读 e2e dial 帧（[4B len][{"dial":addr,"e2e":true}]）。
	head, err := readLenFrame(outer, maxDialFrameBytes)
	if err != nil {
		return fmt.Errorf("endtoend: 读 dial 帧失败: %w", err)
	}
	var d hub.DialRequest
	if uerr := json.Unmarshal(head, &d); uerr != nil || d.Dial == "" {
		return fmt.Errorf("endtoend: 非法 dial 帧（需 {\"dial\":\"addr\"}）")
	}
	// 出口拨号（dialPolicy 校验 + 解析，防 DNS rebinding TOCTOU）。
	resolved, ok := dialPolicy(d.Dial)
	if !ok {
		return fmt.Errorf("endtoend: 出口拨号目标未通过拨号策略: %s", d.Dial)
	}
	dialAddr := resolved
	if dialAddr == "" {
		dialAddr = d.Dial
	}
	remote, err := net.DialTimeout("tcp", dialAddr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("endtoend: X 出口拨号失败: %w", err)
	}
	defer remote.Close()
	// 透传 dial 帧到出口（T 侧 ServeE2EStream 据此 DetectE2E）。
	if err := writeLenFrame(remote, head); err != nil {
		return fmt.Errorf("endtoend: 透传 dial 帧失败: %w", err)
	}
	// 双向泵送密文字节（X 不接触明文）。
	iostream.Pump(outer, remote, iostream.PumpGrace)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return nil
}
