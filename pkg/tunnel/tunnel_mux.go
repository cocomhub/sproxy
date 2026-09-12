// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// defaultHandshakeTimeout 是隧道握手的默认超时；可经 WithHandshakeTimeout 覆写。
const defaultHandshakeTimeout = 30 * time.Second

// Tunnel 在一条 mux 多路复用连接之上提供 HTTP 请求-响应交换。
type Tunnel struct {
	mux             *mux.Mux
	key             []byte
	handshake       sync.Once
	sessionKey      []byte
	skMu            sync.Mutex
	replayProtector *ReplayProtector

	// identity 是本端长时身份（可选，P1 身份 pinning）。
	identity *Identity
	// peerFingerprints 是对端身份指纹 pinning 列表（非空时握手 fail-closed 校验）。
	peerFingerprints []string
	// handshakeErr 记录 dialer 侧握手失败（仅配置 pin 时置位，fail-closed）。
	handshakeErr error
	// peerFP 记录握手获得的对端身份指纹（签名校验过，可作授权输入——契约见 PeerFingerprint）。
	peerFP string
	// handshakeTimeout 是本次隧道握手的超时（WithHandshakeTimeout 覆写，默认 30s）。
	handshakeTimeout time.Duration
}

// TunnelOption 配置 Tunnel 的可选参数。
type TunnelOption func(*Tunnel)

// WithIdentity 设置本端长时身份密钥对（用于在对端 pin 本端时提供公钥）。
func WithIdentity(id *Identity) TunnelOption {
	return func(t *Tunnel) {
		t.identity = id
	}
}

// WithPeerFingerprints 设置对端身份指纹 pinning 列表。
// 配置后握手时校验对端身份指纹，不匹配或对端未提供身份时拒绝（fail-closed）。
func WithPeerFingerprints(fps []string) TunnelOption {
	return func(t *Tunnel) {
		t.peerFingerprints = append([]string(nil), fps...)
	}
}

// WithHandshakeTimeout 覆写本次隧道握手的超时（<=0 时忽略，保持默认 30s）。
// dialer 侧作用于 ensureHandshake，listener 侧作用于 Serve 进入 accept 循环前的握手。
//
// 为什么需要它：Serve 的握手与 accept 循环**共用传入 ctx**，调用方无法单独缩短握手
// 阶段；远程只读 listener 需要一个远小于 30s 的握手窗口（对端一建立连接即握手，没有
// 需要久等的场景），否则每个停滞对端都要拖满 30s 才被拒、连接迟迟不释放。
func WithHandshakeTimeout(d time.Duration) TunnelOption {
	return func(t *Tunnel) {
		if d > 0 {
			t.handshakeTimeout = d
		}
	}
}

