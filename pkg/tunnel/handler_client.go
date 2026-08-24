// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Handler 处理加密隧道请求，支持外部转发和本地路由两种模式。
//
// 外部转发（默认）：将加密请求解密后转发到外部目标 URL。
// 本地路由：当配置了 localHandler 且请求 URL 为相对路径时，将请求直接路由到本地 handler。
//
// 两种模式统一使用流式帧协议，响应体通过 Pipe 流式加密，不缓冲在内存中。
//
// 认证驱动：隧道编解码密钥不再由进程级静态 tunnel_key 提供，改由 authMiddleware
// 验签后按 AK→SK 派生密钥（SetTunnelKey 放入请求 ctx），ServeHTTP 用 GetTunnelKey(ctx)
// 解密 metadata 与 body、加密响应。未携带密钥的请求 401。
// 重放保护：ServiceHTTP 中解析 metadata 后调用 replayProtector.Validate 检测重放攻击。
type Handler struct {
	httpClient      *http.Client
	localHandler    http.Handler
	logger          *slog.Logger
	replayProtector *ReplayProtector
}

// NewHandler 创建一个仅支持外部转发的加密隧道处理器。
//
// 密钥不在此构造（由 authMiddleware 派生后放入请求 ctx），key 参数仅占位（旧签名兼容）。
// logger 为 nil 时使用 slog.Default()。
// 使用方式：mux.Handle("POST /tunnel", tunnel.NewHandler(nil, logger))。
func NewHandler(key []byte, logger *slog.Logger) http.Handler {
	log := logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		httpClient: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logger:          log,
		replayProtector: NewReplayProtector(),
	}
}

// NewLocalHandler 创建一个支持本地路由和外部转发的加密隧道处理器。
//
// 当请求 URL 为绝对路径（如 /upload）且在 local 中注册时，直接在当前进程中转发到 local handler；
// 当请求 URL 为绝对 URL（如 https://example.com/api）时，与原 NewHandler 行为一致。
// 密钥由 authMiddleware 派生后放入请求 ctx，key 参数仅占位（旧签名兼容）。
// logger 为 nil 时使用 slog.Default()。
func NewLocalHandler(key []byte, local http.Handler, logger *slog.Logger) http.Handler {
	log := logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		httpClient: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		localHandler:    local,
		logger:          log,
		replayProtector: NewReplayProtector(),
	}
}

// UpdateKey 不再支持：隧道密钥由认证派生，无法热替换进程级密钥。
// 保留 API 以兼容 SIGHUP 流程（调用成为 no-op）。
func (h *Handler) UpdateKey(newKey []byte) {}

