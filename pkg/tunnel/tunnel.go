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
//   - 客户端：Client 结构体提供 Do 方法，发送加密请求并解密响应
//
// 协议格式
//
// 请求：客户端将 Request JSON 序列化后 AES-256-GCM 加密，通过 HTTP POST 发送密文。
// 响应：服务端解密请求、转发到目标、将 Response JSON 加密后返回。
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
// 客户端调用：
//
//	client, _ := tunnel.NewClient("0123456789abcdef...", "http://proxy:8080/tunnel", 30*time.Second)
//	resp, _ := client.Do(&tunnel.Request{
//	    Method: "GET",
//	    URL:    "https://api.example.com/data",
//	})
//	fmt.Println(resp.Status)
package tunnel

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

// NewHandler 创建一个加密隧道 HTTP 处理器，可嵌入任意 http.ServeMux。
//
// 处理器流程：
//  1. 读取请求体（密文）
//  2. 使用 key 解密得到 Request JSON
//  3. 构造 HTTP 请求转发到目标 URL
//  4. 将目标响应加密后返回
//
// 如果 key 为空，处理器直接返回 403 Forbidden。
// 使用方式：mux.Handle("POST /tunnel", tunnel.NewHandler(key))
func NewHandler(key []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(key) == 0 {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		plaintext, err := Decrypt(key, body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var req Request
		if err := json.Unmarshal(plaintext, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var bodyReader io.Reader
		if req.Body != "" {
			decoded, err := DecodeBody(req.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			bodyReader = bytes.NewReader(decoded)
		}

		proxyReq, err := http.NewRequestWithContext(r.Context(), req.Method, req.URL, bodyReader)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		for k, v := range req.Headers {
			proxyReq.Header.Set(k, v)
		}

		client := &http.Client{}
		resp, err := client.Do(proxyReq)
		if err != nil {
			tunnelResp := Response{
				Status:  502,
				Headers: map[string]string{},
				Body:    EncodeBody([]byte(err.Error())),
			}
			writeEncryptedResponse(w, key, tunnelResp)
			return
		}
		defer resp.Body.Close()

		respBodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		respHeaders := make(map[string]string)
		for k := range resp.Header {
			respHeaders[k] = resp.Header.Get(k)
		}

		tunnelResp := Response{
			Status:  resp.StatusCode,
			Headers: respHeaders,
			Body:    EncodeBody(respBodyBytes),
		}
		writeEncryptedResponse(w, key, tunnelResp)
	})
}

func writeEncryptedResponse(w http.ResponseWriter, key []byte, resp Response) {
	respJSON, err := json.Marshal(resp)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	encrypted, err := Encrypt(key, respJSON)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encrypted)
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

// Do 发送一个加密隧道请求并返回解密后的响应。
//
// 流程：
//  1. 将 Request JSON 序列化
//  2. 使用 AES-256-GCM 加密
//  3. HTTP POST 到 TunnelURL
//  4. 解密响应体得到 Response
//
// 如果 HTTP 状态码不是 200，返回错误。
func (c *Client) Do(req *Request) (*Response, error) {
	payloadJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	encrypted, err := Encrypt(c.Key, payloadJSON)
	if err != nil {
		return nil, fmt.Errorf("encrypt request: %w", err)
	}

	httpResp, err := c.HTTPClient.Post(c.TunnelURL, "application/octet-stream", bytes.NewReader(encrypted))
	if err != nil {
		return nil, fmt.Errorf("post request: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tunnel error (HTTP %d): %s", httpResp.StatusCode, string(respBody))
	}

	decrypted, err := Decrypt(c.Key, respBody)
	if err != nil {
		return nil, fmt.Errorf("decrypt response: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(decrypted, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return &resp, nil
}

func httpRequestToTunnelRequest(req *http.Request) (*Request, error) {
	headers := make(map[string]string)
	for k := range req.Header {
		headers[k] = req.Header.Get(k)
	}

	var bodyEncoded string
	if req.Body != nil && req.Body != http.NoBody {
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		bodyEncoded = EncodeBody(bodyBytes)
	}

	return &Request{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: headers,
		Body:    bodyEncoded,
	}, nil
}

func tunnelResponseToHTTPResponse(resp *Response) (*http.Response, error) {
	bodyBytes, err := DecodeBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("decode response body: %w", err)
	}

	header := make(http.Header)
	for k, v := range resp.Headers {
		header.Set(k, v)
	}

	return &http.Response{
		Status:     fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status)),
		StatusCode: resp.Status,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(bodyBytes)),
	}, nil
}

// DoHTTP 接受标准 *http.Request，通过加密隧道转发并返回标准 *http.Response。
//
// 与 Do 不同，DoHTTP 直接使用标准库类型，调用方无需了解 tunnel.Request/Response 结构体。
// 目标返回非 2xx 状态码时，仍返回 *http.Response（非 error），StatusCode 正确反映目标状态。
func (c *Client) DoHTTP(req *http.Request) (*http.Response, error) {
	tunnelReq, err := httpRequestToTunnelRequest(req)
	if err != nil {
		return nil, fmt.Errorf("convert request: %w", err)
	}

	tunnelResp, err := c.Do(tunnelReq)
	if err != nil {
		return nil, err
	}

	httpResp, err := tunnelResponseToHTTPResponse(tunnelResp)
	if err != nil {
		return nil, fmt.Errorf("convert response: %w", err)
	}

	return httpResp, nil
}