func NewTunnel(m *mux.Mux, key []byte, opts ...TunnelOption) *Tunnel {
	t := &Tunnel{
		mux:              m,
		key:              key,
		replayProtector:  NewReplayProtector(),
		handshakeTimeout: defaultHandshakeTimeout,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// PeerFingerprint 返回**本次握手已确立**的对端身份指纹（Ed25519 公钥指纹）。
//
// 契约（可作授权输入——Y 一期远程只读面据此判定对端是哪个 mesh 节点）：
//   - 该值由对端在握手阶段提供，并经 ed25519.Verify 对其身份公钥做签名校验
//     （ecdh.go 的 readPeerIdentity）。签名消息绑定本次握手的双方临时 ECDH 公钥
//     （identitySigMessage），即 proof of possession：宣称某指纹的一方必须持有
//     对应私钥，且签名不可跨会话重放——**他人无法冒用该指纹**。
//   - 本端配置了 WithPeerFingerprints 时，握手另外要求该指纹命中 pin 列表，否则
//     握手 fail-closed 失败（ErrPeerFingerprintMismatch/ErrPeerFingerprintRequired），
//     此时本方法返回空串。未配置 pin 时返回值仍不可伪造，但**未经本端信任锚比对**——
//     把它当授权依据的调用方必须自行比对可信列表（pkg/server 的 mesh_readers 即如此）。
//   - 仅对 keyed 隧道（NewTunnel 的 key 非 nil，握手已执行）有意义。
//
// **调用方必须先判空**：空字符串 = 未认证（未握手、握手失败、对端无身份、或旧对端无
// 身份扩展），**不得据此授权**；只有非空值才可作为授权输入使用。
//
// 并发安全（内部 skMu 保护）。写入点即两处握手完成处：dialer 侧 ensureHandshake
// （受 sync.Once 约束，一次）、listener 侧 Serve 进入 accept 循环前。
func (t *Tunnel) PeerFingerprint() string {
	t.skMu.Lock()
	defer t.skMu.Unlock()
	return t.peerFP
}

// HandshakeErr 返回握手失败的错误（fail-closed pinning 路径）。
// 未配置 pin、握手成功或未握手时返回 nil。供调用方在 Do 失败后判断握手是否
// 失败，从而决定是否需要关闭并重建 mux（避免复用残留 mux 发起第二次握手，
// 见 pkg/client.getTunnelMux）。
func (t *Tunnel) HandshakeErr() error {
	t.skMu.Lock()
	defer t.skMu.Unlock()
	return t.handshakeErr
}

// ensureHandshake 确保 ECDH 握手已完成。
// 在 dialer 侧：首次调用时发起握手（m.Open），返回握手完成。
// 在 listener 侧：握手由 Serve 在进入 accept 循环前完成。
// 注意：握手受 sync.Once 保护只执行一次，因此使用 context.Background()
// 而非请求级 context——握手是一次性操作，影响整个隧道生命周期，
// 不应被单个请求的生命周期取消。
func (t *Tunnel) ensureHandshake() {
	if t.key == nil {
		return
	}
	t.handshake.Do(func() {
		if t.mux.Role() != mux.RoleDialer {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), t.handshakeTimeout)
		defer cancel()
		// C-1 修复：静态密钥参与会话密钥派生（非匿名 ECDH）。t.key 非 nil 才进入
		// 本分支，故此处恒传非 nil staticKey。与 listener 侧对称，确保两端派生一致。
		// 同步发布协议变更：旧对端（不混 key）与此端握手将因 sessionKey 不一致而失败。
		sk, peerFP, err := performHandshakeWithIdentity(ctx, t.mux, true, t.identity, t.peerFingerprints, t.key)
		switch {
		case err == nil:
			t.skMu.Lock()
			t.sessionKey = sk
			t.peerFP = peerFP
			t.skMu.Unlock()
		case len(t.peerFingerprints) > 0:
			// fail-closed：配置了 pin 但握手失败，隧道操作必须拒绝，不回退静态密钥。
			// handshakeErr 在 skMu 下写入，供 HandshakeErr() 并发安全读取。
			t.skMu.Lock()
			t.handshakeErr = fmt.Errorf("tunnel: 对端指纹校验失败: %w", err)
			t.skMu.Unlock()
			slog.Error("隧道握手失败（dialer，已配置对端指纹 pinning）", "error", err)
		default:
			slog.Warn("ECDH 握手失败（dialer），回退到静态密钥", "error", err)
		}
	})
}

// encryptionKey 返回用于加密的密钥。
// 必须确保 handshake 已完成后调用。
func (t *Tunnel) encryptionKey() []byte {
	if t.key == nil {
		return nil
	}
	t.skMu.Lock()
	sk := t.sessionKey
	t.skMu.Unlock()
	if sk != nil {
		return sk
	}
	return t.key
}

// Do 发送 HTTP 请求并返回响应。
func (t *Tunnel) Do(req *http.Request) (*http.Response, error) {
	// 在打开请求流之前确保握手已完成
	t.ensureHandshake()
	// fail-closed：配置了对端指纹 pinning 且握手校验失败时，拒绝所有隧道操作。
	if err := t.HandshakeErr(); err != nil {
		return nil, err
	}

	ctx := req.Context()
	stream, err := t.mux.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("tunnel: open stream: %w", err)
	}

	if err = t.sendRequestMeta(stream, req); err != nil {
		stream.Close()
		return nil, err
	}

	if err = t.sendRequestBody(stream, req); err != nil {
		stream.Close()
		return nil, err
	}

	if err = stream.CloseWrite(); err != nil {
		stream.Close()
		return nil, fmt.Errorf("tunnel: close write: %w", err)
	}

	respMeta, err := t.readResponseMeta(stream)
	if err != nil {
		stream.Close()
		return nil, err
	}

	return &http.Response{
		Status:        fmt.Sprintf("%d %s", respMeta.Status, http.StatusText(respMeta.Status)),
		StatusCode:    respMeta.Status,
		Proto:         respMeta.Proto,
		Header:        respMeta.Headers.Clone(),
		Body:          &streamBody{stream: stream, key: t.encryptionKey()},
		ContentLength: respMeta.ContentLength,
	}, nil
}

