// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tunnel 提供基于 AES-256-GCM 的加密隧道转发功能。
//
// 核心能力
//
//   - 密钥管理：生成随机密钥（GenerateKey）、解析十六进制密钥（ParseKey）
//   - 加解密：AES-256-GCM 加密（Encrypt）和解密（Decrypt），nonce 随机生成并前置
//   - 编解码：Base64 编码（EncodeBody）和解码（DecodeBody），用于传输二进制数据
//   - 服务端：NewHandler 返回标准 http.Handler，可嵌入任意 HTTP 服务
//   - 客户端：Client 结构体提供 Do 和 DoRaw 方法，发送加密请求并解密响应
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
//
// 客户端调用（低级 API，直接使用 tunnel.Request）：
//
//	client.DoRaw(&tunnel.Request{
//	    Method: "GET",
//	    URL:    "https://api.example.com/data",
//	})
package tunnel

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	frameContentType = "application/x-tunnel-frame"
)

// Request 表示一个加密隧道请求，包含要转发到目标服务器的 HTTP 请求信息。
//
// Body 字段应使用 EncodeBody 进行 Base64 编码后再设置。
type Request struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// Response 表示加密隧道响应，包含目标服务器返回的 HTTP 响应信息。
//
// Body 字段为 Base64 编码，需使用 DecodeBody 解码后获取原始内容。
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
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

// EncodeBody 将二进制数据编码为 Base64 字符串，用于填充 Request.Body 或 Response.Body。
func EncodeBody(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// DecodeBody 将 Base64 字符串解码为原始二进制数据，用于解析 Request.Body 或 Response.Body。
func DecodeBody(encoded string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(encoded)
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
func decodeMetadataFrame(r io.Reader, key []byte) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("read metadata length: %w", err)
	}
	metaLen := binary.BigEndian.Uint32(lenBuf[:])
	encMeta := make([]byte, metaLen)
	if _, err := io.ReadFull(r, encMeta); err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	return Decrypt(key, encMeta)
}

