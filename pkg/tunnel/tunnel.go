// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tunnel 提供基于 AES-256-GCM 的加密隧道转发功能。
package tunnel

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	frameContentType  = "application/x-tunnel-frame"
	headerContentType = "Content-Type"
	MaxMetadataBytes  = 1 << 20
)

var ErrMetadataTooLarge = fmt.Errorf("metadata frame too large (> %d bytes)", MaxMetadataBytes)

type Request struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type Response struct {
	Proto         string      `json:"proto"`
	Status        int         `json:"status"`
	Headers       http.Header `json:"headers"`
	ContentLength int64       `json:"content_length"`
}

func ParseKey(hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("invalid key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes (64 hex chars)")
	}
	return key, nil
}

func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return hex.EncodeToString(key), nil
}

func Encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

func Decrypt(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

func encodeMetadataFrame(key, metadataJSON []byte) ([]byte, error) {
	encMeta, err := Encrypt(key, metadataJSON)
	if err != nil {
		return nil, fmt.Errorf("encrypt metadata: %w", err)
	}
	buf := make([]byte, 4+len(encMeta))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(encMeta)))
	copy(buf[4:], encMeta)
	return buf, nil
}

func decodeMetadataFrame(r io.Reader, key []byte) ([]byte, error) {
	encMeta, err := readEncMeta(r)
	if err != nil {
		return nil, err
	}
	return Decrypt(key, encMeta)
}

func readEncMeta(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("read metadata length: %w", err)
	}
	metaLen := binary.BigEndian.Uint32(lenBuf[:])
	if metaLen > MaxMetadataBytes {
		return nil, ErrMetadataTooLarge
	}
	encMeta := make([]byte, metaLen)
	if _, err := io.ReadFull(r, encMeta); err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	return encMeta, nil
}

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

func isRelativePath(urlStr string) bool {
	return !strings.Contains(urlStr, "://") && strings.HasPrefix(urlStr, "/")
}

type noopCloseReader struct{ io.Reader }

func (noopCloseReader) Close() error { return nil }

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
	rw.mu.Lock()
	if !rw.wroteHdr {
		*rw.code = http.StatusOK
		rw.wroteHdr = true
	}
	rw.mu.Unlock()
	return rw.buf.Write(data)
}

type streamRecorder struct {
	header     http.Header
	statusCode int
	mu         sync.Mutex
	bodyWriter *io.PipeWriter
	once       sync.Once
	metaReady  chan struct{}
}

func newStreamRecorder(bodyWriter *io.PipeWriter) *streamRecorder {
	return &streamRecorder{
		header:     make(http.Header),
		statusCode: http.StatusOK,
		bodyWriter: bodyWriter,
		metaReady:  make(chan struct{}),
	}
}

func (sr *streamRecorder) Header() http.Header {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.header
}

func (sr *streamRecorder) WriteHeader(code int) {
	sr.mu.Lock()
	sr.statusCode = code
	sr.mu.Unlock()
}

func (sr *streamRecorder) Write(data []byte) (int, error) {
	sr.once.Do(func() {
		close(sr.metaReady)
	})
	return sr.bodyWriter.Write(data)
}

func defaultLogger(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.NewTextHandler(nil, nil))
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}