// sendRequestMeta 将请求元数据序列化、加密并写入流。
func (t *Tunnel) sendRequestMeta(stream mux.Stream, req *http.Request) error {
	encKey := t.encryptionKey()
	reqMeta := &Request{
		Method:  req.Method,
		URL:     req.URL.RequestURI(),
		Headers: flattenHeaders(req.Header),
	}
	// 有密钥时填充重放保护字段
	if encKey != nil {
		reqMeta.IAT = time.Now().Unix()
		reqMeta.JTI = generateJTI()
	}
	reqMetaJSON, err := json.Marshal(reqMeta)
	if err != nil {
		return fmt.Errorf("tunnel: marshal request: %w", err)
	}
	var metaBytes []byte
	if encKey != nil {
		metaBytes, err = Encrypt(encKey, reqMetaJSON, []byte(AADMeta))
	} else {
		metaBytes = reqMetaJSON
	}
	if err != nil {
		return fmt.Errorf("tunnel: encrypt: %w", err)
	}

	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(metaBytes)))
	// writeFull：mux.Stream.Write 短写（窗口受限）时循环写足，不得忽略返回的 n。
	if err := writeFull(stream, lenBuf); err != nil {
		return fmt.Errorf("tunnel: write meta len: %w", err)
	}
	if err := writeFull(stream, metaBytes); err != nil {
		return fmt.Errorf("tunnel: write meta: %w", err)
	}
	return nil
}

// sendRequestBody 将请求体写入流。
func (t *Tunnel) sendRequestBody(stream mux.Stream, req *http.Request) error {
	if req.Body == nil {
		return nil
	}
	// 兜底：请求体写完后即使出错也必须让对端感知写入已结束,
	// 否则对端在 handleStream 停读后, 这里可能永久阻塞（断流死锁）。
	defer stream.CloseWrite()
	encKey := t.encryptionKey()
	if encKey != nil {
		if _, err := EncryptStream(encKey, req.Body, stream, []byte(AADStream)); err != nil {
			return fmt.Errorf("tunnel: encrypt body: %w", err)
		}
	} else {
		if _, err := io.Copy(stream, req.Body); err != nil {
			return fmt.Errorf("tunnel: write body: %w", err)
		}
	}
	return nil
}

// readResponseMeta 从流中读取响应元数据。
func (t *Tunnel) readResponseMeta(stream mux.Stream) (*Response, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		return nil, fmt.Errorf("tunnel: read resp meta len: %w", err)
	}
	metaLen := binary.BigEndian.Uint32(lenBuf)
	respMetaRaw := make([]byte, metaLen)
	if _, err := io.ReadFull(stream, respMetaRaw); err != nil {
		return nil, fmt.Errorf("tunnel: read resp meta: %w", err)
	}

	var respMeta Response
	encKey := t.encryptionKey()
	if encKey != nil {
		plainMeta, err := Decrypt(encKey, respMetaRaw, []byte(AADMeta))
		if err != nil {
			return nil, fmt.Errorf("tunnel: decrypt resp: %w", err)
		}
		if err := json.Unmarshal(plainMeta, &respMeta); err != nil {
			return nil, fmt.Errorf("tunnel: unmarshal resp: %w", err)
		}
	} else {
		if err := json.Unmarshal(respMetaRaw, &respMeta); err != nil {
			return nil, fmt.Errorf("tunnel: unmarshal resp: %w", err)
		}
	}
	return &respMeta, nil
}