// NewHandler 创建一个加密隧道 HTTP 处理器，可嵌入任意 http.ServeMux。
//
// 处理器统一使用流式帧协议（application/x-tunnel-frame）：
//  1. 从请求体读取 metadata 帧，解密得到目标 URL、Method、Headers
//  2. 剩余请求体通过 DecryptStream 流式解密，作为代理请求 body
//  3. 代理请求转发到目标 URL
//  4. 响应 metadata 帧 + EncryptStream 流式加密目标响应 body
//
// 如果 key 为空，处理器直接返回 403 Forbidden。
// 使用方式：mux.Handle("POST /tunnel", tunnel.NewHandler(key))
func NewHandler(key []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(key) == 0 {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		// 1. 解析 metadata 帧
		metaJSON, err := decodeMetadataFrame(r.Body, key)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req Request
		if err := json.Unmarshal(metaJSON, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// 2. r.Body 剩余部分为流式加密 body，通过 Pipe + DecryptStream 流式解密
		bodyPr, bodyPw := io.Pipe()
		go func() {
			_, decErr := DecryptStream(key, r.Body, bodyPw)
			bodyPw.CloseWithError(decErr)
		}()

		// 3. 构造并发送代理请求
		proxyReq, err := http.NewRequestWithContext(r.Context(), req.Method, req.URL, bodyPr)
		if err != nil {
			bodyPr.CloseWithError(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for k, v := range req.Headers {
			proxyReq.Header.Set(k, v)
		}

		httpClient := &http.Client{}
		resp, err := httpClient.Do(proxyReq)
		if err != nil {
			// 转发失败：返回 502，仍使用帧格式
			errMetaJSON, _ := json.Marshal(Response{
				Status:  502,
				Headers: map[string]string{},
			})
			errMetaFrame, _ := encodeMetadataFrame(key, errMetaJSON)
			w.Header().Set("Content-Type", frameContentType)
			w.WriteHeader(http.StatusOK)
			w.Write(errMetaFrame)
			EncryptStream(key, strings.NewReader(err.Error()), w) //nolint:errcheck
			return
		}
		defer resp.Body.Close()

		// 4. 收集响应头
		respHeaders := make(map[string]string)
		for k := range resp.Header {
			respHeaders[k] = resp.Header.Get(k)
		}

		// 5. 写出响应：metadata 帧 + 流式加密 body
		respMetaJSON, err := json.Marshal(Response{Status: resp.StatusCode, Headers: respHeaders})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		metaFrame, err := encodeMetadataFrame(key, respMetaJSON)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", frameContentType)
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(metaFrame); err != nil {
			return
		}
		EncryptStream(key, resp.Body, w) //nolint:errcheck
	})
}

// Client 是加密隧道客户端，用于向隧道服务端发送加密请求并接收解密响应。
//
// 零值不可用，必须通过 NewClient 创建。
type Client struct {
	Key        []byte
	TunnelURL  string
	HTTPClient *http.Client
}

// NewClient 创建一个加密隧道客户端。
//
// 参数：
//   - hexKey: 64 位十六进制密钥字符串，与 sproxy 服务端 tunnel_key 一致
//   - tunnelURL: 隧道服务端地址，如 "http://proxy:8080/tunnel"
//   - timeout: HTTP 客户端超时时间
//
// 如果 hexKey 格式无效（非 64 位十六进制），返回错误。
func NewClient(hexKey, tunnelURL string, timeout time.Duration) (*Client, error) {
	key, err := ParseKey(hexKey)
	if err != nil {
		return nil, err
	}
	return &Client{
		Key:       key,
		TunnelURL: strings.TrimRight(tunnelURL, "/"),
		HTTPClient: &http.Client{
			Timeout: timeout,
		},
	}, nil
}

// DoRaw 发送一个加密隧道请求（使用 tunnel.Request）并返回解密后的响应。
//
// 这是低级 API，直接操作 tunnel.Request / tunnel.Response 结构体。
// 推荐使用 Do(req *http.Request) 获取标准库类型。
//
// 内部使用流式帧协议：metadata 帧 + EncryptStream 发送请求，
// decodeMetadataFrame + DecryptStream 接收响应。
// Response.Body 仍为 Base64 编码字符串，与旧版签名完全兼容。
//
// 如果 HTTP 状态码不是 200，返回错误。
func (c *Client) DoRaw(req *Request) (*Response, error) {
	// 1. metadata（不含 body 字段）
	metaOnly := &Request{Method: req.Method, URL: req.URL, Headers: req.Headers}
	metaJSON, err := json.Marshal(metaOnly)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	metaFrame, err := encodeMetadataFrame(c.Key, metaJSON)
	if err != nil {
		return nil, fmt.Errorf("encode metadata frame: %w", err)
	}

	// 2. body：将 req.Body（Base64）解码后作为流式加密源
	var bodyReader io.Reader = strings.NewReader("")
	if req.Body != "" {
		bodyBytes, decErr := DecodeBody(req.Body)
		if decErr != nil {
			return nil, fmt.Errorf("decode request body: %w", decErr)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}
	pr, pw := io.Pipe()
	go func() {
		_, encErr := EncryptStream(c.Key, bodyReader, pw)
		pw.CloseWithError(encErr)
	}()

	// 3. POST：metaFrame + encrypted stream
	combined := io.MultiReader(bytes.NewReader(metaFrame), pr)
	httpResp, err := c.HTTPClient.Post(c.TunnelURL, frameContentType, combined)
	if err != nil {
		return nil, fmt.Errorf("post request: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("tunnel error (HTTP %d): %s", httpResp.StatusCode, string(errBody))
	}

	// 4. 响应：decodeMetadataFrame + DecryptStream（全量收集，保持 Response.Body 为 Base64）
	respMetaJSON, err := decodeMetadataFrame(httpResp.Body, c.Key)
	if err != nil {
		return nil, fmt.Errorf("decode response metadata: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(respMetaJSON, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response metadata: %w", err)
	}

	var bodyBuf bytes.Buffer
	if _, err := DecryptStream(c.Key, httpResp.Body, &bodyBuf); err != nil {
		return nil, fmt.Errorf("decrypt response body: %w", err)
	}
	resp.Body = EncodeBody(bodyBuf.Bytes())

	return &resp, nil
}

// Do 接受标准 *http.Request，通过加密隧道转发并返回标准 *http.Response。
//
// 这是推荐的主 API，使用标准库类型，调用方零学习成本。
// 所有请求/响应统一使用流式帧协议，内存占用恒定（不超过单个加密块大小）。
// 返回的 *http.Response.Body 为流式 Reader，调用方可边读边消费，关闭时自动释放底层连接。
// 目标返回非 2xx 状态码时，仍返回 *http.Response（非 error），StatusCode 正确反映目标状态。
func (c *Client) Do(req *http.Request) (*http.Response, error) {
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
	httpResp, err := c.HTTPClient.Post(c.TunnelURL, frameContentType, combined)
	if err != nil {
		return nil, fmt.Errorf("post request: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		return nil, fmt.Errorf("tunnel error (HTTP %d): %s", httpResp.StatusCode, string(errBody))
	}

	// 4. 解析响应 metadata
	respMetaJSON, err := decodeMetadataFrame(httpResp.Body, c.Key)
	if err != nil {
		httpResp.Body.Close()
		return nil, fmt.Errorf("decode response metadata: %w", err)
	}
	var tunnelResp Response
	if err := json.Unmarshal(respMetaJSON, &tunnelResp); err != nil {
		httpResp.Body.Close()
		return nil, fmt.Errorf("unmarshal response metadata: %w", err)
	}

	// 5. 响应 body：流式 Pipe + DecryptStream goroutine
	// 调用方 resp.Body.Close() 会关闭 rpr，进而触发 rpw.CloseWithError，终止 goroutine
	rpr, rpw := io.Pipe()
	go func() {
		_, decErr := DecryptStream(c.Key, httpResp.Body, rpw)
		rpw.CloseWithError(decErr)
		httpResp.Body.Close()
	}()

	header := make(http.Header)
	for k, v := range tunnelResp.Headers {
		header.Set(k, v)
	}

	return &http.Response{
		Status:     fmt.Sprintf("%d %s", tunnelResp.Status, http.StatusText(tunnelResp.Status)),
		StatusCode: tunnelResp.Status,
		Header:     header,
		Body:       rpr,
	}, nil
}
