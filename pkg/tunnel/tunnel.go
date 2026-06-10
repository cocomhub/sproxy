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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	frameContentType = "application/x-tunnel-frame"

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

// Encrypt 使用 AES-256-GCM 加密明文。
//
// 返回的密文格式为：nonce（12 字节） + ciphertext + auth_tag（16 字节）。
// 每次调用生成随机 nonce，相同明文产生不同密文。
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

// Decrypt 使用 AES-256-GCM 解密由 Encrypt 生成的密文。
//
// 自动从密文中提取 nonce（前 12 字节），然后进行 GCM 解密和认证。
// 如果密文被篡改或密钥不匹配，返回错误。
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

// Handler 处理加密隧道请求，支持外部转发和本地路由两种模式。
//
// 外部转发（默认）：将加密请求解密后转发到外部目标 URL。
// 本地路由：当配置了 localHandler 且请求 URL 为相对路径时，将请求直接路由到本地 handler。
//
// 两种模式统一使用流式帧协议，响应体通过 Pipe 流式加密，不缓冲在内存中。
//
// 密钥轮换：通过 UpdateKey 可运行时热替换密钥，旧密钥保留短时窗口供存量连接完成。
// 所有新加密使用新密钥；解密时先尝试新密钥，不匹配则尝试旧密钥。
type Handler struct {
	keyMu        sync.RWMutex
	primaryKey   []byte // 当前活跃密钥，用于加密和解密
	oldKey       []byte // 前一个密钥，仅用于解密存量连接（短时窗口）
	httpClient   *http.Client
	localHandler http.Handler
	logger       *slog.Logger
}

// NewHandler 创建一个仅支持外部转发的加密隧道处理器。
//
// 行为同旧版闭包实现。如果 key 为空，处理器直接返回 403 Forbidden。
// logger 为 nil 时使用 slog.Default()。
// 使用方式：mux.Handle("POST /tunnel", tunnel.NewHandler(key))
func NewHandler(key []byte, logger *slog.Logger) http.Handler {
	log := logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		primaryKey: key,
		httpClient: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logger: log,
	}
}

// NewLocalHandler 创建一个支持本地路由和外部转发的加密隧道处理器。
//
// 当请求 URL 为绝对路径（如 /upload）且在 local 中注册时，直接在当前进程中转发到 local handler；
// 当请求 URL 为绝对 URL（如 https://example.com/api）时，与原 NewHandler 行为一致。
// logger 为 nil 时使用 slog.Default()。
func NewLocalHandler(key []byte, local http.Handler, logger *slog.Logger) http.Handler {
	log := logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		primaryKey: key,
		httpClient: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		localHandler: local,
		logger:       log,
	}
}

// UpdateKey 热替换隧道加密密钥。
//
// 调用后，新连接使用 newKey 加密；存量连接仍可用旧密钥解密（写入者已使用旧密钥加密的流）。
// 多次调用只保留最近两代密钥（当前 + 前一代），更早的密钥不再接受。
func (h *Handler) UpdateKey(newKey []byte) {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	h.oldKey = h.primaryKey
	h.primaryKey = newKey
}

// resolveKey 从请求体解析 metadata 帧并尝试所有可用密钥解密。
//
// 返回：解密后的 metadata JSON、匹配的密钥（用于后续 body 流解密）、错误。
// 先尝试 primaryKey，失败后再尝试 oldKey。
func (h *Handler) resolveKey(r io.Reader) ([]byte, []byte, error) {
	encMeta, err := readEncMeta(r)
	if err != nil {
		return nil, nil, err
	}

	// 持读锁收集可用密钥列表，先尝试 primaryKey，再尝试 oldKey
	h.keyMu.RLock()
	keys := make([][]byte, 0, 2)
	keys = append(keys, h.primaryKey)
	if len(h.oldKey) > 0 {
		keys = append(keys, h.oldKey)
	}
	h.keyMu.RUnlock()

	var lastErr error
	for _, key := range keys {
		data, err := Decrypt(key, encMeta)
		if err == nil {
			return data, key, nil
		}
		lastErr = err
	}
	return nil, nil, fmt.Errorf("decrypt metadata with all keys: %w", lastErr)
}

