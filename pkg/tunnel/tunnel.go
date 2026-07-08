// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tunnel 提供基于 AES-256-GCM 的加密隧道转发功能。
//
// 核心能力
//
//   - 密钥管理：生成随机密钥（GenerateKey）、解析十六进制密钥（ParseKey）
//   - 加解密：AES-256-GCM 加密（Encrypt）和解密（Decrypt），nonce 随机生成并前置
//   - 编解码：Base64 编码（EncodeBody）和解码（DecodeBody），用于传输二进制数据
//   - 服务端：NewHandler / NewLocalHandler 返回标准 http.Handler，可嵌入任意 HTTP 服务
//   - 本地路由：NewLocalHandler 支持将相对路径请求直接路由到本地 handler，无需外部 HTTP 调用
//   - 客户端：Client 结构体提供 Do 方法，发送加密请求并解密响应
//   - 密钥轮换：Handler.UpdateKey 支持在运行时热替换密钥，旧密钥保留短时窗口供存量连接使用
//   - 多路复用隧道：NewTunnel 基于 mux 层创建持久双向隧道，支持流复用和双向 HTTP 请求交换
//
// 协议格式
//
// 所有请求/响应统一使用帧协议（application/x-tunnel-frame）：
//
//	[4B big-endian metaLen][encrypted metadata][stream chunks...]
//
// 其中 encrypted metadata = Encrypt(key, metaJSON)，stream chunks 由 EncryptStream 生成，
// 格式为 [2B chunkLen][nonce|ciphertext|tag]，每块独立加密。
// body 为空时 stream chunks 部分为零字节。
//
// 安全特性
//
//   - 每次加密使用随机 nonce（12 字节），相同明文产生不同密文
//   - GCM 模式提供认证加密，可检测篡改
//   - 密钥为 32 字节（AES-256），需通过安全通道分发
//   - UpdateKey 热替换密钥时，旧密钥仍可解密存量连接（短时窗口）
//
// 使用示例
//
// 服务端嵌入：
//
//	key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
//	mux := http.NewServeMux()
//	mux.Handle("POST /tunnel", tunnel.NewHandler(key))
//	http.ListenAndServe(":8080", mux)
//
// 客户端调用（标准库风格）：
//
//	client, _ := tunnel.NewClient("0123456789abcdef...", "http://proxy:8080/tunnel", 30*time.Second)
//	req, _ := http.NewRequest("GET", "https://api.example.com/data", nil)
//	resp, _ := client.Do(req)
//	defer resp.Body.Close()
//	io.Copy(os.Stdout, resp.Body)
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
	"net/http"
	"strings"
	"sync"
)

const (
	frameContentType  = "application/x-tunnel-frame"
	headerContentType = "Content-Type"
	// MaxMetadataBytes 限制 metadata 帧的最大长度（1 MiB），防止远程攻击者通过伪造的 metaLen 触发 OOM。
	// 合法 metadata 通常仅几百字节到几 KiB，1 MiB 已留足上限。
	MaxMetadataBytes = 1 << 20
)

// ErrMetadataTooLarge 表示 metadata 帧长度超过 MaxMetadataBytes。
var ErrMetadataTooLarge = fmt.Errorf("metadata frame too large (> %d bytes)", MaxMetadataBytes)

// Request 表示一个加密隧道请求，包含要转发到目标服务器的 HTTP 请求信息。
type Request struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// Response 表示加密隧道响应，包含目标服务器返回的 HTTP 响应信息。
type Response struct {
	Proto         string      `json:"proto"`
	Status        int         `json:"status"`
	Headers       http.Header `json:"headers"`
	ContentLength int64       `json:"content_length"`
}

// ParseKey 将十六进制字符串解析为 32 字节的 AES-256 密钥。
//
// hexKey 必须是 64 个十六进制字符（编码 32 字节）。
// 返回的密钥可直接用于 Encrypt、Decrypt、NewHandler、NewClient。
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

// GenerateKey 使用 crypto/rand 生成一个随机的 AES-256 密钥，返回 64 字符的十六进制字符串。
//
// 生成的密钥可用于配置 sproxy 的 tunnel_key 和 sclient 的 tunnel_key。
func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// blockCache 缓存 AES cipher.Block 实例，避免同一密钥重复做密钥扩展。
// cipher.Block 接口是 goroutine-safe 的，可安全并发使用。
var blockCache sync.Map

// getCipherBlock 从缓存中获取 cipher.Block，缓存未命中时创建并缓存。
func getCipherBlock(key []byte) (cipher.Block, error) {
	k := string(key)
	if v, ok := blockCache.Load(k); ok {
		return v.(cipher.Block), nil //nolint:errcheck
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	blockCache.Store(k, block)
	return block, nil
}

// Encrypt 使用 AES-256-GCM 加密明文。
//
// 返回的密文格式为：nonce（12 字节） + ciphertext + auth_tag（16 字节）。
// 每次调用生成随机 nonce，相同明文产生不同密文。
func Encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := getCipherBlock(key)
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

// Decrypt 使用 AES-256-GCM 解密由 Encrypt 生成的密文。
//
// 自动从密文中提取 nonce（前 12 字节），然后进行 GCM 解密和认证。
// 如果密文被篡改或密钥不匹配，返回错误。
func Decrypt(key, data []byte) ([]byte, error) {
	block, err := getCipherBlock(key)
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

// encodeMetadataFrame 将 metadata JSON 加密后生成帧头：[4B big-endian metaLen][encrypted metadata]。
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

// decodeMetadataFrame 从 r 中读取 [4B metaLen][encrypted metadata]，解密后返回 metadata JSON。
// 读取完成后 r 的当前位置指向 stream chunks 的起始处。
// 如果 metaLen 超过 MaxMetadataBytes，返回 ErrMetadataTooLarge，防止恶意输入触发 OOM。
func decodeMetadataFrame(r io.Reader, key []byte) ([]byte, error) {
	encMeta, err := readEncMeta(r)
	if err != nil {
		return nil, err
	}
	return Decrypt(key, encMeta)
}

// readEncMeta 从 r 中读取 [4B metaLen][encrypted metadata] 原始密文（不解密）。
// 为 resolveKey 和 decodeMetadataFrame 提供共享的帧解析逻辑。
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

// isRelativePath 判断 urlStr 是否为相对路径（以 / 开头且不包含 ://）。
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

// streamRecorder 是一个自定义 http.ResponseWriter，将 handler 的输出通过 Pipe 流式输出，
// 供 EncryptStream 消费。状态码和响应头在首次 Write 时确定并通知给加密 goroutine。
//
// 所有对 header / statusCode 的访问都通过 mu 串行化：
// 标准 http.Handler 约定单个 goroutine 使用 ResponseWriter，但响应头组装 goroutine 与
// handler goroutine 之间通过 metaReady 跨边界共享 header，因此显式加锁更安全。
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