// Serve 在隧道上提供 HTTP 服务。
// 在进入 accept 循环前，同步执行 ECDH 握手（listener 侧）。
// keyed listener（t.key != nil）握手失败即返回错误终止（fail-closed）——C-1 修复后
// 会话密钥绑定静态密钥，握手失败意味着对端不知道 key（或版本不一致），任何回退都会
// 引入固定静态密钥加密（丧失前向保密 + 跨连接重放窄面），完整兑现 AD-3 红线
// 「绝不允许匿名 ECDH + 静态密钥仅作握手失败回退」。
// 未配置 key（明文模式）不握手，行为不变（向后兼容）。
//
// 契约：与 relay.Serve 一致——**ctx 取消（正常关闭，含握手中断）→ 返回 nil；
// 返回非 nil ⟺ 终止性错误**（握手超时/协议失败/身份校验失败、mux 被关闭且 ctx
// 仍存活）。调用方的 `if err != nil` 判空有意义（非恒真比较），应保留。
func (t *Tunnel) Serve(ctx context.Context, handler http.Handler) error {
	if t.key != nil && t.mux.Role() == mux.RoleListener {
		hctx, cancel := context.WithTimeout(ctx, t.handshakeTimeout)
		// C-1 修复：静态密钥参与会话密钥派生（非匿名 ECDH）。t.key 非 nil 才进入
		// 本分支，故此处恒传非 nil staticKey。与 dialer 侧对称，确保两端派生一致。
		// 同步发布协议变更：旧对端（不混 key）与此端握手将因 sessionKey 不一致而失败。
		sk, peerFP, err := performHandshakeWithIdentity(hctx, t.mux, false, t.identity, t.peerFingerprints, t.key)
		cancel()
		if err != nil {
			// 区分「停机导致的握手中断」与「真握手失败」：判据必须是**父 ctx**（而非
			// hctx）。父 ctx 取消时 m.Accept/读写随 ctx 一同返回，握手必然以中断告终，
			// 那是优雅停机，归一化为 nil（与下方 accept 循环同一契约），否则调用方会
			// 把正常关闭误报为 fail-closed 终止性错误。
			// 反之，hctx 还会因 handshakeTimeout 超时而结束——那是**真失败**（对端停滞
			// / 垃圾字节 / 版本不一致），此时父 ctx 仍存活，必须保持非 nil，绝不能被
			// 这里归零。协议失败与身份校验失败同样发生在父 ctx 存活期间，同理保持非 nil。
			if ctx.Err() != nil {
				return nil
			}
			// fail-closed（审查 Important #1）：keyed listener 握手失败不回退静态密钥——
			// 任何对端（含攻击者）若不知道 key，派生 sessionKey 与合法对端不同，握手
			// 虽在协议层成功但数据面首帧必然解密失败；此处握手显式失败（停滞/垃圾/版本
			// 不一致）同样拒绝。避免固定静态密钥回退丧失前向保密与引入跨连接重放面。
			return fmt.Errorf("tunnel: 握手失败（keyed listener，fail-closed）: %w", err)
		}
		t.skMu.Lock()
		t.sessionKey = sk
		t.peerFP = peerFP
		t.skMu.Unlock()
	}

	for {
		stream, err := t.mux.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// 正常关闭：Accept 因 ctx 取消而返回，不是错误（与 relay.Serve 同契约：
				// ctx 取消 → nil；返回非 nil ⟺ 终止性错误）。
				return nil
			}
			return fmt.Errorf("tunnel: accept: %w", err)
		}
		go t.handleStream(stream, handler)
	}
}

// handleStream 处理一条隧道流。
func (t *Tunnel) handleStream(stream mux.Stream, handler http.Handler) {
	defer stream.CloseWrite()

	reqMeta, err := t.readAndDecryptMeta(stream)
	if err != nil {
		// 审查 Minor #3（诊断）：数据面解密失败通常是密钥不匹配 / 版本不一致（C-1 修复
		// 的同步发布协议变更）或恶意对端。静默 return 让客户端只得泛化 EOF，运维无法
		// 定位升级后互通失败。加 Warn 记录错误供排查（不泄露密钥内容，仅错误信息）。
		if t.key != nil {
			slog.Warn("隧道请求元数据解密失败（可能密钥不匹配/版本不一致）", "error", err)
		}
		return
	}

	// 重放保护：当有密钥时检查 IAT/JTI
	if t.key != nil && (reqMeta.IAT > 0 || reqMeta.JTI != "") {
		if vErr := t.replayProtector.Validate(reqMeta.JTI, reqMeta.IAT); vErr != nil {
			slog.Warn("mux 重放检测失败", "error", vErr, "jti", reqMeta.JTI)
			// 写回错误响应，避免 dialer 侧 Do() 阻塞等待
			errResp := bytes.NewBufferString(vErr.Error())
			t.writeEncryptedResponse(stream, http.StatusTooEarly, make(http.Header), errResp)
			return
		}
	}

	var bodyReader io.ReadCloser
	encKey := t.encryptionKey()
	if encKey != nil {
		pr, pw := io.Pipe()
		bodyReader = pr
		go func() {
			_, decErr := DecryptStream(encKey, stream, pw, []byte(AADStream))
			pw.CloseWithError(decErr)
		}()
	} else {
		bodyReader = &noopCloseReader{Reader: stream}
	}

	localReq, err := http.NewRequest(reqMeta.Method, reqMeta.URL, bodyReader)
	if err != nil {
		return
	}
	for k, v := range reqMeta.Headers {
		localReq.Header.Set(k, v)
	}

	buf := new(bytes.Buffer)
	code := http.StatusOK
	hdrs := make(http.Header)
	rw := &bufferedResponseWriter{buf: buf, code: &code, hdrs: &hdrs}
	handler.ServeHTTP(rw, localReq)
	bodyReader.Close()

	t.writeEncryptedResponse(stream, code, hdrs, buf)
}

