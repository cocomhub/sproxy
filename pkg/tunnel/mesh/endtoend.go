// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
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
