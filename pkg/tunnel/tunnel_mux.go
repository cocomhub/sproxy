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

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// Tunnel 在一条 mux 多路复用连接之上提供 HTTP 请求-响应交换。
type Tunnel struct {
	mux *mux.Mux
	key []byte
}

func NewTunnel(m *mux.Mux, key []byte) *Tunnel {
	return &Tunnel{mux: m, key: key}
}

// Do 发送 HTTP 请求并返回响应。
func (t *Tunnel) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	stream, err := t.mux.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("tunnel: open stream: %w", err)
	}

	reqMeta, err := json.Marshal(&Request{
		Method:  req.Method,
		URL:     req.URL.RequestURI(),
		Headers: flattenHeaders(req.Header),
	})
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("tunnel: marshal request: %w", err)
	}

	var metaBytes []byte
	if t.key != nil {
		metaBytes, err = Encrypt(t.key, reqMeta)
	} else {
		metaBytes = reqMeta
	}
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("tunnel: encrypt: %w", err)
	}

	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(metaBytes)))
	if _, err := stream.Write(lenBuf); err != nil {
		stream.Close()
		return nil, fmt.Errorf("tunnel: write meta len: %w", err)
	}
	if _, err := stream.Write(metaBytes); err != nil {
		stream.Close()
		return nil, fmt.Errorf("tunnel: write meta: %w", err)
	}

	if req.Body != nil {
		if t.key != nil {
			if _, err := EncryptStream(t.key, req.Body, stream); err != nil {
				stream.Close()
				return nil, fmt.Errorf("tunnel: encrypt body: %w", err)
			}
		} else {
			if _, err := io.Copy(stream, req.Body); err != nil {
				stream.Close()
				return nil, fmt.Errorf("tunnel: write body: %w", err)
			}
		}
	}

	if err := stream.CloseWrite(); err != nil {
		stream.Close()
		return nil, fmt.Errorf("tunnel: close write: %w", err)
	}

	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		stream.Close()
		return nil, fmt.Errorf("tunnel: read resp meta len: %w", err)
	}
	metaLen := binary.BigEndian.Uint32(lenBuf)
	respMetaRaw := make([]byte, metaLen)
	if _, err := io.ReadFull(stream, respMetaRaw); err != nil {
		stream.Close()
		return nil, fmt.Errorf("tunnel: read resp meta: %w", err)
	}

	var respMeta Response
	if t.key != nil {
		plainMeta, err := Decrypt(t.key, respMetaRaw)
		if err != nil {
			stream.Close()
			return nil, fmt.Errorf("tunnel: decrypt resp: %w", err)
		}
		if err := json.Unmarshal(plainMeta, &respMeta); err != nil {
			stream.Close()
			return nil, fmt.Errorf("tunnel: unmarshal resp: %w", err)
		}
	} else {
		if err := json.Unmarshal(respMetaRaw, &respMeta); err != nil {
			stream.Close()
			return nil, fmt.Errorf("tunnel: unmarshal resp: %w", err)
		}
	}

	return &http.Response{
		Status:        fmt.Sprintf("%d %s", respMeta.Status, http.StatusText(respMeta.Status)),
		StatusCode:    respMeta.Status,
		Proto:         respMeta.Proto,
		Header:        respMeta.Headers.Clone(),
		Body:          &streamBody{stream: stream, key: t.key},
		ContentLength: respMeta.ContentLength,
	}, nil
}

// streamBody 包装 mux.Stream 为 io.ReadCloser，用于响应体。
// 当 key 非 nil 时，自动解密流。
type streamBody struct {
	stream *mux.Stream
	key    []byte
	once   sync.Once
	pr     *io.PipeReader
	pw     *io.PipeWriter
}

func (b *streamBody) Read(p []byte) (int, error) {
	if b.key != nil {
		b.once.Do(func() {
			b.pr, b.pw = io.Pipe()
			go func() {
				_, err := DecryptStream(b.key, b.stream, b.pw)
				b.pw.CloseWithError(err)
			}()
		})
		return b.pr.Read(p)
	}
	return b.stream.Read(p)
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

// Serve 在隧道上提供 HTTP 服务（服务端）。
func (t *Tunnel) Serve(ctx context.Context, handler http.Handler) error {
	for {
		stream, err := t.mux.Accept(ctx)
		if err != nil {
			return fmt.Errorf("tunnel: accept: %w", err)
		}
		go t.handleStream(stream, handler)
	}
}

func (t *Tunnel) handleStream(stream *mux.Stream, handler http.Handler) {
	defer stream.CloseWrite()
	// 注意：不 defer stream.Close() — 由客户端读完响应后主动 Close

	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		return
	}
	metaLen := binary.BigEndian.Uint32(lenBuf)
	if metaLen > MaxMetadataBytes {
		return
	}
	metaRaw := make([]byte, metaLen)
	if _, err := io.ReadFull(stream, metaRaw); err != nil {
		return
	}

	var reqMeta Request
	if t.key != nil {
		plain, err := Decrypt(t.key, metaRaw)
		if err != nil {
			return
		}
		if err := json.Unmarshal(plain, &reqMeta); err != nil {
			return
		}
	} else {
		if err := json.Unmarshal(metaRaw, &reqMeta); err != nil {
			return
		}
	}

	var bodyReader io.ReadCloser
	if t.key != nil {
		pr, pw := io.Pipe()
		bodyReader = pr
		go func() {
			_, err := DecryptStream(t.key, stream, pw)
			pw.CloseWithError(err)
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

	// 缓冲整个响应 body
	buf := new(bytes.Buffer)
	code := http.StatusOK
	hdrs := make(http.Header)
	rw := &bufferedResponseWriter{buf: buf, code: &code, hdrs: &hdrs}
	handler.ServeHTTP(rw, localReq)
	bodyReader.Close()

	respMetaJSON, _ := json.Marshal(Response{
		Proto:         "HTTP/1.1",
		Status:        code,
		Headers:       hdrs,
		ContentLength: -1,
	})

	var metaBytes []byte
	if t.key != nil {
		metaBytes, _ = Encrypt(t.key, respMetaJSON)
	} else {
		metaBytes = respMetaJSON
	}

	lb := make([]byte, 4)
	binary.BigEndian.PutUint32(lb, uint32(len(metaBytes)))
	stream.Write(lb)
	stream.Write(metaBytes)

	if t.key != nil {
		EncryptStream(t.key, buf, stream)
	} else {
		io.Copy(stream, buf)
	}
}

// noopCloseReader 包装 io.Reader 为 io.ReadCloser，Close 是空操作。
type noopCloseReader struct{ io.Reader }

func (noopCloseReader) Close() error { return nil }

// bufferedResponseWriter 缓冲整个响应。
type bufferedResponseWriter struct {
	buf      *bytes.Buffer
	code     *int
	hdrs     *http.Header
	mu       sync.Mutex
	wroteHdr bool
}

func (rw *bufferedResponseWriter) Header() http.Header { return *rw.hdrs }

func (rw *bufferedResponseWriter) WriteHeader(code int) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if !rw.wroteHdr {
		*rw.code = code
		rw.wroteHdr = true
	}
}

func (rw *bufferedResponseWriter) Write(data []byte) (int, error) {
	if !rw.wroteHdr {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.buf.Write(data)
}

// flattenHeaders 将 http.Header（map[string][]string）转为 map[string]string。
func flattenHeaders(h http.Header) map[string]string {
	if h == nil {
		return nil
	}
	r := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			r[k] = v[0]
		}
	}
	return r
}
