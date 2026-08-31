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

const handshakeTimeout = 30 * time.Second

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
	// peerFP 记录握手时获得的对端身份指纹（展示/诊断用）。
	peerFP string
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

func NewTunnel(m *mux.Mux, key []byte, opts ...TunnelOption) *Tunnel {
	t := &Tunnel{
		mux:             m,
		key:             key,
		replayProtector: NewReplayProtector(),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// PeerFingerprint 返回握手时获得的对端身份指纹（未握手或无身份时为空字符串）。
// 仅供日志/诊断展示。
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
		ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
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
	if _, err := stream.Write(lenBuf); err != nil {
		return fmt.Errorf("tunnel: write meta len: %w", err)
	}
	if _, err := stream.Write(metaBytes); err != nil {
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
// 在进入 accept 循环前，同步执行 ECDH 握手（listener 侧），超时后回退到静态密钥。
// 若配置了对端指纹 pinning 且握手校验失败，Serve 返回错误终止（fail-closed）。
func (t *Tunnel) Serve(ctx context.Context, handler http.Handler) error {
	if t.key != nil && t.mux.Role() == mux.RoleListener {
		hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
		// C-1 修复：静态密钥参与会话密钥派生（非匿名 ECDH）。t.key 非 nil 才进入
		// 本分支，故此处恒传非 nil staticKey。与 dialer 侧对称，确保两端派生一致。
		// 同步发布协议变更：旧对端（不混 key）与此端握手将因 sessionKey 不一致而失败。
		sk, peerFP, err := performHandshakeWithIdentity(hctx, t.mux, false, t.identity, t.peerFingerprints, t.key)
		cancel()
		switch {
		case err == nil:
			t.skMu.Lock()
			t.sessionKey = sk
			t.peerFP = peerFP
			t.skMu.Unlock()
		case len(t.peerFingerprints) > 0:
			// fail-closed：配置了 pin 但握手失败，拒绝整个隧道，不进入 accept 循环。
			return fmt.Errorf("tunnel: 对端指纹校验失败: %w", err)
		default:
			slog.Warn("ECDH 握手失败（listener），回退到静态密钥", "error", err)
		}
		// 未配置 pin 时即使握手失败，Serve 也继续运行（向后兼容）
	}

	for {
		stream, err := t.mux.Accept(ctx)
		if err != nil {
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
	stream.Write(lb)
	stream.Write(metaBytes)

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