// readAndDecryptMeta 从流中读取请求元数据。
func (t *Tunnel) readAndDecryptMeta(stream mux.Stream) (*Request, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		return nil, err
	}
	metaLen := binary.BigEndian.Uint32(lenBuf)
	if metaLen > MaxMetadataBytes {
		return nil, ErrMetadataTooLarge
	}
	metaRaw := make([]byte, metaLen)
	if _, err := io.ReadFull(stream, metaRaw); err != nil {
		return nil, err
	}

	var reqMeta Request
	encKey := t.encryptionKey()
	if encKey != nil {
		plain, err := Decrypt(encKey, metaRaw, []byte(AADMeta))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(plain, &reqMeta); err != nil {
			return nil, err
		}
	} else {
		if err := json.Unmarshal(metaRaw, &reqMeta); err != nil {
			return nil, err
		}
	}
	return &reqMeta, nil
}

// writeEncryptedResponse 将响应 metadata 和 body 写入流。
func (t *Tunnel) writeEncryptedResponse(stream mux.Stream, code int, hdrs http.Header, buf *bytes.Buffer) {
	respMetaJSON, _ := json.Marshal(Response{
		Proto:         "HTTP/1.1",
		Status:        code,
		Headers:       hdrs,
		ContentLength: -1,
	})

	encKey := t.encryptionKey()
	var metaBytes []byte
	if encKey != nil {
		metaBytes, _ = Encrypt(encKey, respMetaJSON, []byte(AADMeta))
	} else {
		metaBytes = respMetaJSON
	}

	lb := make([]byte, 4)
	binary.BigEndian.PutUint32(lb, uint32(len(metaBytes)))
	// writeFull：mux.Stream.Write 短写（窗口受限）时循环写足。此处曾直接忽略 n，
	// 大响应体（> 流控窗口 64 KB）会与元数据/密文错位，对端解密报 GCM 认证失败。
	//
	// 写失败（对端已关流）在此静默返回：本函数无错误返回位，且调用方
	// handleStream 的收尾路径对「对端已走」不做处理（与既有语义一致）。
	if err := writeFull(stream, lb); err != nil {
		return
	}
	if err := writeFull(stream, metaBytes); err != nil {
		return
	}

	if encKey != nil {
		EncryptStream(encKey, buf, stream, []byte(AADStream))
	} else {
		io.Copy(stream, buf)
	}
}

// streamBody 包装 mux.Stream 为 io.ReadCloser，用于响应体。
type streamBody struct {
	stream    mux.Stream
	key       []byte
	initOnce  sync.Once // 仅用于 pipe 初始化
	closeOnce sync.Once // 仅用于关闭
	pr        *io.PipeReader
	pw        *io.PipeWriter

	rdBuf []byte
	rdOff int
}

const streamBodyBufSize = 65536 // 64 KB 预读缓冲

func (b *streamBody) Read(p []byte) (int, error) {
	if b.key != nil {
		if len(b.rdBuf) == 0 || b.rdOff >= len(b.rdBuf) {
			b.rdBuf = make([]byte, streamBodyBufSize)
			b.initOnce.Do(func() {
				b.pr, b.pw = io.Pipe()
				go func() {
					_, err := DecryptStream(b.key, b.stream, b.pw, []byte(AADStream))
					b.pw.CloseWithError(err)
				}()
			})
			n, err := b.pr.Read(b.rdBuf)
			if err != nil && err != io.EOF {
				return 0, err
			}
			b.rdBuf = b.rdBuf[:n]
			b.rdOff = 0
			if n == 0 {
				return 0, io.EOF
			}
		}
		n := copy(p, b.rdBuf[b.rdOff:])
		b.rdOff += n
		return n, nil
	}

	if b.rdOff >= len(b.rdBuf) {
		b.rdBuf = make([]byte, streamBodyBufSize)
		n, err := io.ReadAtLeast(b.stream, b.rdBuf, 1)
		if err != nil && err != io.EOF {
			return 0, err
		}
		b.rdBuf = b.rdBuf[:n]
		b.rdOff = 0
		if n == 0 {
			return 0, io.EOF
		}
	}

	n := copy(p, b.rdBuf[b.rdOff:])
	b.rdOff += n
	return n, nil
}

func (b *streamBody) Close() error {
	b.closeOnce.Do(func() {
		if b.pr != nil {
			b.pr.Close()
			return
		}
		b.stream.Close()
	})
	return nil
}