// resolveKey 从请求体解析 metadata 帧并解密。
//
// 密钥从请求 context（GetTunnelKey）获取；未携带密钥返回 ErrTunnelKeyMissing。
// 返回：解密后的 metadata JSON、匹配的密钥（用于后续 body 流解密）、错误。
func (h *Handler) resolveKey(r io.Reader, key []byte) ([]byte, []byte, error) {
	encMeta, err := readEncMeta(r)
	if err != nil {
		return nil, nil, err
	}
	if len(key) == 0 {
		return nil, nil, ErrTunnelKeyMissing
	}
	data, err := Decrypt(key, encMeta, []byte(AADMeta))
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt metadata: %w", err)
	}
	return data, key, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 隧道密钥来自请求 context（authMiddleware 验签后派生）。
	key := GetTunnelKey(r.Context())
	if len(key) == 0 {
		h.logger.Warn("隧道请求缺少派生密钥", "remote_addr", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	h.logger.Debug("隧道请求", "method", r.Method, "remote_addr", r.RemoteAddr)

	// 1. 解析 metadata 帧，用 context 密钥解密
	metaJSON, resolvedKey, err := h.resolveKey(r.Body, key)
	if err != nil {
		h.logger.Error("解析隧道 metadata 失败", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req Request
	if err := json.Unmarshal(metaJSON, &req); err != nil {
		h.logger.Error("反序列化隧道请求失败", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	h.logger.Debug("隧道请求 metadata", "method", req.Method, "url", req.URL)

	// 3. 重放保护检查：如果 metadata 中包含 IAT/JTI，则验证
	if req.IAT > 0 || req.JTI != "" {
		if err := h.replayProtector.Validate(req.JTI, req.IAT); err != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			r.Body.Close()
			h.logger.Warn("重放检测失败", "error", err, "jti", req.JTI)
			http.Error(w, "too early", http.StatusTooEarly) // 425
			return
		}
	}

	// 4. r.Body 剩余部分为流式加密 body，通过 Pipe + DecryptStream 流式解密
	bodyPr, bodyPw := io.Pipe()
	go func() {
		_, decErr := DecryptStream(resolvedKey, r.Body, bodyPw, []byte(AADStream))
		bodyPw.CloseWithError(decErr)
	}()

	// 分支：本地路由 vs 外部转发
	if h.localHandler != nil && isRelativePath(req.URL) {
		h.dispatchLocal(w, r, &req, bodyPr, resolvedKey)
	} else {
		h.forwardExternal(w, r, &req, bodyPr, resolvedKey)
	}
}

// dispatchLocal 将加密请求路由到本地 handler，响应体通过 Pipe 流式加密。
func (h *Handler) dispatchLocal(w http.ResponseWriter, r *http.Request, req *Request, body io.Reader, encKey []byte) {
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
		defer bodyPr.Close()

		select {
		case <-sr.metaReady:
		case <-r.Context().Done():
			return
		}

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
		metaFrame, _ := encodeMetadataFrame(encKey, respMetaJSON)

		w.Header().Set(headerContentType, frameContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(metaFrame)
		_, _ = EncryptStream(encKey, bodyPr, w, []byte(AADStream))
	}()

	// 同步运行本地 handler。
	// 使用 defer + recover 兜底：handler 即便 panic，也能保证 metaReady 被关闭 + bodyPw 被 Close，
	// 避免上方 goroutine 永远阻塞在 <-sr.metaReady 而导致整个隧道 goroutine 泄漏。
	func() {
		defer func() {
			sr.once.Do(func() { close(sr.metaReady) })
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
func (h *Handler) forwardExternal(w http.ResponseWriter, r *http.Request, req *Request, body io.Reader, encKey []byte) {
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
		errMetaJSON, _ := json.Marshal(Response{Status: 502, Headers: make(http.Header)})
		errMetaFrame, _ := encodeMetadataFrame(encKey, errMetaJSON)
		w.Header().Set(headerContentType, frameContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(errMetaFrame)
		if _, err = EncryptStream(encKey, strings.NewReader(err.Error()), w, []byte(AADStream)); err != nil {
			h.logger.Error("隧道错误响应加密失败", "error", err)
		}
		return
	}
	defer resp.Body.Close()

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
	metaFrame, err := encodeMetadataFrame(encKey, respMetaJSON)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set(headerContentType, frameContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(metaFrame)
	if _, err := EncryptStream(encKey, resp.Body, w, []byte(AADStream)); err != nil {
		h.logger.Error("隧道响应加密失败", "error", err)
	}
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
	headers := make(map[string]string)
	for k := range req.Header {
		headers[k] = req.Header.Get(k)
	}
	// 生成重放保护字段
	now := time.Now().Unix()
	jti := generateJTI()
	metaJSON, err := json.Marshal(&Request{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: headers,
		IAT:     now,
		JTI:     jti,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	metaFrame, err := encodeMetadataFrame(c.Key, metaJSON)
	if err != nil {
		return nil, fmt.Errorf("encode metadata frame: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		var src io.Reader = strings.NewReader("")
		if req.Body != nil && req.Body != http.NoBody {
			defer req.Body.Close()
			src = req.Body
		}
		_, encErr := EncryptStream(c.Key, src, pw, []byte(AADStream))
		pw.CloseWithError(encErr)
	}()

	combined := io.MultiReader(bytes.NewReader(metaFrame), pr)
	tunnelReq, err := http.NewRequestWithContext(req.Context(), "POST", c.TunnelURL, combined)
	if err != nil {
		pr.Close()
		return nil, fmt.Errorf("create tunnel request: %w", err)
	}
	tunnelReq.Header.Set(headerContentType, frameContentType)
	// 全链路追踪：把内层请求的 traceparent 复制到外层 /tunnel 请求，
	// 使服务端外层 requestLogMiddleware 记录的外层请求 trace_id 与客户端 trace 关联。
	if tp := req.Header.Get("Traceparent"); tp != "" {
		tunnelReq.Header.Set("Traceparent", tp)
	}
	httpResp, err := c.HTTPClient.Do(tunnelReq)
	if err != nil {
		// 兜底关闭上行 pipe：请求体加密 goroutine 仍在阻塞写 pw（io.Copy 下游断流时
		// 永不返回），必须 Close 使 EncryptStream 的写立即失败, 否则 uploadWg.Wait() 死锁。
		pr.Close()
		return nil, fmt.Errorf("post request: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		// 非 200（如 400/425）：服务端可能未消费完请求体，同样兜底关闭上行 pipe,
		// 解除请求体加密 goroutine 的阻塞。
		pr.Close()
		return nil, fmt.Errorf("tunnel error (HTTP %d): %s", httpResp.StatusCode, string(errBody))
	}

	respMetaJSON, err := decodeMetadataFrame(httpResp.Body, c.Key)
	if err != nil {
		httpResp.Body.Close()
		// 解码响应 metadata 失败同样意味着响应已结束, 上行 pipe 不再被消费,
		// 关闭避免请求体加密 goroutine 永久阻塞。
		pr.Close()
		return nil, fmt.Errorf("decode response metadata: %w", err)
	}
	var tunnelResp Response
	if err := json.Unmarshal(respMetaJSON, &tunnelResp); err != nil {
		httpResp.Body.Close()
		return nil, fmt.Errorf("unmarshal response metadata: %w", err)
	}

	rpr, rpw := io.Pipe()
	go func() {
		_, decErr := DecryptStream(c.Key, httpResp.Body, rpw, []byte(AADStream))
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