// isRelativePath 判断 urlStr 是否为相对路径（以 / 开头且不包含 ://）。
func isRelativePath(urlStr string) bool {
	return !strings.Contains(urlStr, "://") && strings.HasPrefix(urlStr, "/")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(h.primaryKey) == 0 {
		h.logger.Warn("隧道密钥为空，拒绝请求")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	h.logger.Debug("隧道请求", "method", r.Method, "remote_addr", r.RemoteAddr)

	// 1. 解析 metadata 帧，使用 resolveKey 尝试 primary + old 密钥
	metaJSON, resolvedKey, err := h.resolveKey(r.Body)
	if err != nil {
		h.logger.Error("解析隧道 metadata 失败", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req Request
	if err := json.Unmarshal(metaJSON, &req); err != nil {
		h.logger.Error("反序列化隧道请求失败", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	h.logger.Debug("隧道请求 metadata", "method", req.Method, "url", req.URL)

	// 2. r.Body 剩余部分为流式加密 body，通过 Pipe + DecryptStream 流式解密
	//    使用 resolveKey 匹配成功的 resolvedKey（兼容正在轮换中的旧密钥）
	bodyPr, bodyPw := io.Pipe()
	go func() {
		_, decErr := DecryptStream(resolvedKey, r.Body, bodyPw)
		bodyPw.CloseWithError(decErr)
	}()

	// 分支：本地路由 vs 外部转发
	if h.localHandler != nil && isRelativePath(req.URL) {
		h.dispatchLocal(w, r, &req, bodyPr)
	} else {
		h.forwardExternal(w, r, &req, bodyPr)
	}
}

// dispatchLocal 将加密请求路由到本地 handler，响应体通过 Pipe 流式加密。
func (h *Handler) dispatchLocal(w http.ResponseWriter, r *http.Request, req *Request, body io.Reader) {
	h.logger.Debug("隧道本地路由", "method", req.Method, "url", req.URL)
	localReq, err := http.NewRequestWithContext(r.Context(), req.Method, req.URL, body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for k, v := range req.Headers {
		localReq.Header.Set(k, v)
	}

	// Pipe：本地 handler 写入 body，流式加密 goroutine 读取
	bodyPr, bodyPw := io.Pipe()
	sr := newStreamRecorder(bodyPw)

	// Goroutine：等待 metadata 就绪，写出 metadata 帧 + 流式加密 body
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-sr.metaReady

		sr.mu.Lock()
		code := sr.statusCode
		hdrs := sr.header.Clone()
		sr.mu.Unlock()

		respMetaJSON, _ := json.Marshal(Response{
			Proto:         "HTTP/1.1",
			Status:        code,
			Headers:       hdrs,
			ContentLength: -1,
		})
		// 用 primaryKey 加密响应（始终使用最新密钥）
		h.keyMu.RLock()
		encKey := h.primaryKey
		h.keyMu.RUnlock()
		metaFrame, _ := encodeMetadataFrame(encKey, respMetaJSON)

		w.Header().Set("Content-Type", frameContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(metaFrame)
		_, _ = EncryptStream(encKey, bodyPr, w)
		_ = bodyPr.Close()
	}()

	// 同步运行本地 handler。
	// 使用 defer + recover 兜底：handler 即便 panic，也能保证 metaReady 被关闭 + bodyPw 被 Close，
	// 避免上方 goroutine 永远阻塞在 <-sr.metaReady 而导致整个隧道 goroutine 泄漏。
	func() {
		defer func() {
			sr.once.Do(func() {
				close(sr.metaReady)
			})
			_ = bodyPw.Close()
			if rec := recover(); rec != nil {
				h.logger.Error("本地 handler panic", "panic", rec, "url", req.URL)
			}
		}()
		h.localHandler.ServeHTTP(sr, localReq)
	}()

	<-done
}

// forwardExternal 将加密请求转发到外部目标 URL，保持原 NewHandler 的完整行为。
func (h *Handler) forwardExternal(w http.ResponseWriter, r *http.Request, req *Request, body io.Reader) {
	h.logger.Debug("隧道外部转发", "method", req.Method, "url", req.URL)
	proxyReq, err := http.NewRequestWithContext(r.Context(), req.Method, req.URL, body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for k, v := range req.Headers {
		proxyReq.Header.Set(k, v)
	}

	resp, err := h.httpClient.Do(proxyReq)
	if err != nil {
		// 转发失败：返回 502，仍使用帧格式
		errMetaJSON, _ := json.Marshal(Response{
			Status:  502,
			Headers: make(http.Header),
		})
		h.keyMu.RLock()
		encKey := h.primaryKey
		h.keyMu.RUnlock()
		errMetaFrame, _ := encodeMetadataFrame(encKey, errMetaJSON)
		w.Header().Set("Content-Type", frameContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(errMetaFrame)
		if _, err := EncryptStream(encKey, strings.NewReader(err.Error()), w); err != nil {
			h.logger.Error("隧道错误响应加密失败", "error", err)
		}
		return
	}
	defer resp.Body.Close()

	// 写出响应：metadata 帧 + 流式加密 body
	respMetaJSON, err := json.Marshal(Response{
		Proto:         resp.Proto,
		Status:        resp.StatusCode,
		Headers:       resp.Header.Clone(),
		ContentLength: resp.ContentLength,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	h.keyMu.RLock()
	encKey := h.primaryKey
	h.keyMu.RUnlock()
	metaFrame, err := encodeMetadataFrame(encKey, respMetaJSON)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", frameContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(metaFrame); err != nil {
		return
	}
	if _, err := EncryptStream(encKey, resp.Body, w); err != nil {
		h.logger.Error("隧道响应加密失败", "error", err)
	}
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

	once      sync.Once
	metaReady chan struct{} // 关闭后表示 metadata（状态码、header）已就绪
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

// Client 是加密隧道客户端，用于向隧道服务端发送加密请求并接收解密响应。
//
// 零值不可用，必须通过 NewClient 创建。
type Client struct {
	Key        []byte
	TunnelURL  string
	HTTPClient *http.Client
	logger     *slog.Logger
}

// NewClient 创建一个加密隧道客户端。
//
// 参数：
//   - hexKey: 64 位十六进制密钥字符串，与 sproxy 服务端 tunnel_key 一致
//   - tunnelURL: 隧道服务端地址，如 "http://proxy:8080/tunnel"
//   - timeout: HTTP 客户端超时时间
//   - logger: 日志记录器，为 nil 时使用 slog.Default()
//
// 如果 hexKey 格式无效（非 64 位十六进制），返回错误。
func NewClient(hexKey, tunnelURL string, timeout time.Duration, logger *slog.Logger) (*Client, error) {
	key, err := ParseKey(hexKey)
	if err != nil {
		return nil, err
	}
	log := logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		Key:       key,
		TunnelURL: strings.TrimRight(tunnelURL, "/"),
		HTTPClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logger: log,
	}, nil
}

// Do 接受标准 *http.Request，通过加密隧道转发并返回标准 *http.Response。
//
// 使用标准库类型，调用方零学习成本。
// 所有请求/响应统一使用流式帧协议，内存占用恒定（不超过单个加密块大小）。
// 返回的 *http.Response.Body 为流式 Reader，调用方可边读边消费，关闭时自动释放底层连接。
// 目标返回非 2xx 状态码时，仍返回 *http.Response（非 error），StatusCode 正确反映目标状态。
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	c.logger.Debug("隧道客户端请求", "method", req.Method, "url", req.URL.String())

	// 1. 构造 metadata（不含 body）
	headers := make(map[string]string)
	for k := range req.Header {
		headers[k] = req.Header.Get(k)
	}
	metaJSON, err := json.Marshal(&Request{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: headers,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	metaFrame, err := encodeMetadataFrame(c.Key, metaJSON)
	if err != nil {
		return nil, fmt.Errorf("encode metadata frame: %w", err)
	}

	// 2. body 流式加密（无 body 时加密空流，输出零块）
	pr, pw := io.Pipe()
	go func() {
		var src io.Reader = strings.NewReader("")
		if req.Body != nil && req.Body != http.NoBody {
			defer req.Body.Close()
			src = req.Body
		}
		_, encErr := EncryptStream(c.Key, src, pw)
		pw.CloseWithError(encErr)
	}()

	// 3. POST：metaFrame + encrypted stream
	combined := io.MultiReader(bytes.NewReader(metaFrame), pr)
	tunnelReq, err := http.NewRequestWithContext(req.Context(), "POST", c.TunnelURL, combined)
	if err != nil {
		pr.Close()
		return nil, fmt.Errorf("create tunnel request: %w", err)
	}
	tunnelReq.Header.Set("Content-Type", frameContentType)
	httpResp, err := c.HTTPClient.Do(tunnelReq)
	if err != nil {
		return nil, fmt.Errorf("post request: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		c.logger.Error("隧道响应异常", "status", httpResp.StatusCode, "body", string(errBody))
		return nil, fmt.Errorf("tunnel error (HTTP %d): %s", httpResp.StatusCode, string(errBody))
	}

	// 4. 解析响应 metadata
	respMetaJSON, err := decodeMetadataFrame(httpResp.Body, c.Key)
	if err != nil {
		httpResp.Body.Close()
		c.logger.Error("解析隧道响应 metadata 失败", "error", err)
		return nil, fmt.Errorf("decode response metadata: %w", err)
	}
	var tunnelResp Response
	if err := json.Unmarshal(respMetaJSON, &tunnelResp); err != nil {
		httpResp.Body.Close()
		return nil, fmt.Errorf("unmarshal response metadata: %w", err)
	}

	c.logger.Debug("隧道响应 metadata", "status", tunnelResp.Status, "proto", tunnelResp.Proto)

	// 5. 响应 body：流式 Pipe + DecryptStream goroutine
	// 调用方 resp.Body.Close() 会关闭 rpr，进而触发 rpw.CloseWithError，终止 goroutine
	rpr, rpw := io.Pipe()
	go func() {
		_, decErr := DecryptStream(c.Key, httpResp.Body, rpw)
		rpw.CloseWithError(decErr)
		httpResp.Body.Close()
	}()

	return &http.Response{
		Status:        fmt.Sprintf("%d %s", tunnelResp.Status, http.StatusText(tunnelResp.Status)),
		StatusCode:    tunnelResp.Status,
		Proto:         tunnelResp.Proto,
		Header:        tunnelResp.Headers.Clone(),
		Body:          rpr,
		ContentLength: tunnelResp.ContentLength,
	}, nil
}
