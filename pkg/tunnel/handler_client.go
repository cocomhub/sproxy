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
	"sync"
	"time"
)

// Handler 处理加密隧道请求，支持外部转发和本地路由两种模式。
type Handler struct {
	keyMu        sync.RWMutex
	primaryKey   []byte
	oldKey       []byte
	httpClient   *http.Client
	localHandler http.Handler
	logger       *slog.Logger
}

// NewHandler 创建一个仅支持外部转发的加密隧道处理器。
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
func (h *Handler) UpdateKey(newKey []byte) {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	h.oldKey = h.primaryKey
	h.primaryKey = newKey
}

// resolveKey 从请求体解析 metadata 帧并尝试所有可用密钥解密。
func (h *Handler) resolveKey(r io.Reader) ([]byte, []byte, error) {
	encMeta, err := readEncMeta(r)
	if err != nil {
		return nil, nil, err
	}

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

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(h.primaryKey) == 0 {
		h.logger.Warn("隧道密钥为空，拒绝请求")
		w.WriteHeader(http.StatusForbidden)
		return
	}

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

	bodyPr, bodyPw := io.Pipe()
	go func() {
		_, decErr := DecryptStream(resolvedKey, r.Body, bodyPw)
		bodyPw.CloseWithError(decErr)
	}()

	if h.localHandler != nil && isRelativePath(req.URL) {
		h.dispatchLocal(w, r, &req, bodyPr)
	} else {
		h.forwardExternal(w, r, &req, bodyPr)
	}
}

// dispatchLocal 将加密请求路由到本地 handler。
func (h *Handler) dispatchLocal(w http.ResponseWriter, r *http.Request, req *Request, body io.Reader) {
	localReq, err := http.NewRequestWithContext(r.Context(), req.Method, req.URL, body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for k, v := range req.Headers {
		localReq.Header.Set(k, v)
	}

	bodyPr, bodyPw := io.Pipe()
	sr := newStreamRecorder(bodyPw)

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
		h.keyMu.RLock()
		encKey := h.primaryKey
		h.keyMu.RUnlock()
		metaFrame, _ := encodeMetadataFrame(encKey, respMetaJSON)

		w.Header().Set(headerContentType, frameContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(metaFrame)
		_, _ = EncryptStream(encKey, bodyPr, w)
	}()

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

// forwardExternal 将加密请求转发到外部目标 URL。
func (h *Handler) forwardExternal(w http.ResponseWriter, r *http.Request, req *Request, body io.Reader) {
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
		h.keyMu.RLock()
		encKey := h.primaryKey
		h.keyMu.RUnlock()
		errMetaFrame, _ := encodeMetadataFrame(encKey, errMetaJSON)
		w.Header().Set(headerContentType, frameContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(errMetaFrame)
		if _, err = EncryptStream(encKey, strings.NewReader(err.Error()), w); err != nil {
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
	h.keyMu.RLock()
	encKey := h.primaryKey
	h.keyMu.RUnlock()
	metaFrame, err := encodeMetadataFrame(encKey, respMetaJSON)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set(headerContentType, frameContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(metaFrame)
	if _, err := EncryptStream(encKey, resp.Body, w); err != nil {
		h.logger.Error("隧道响应加密失败", "error", err)
	}
}

// Client 是加密隧道客户端。
type Client struct {
	Key        []byte
	TunnelURL  string
	HTTPClient *http.Client
	logger     *slog.Logger
}

// NewClient 创建一个加密隧道客户端。
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
func (c *Client) Do(req *http.Request) (*http.Response, error) {
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

	combined := io.MultiReader(bytes.NewReader(metaFrame), pr)
	tunnelReq, err := http.NewRequestWithContext(req.Context(), "POST", c.TunnelURL, combined)
	if err != nil {
		pr.Close()
		return nil, fmt.Errorf("create tunnel request: %w", err)
	}
	tunnelReq.Header.Set(headerContentType, frameContentType)
	httpResp, err := c.HTTPClient.Do(tunnelReq)
	if err != nil {
		return nil, fmt.Errorf("post request: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		return nil, fmt.Errorf("tunnel error (HTTP %d): %s", httpResp.StatusCode, string(errBody))
	}

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
