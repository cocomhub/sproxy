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
	"net/http"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

const handshakeTimeout = 3 * time.Second

// Tunnel 在一条 mux 多路复用连接之上提供 HTTP 请求-响应交换。
type Tunnel struct {
	mux             *mux.Mux
	key             []byte
	handshake       sync.Once
	sessionKey      []byte
	skMu            sync.Mutex
	replayProtector *ReplayProtector
}

func NewTunnel(m *mux.Mux, key []byte) *Tunnel {
	return &Tunnel{
		mux:             m,
		key:             key,
		replayProtector: NewReplayProtector(),
	}
}

// ensureHandshake 确保 ECDH 握手已完成。
// 在 dialer 侧：首次调用时发起握手（m.Open），返回握手完成。
// 在 listener 侧：握手由 Serve 在进入 accept 循环前完成。
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
		sk, err := performHandshake(ctx, t.mux, true)
		if err == nil {
			t.skMu.Lock()
			t.sessionKey = sk
			t.skMu.Unlock()
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
// 在进入 accept 循环前，先执行 ECDH 握手（listener 侧）。
func (t *Tunnel) Serve(ctx context.Context, handler http.Handler) error {
	if t.key != nil && t.mux.Role() == mux.RoleListener {
		hctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
		sk, err := performHandshake(hctx, t.mux, false)
		cancel()
		if err == nil {
			t.skMu.Lock()
			t.sessionKey = sk
			t.skMu.Unlock()
		}
		// 即使握手失败，Serve 也继续运行（向后兼容）
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
	stream mux.Stream
	key    []byte
	once   sync.Once
	pr     *io.PipeReader
	pw     *io.PipeWriter

	rdBuf []byte
	rdOff int
}

const streamBodyBufSize = 65536 // 64 KB 预读缓冲

func (b *streamBody) Read(p []byte) (int, error) {
	if b.key != nil {
		if len(b.rdBuf) == 0 || b.rdOff >= len(b.rdBuf) {
			b.rdBuf = make([]byte, streamBodyBufSize)
			b.once.Do(func() {
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
	b.once.Do(func() {
		if b.pr != nil {
			b.pr.Close()
			return
		}
		b.stream.Close()
	})
	return nil
}
