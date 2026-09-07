// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package vaultmock 提供模拟 HashiCorp Vault Transit encrypt/decrypt 端点的
// httptest.Server（L1 单元测试与装配测试共享）。纯 stdlib，无三方依赖。
package vaultmock

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Options 是 NewServer 的构造参数。
type Options struct {
	// Token 是期望收到的 X-Vault-Token 头。非空时 mock 对每个请求断言头一致
	// （不一致 → t.Errorf）；空则不校验。
	Token string
	// DecryptTo 非 nil → decrypt 端点固定回此明文；nil → 回显请求 ciphertext 的镜像
	// （即 encrypt 产物 "vault:v1:"+base64(明文)，支持 Save→Load 全链路往返）。
	DecryptTo []byte
}

// Server 是模拟 Vault Transit encrypt/decrypt 端点的 httptest.Server。
//
//	POST /v1/{mount}/encrypt/{key} → 200 {"data":{"ciphertext":"vault:v1:"+base64(明文)}}
//	POST /v1/{mount}/decrypt/{key} → 200 {"data":{"plaintext":base64(明文)}}（镜像或 DecryptTo）
//
// 记录每次请求收到的 token / context / plaintext(base64) / ciphertext，并维护
// encrypt/decrypt 请求计数（供装配测试断言「收到 1 次 encrypt」「context 正确」）。
type Server struct {
	t     *testing.T
	token string
	srv   *httptest.Server

	mu        sync.Mutex
	decryptTo []byte
	// decrypt 错误覆写（errStatus != 0 时启用：返回 status + {"errors":[errBody]}）。
	errStatus int
	errBody   string
	// lookup-self 错误覆写（lookupStatus != 0 时启用；否则 token 匹配期望 → 200，
	// 不匹配期望（Options.Token 非空）→ 403 permission denied）。
	lookupStatus int
	lookupErr    string

	encryptCount int
	decryptCount int
	tokens       []string
	contexts     []string
	plaintexts   []string // encrypt 请求体 plaintext 字段（base64）
	ciphertexts  []string // decrypt 请求体 ciphertext 字段
}

// NewServer 创建 mock Vault server，并注册 t.Cleanup 关闭。
func NewServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s := &Server{t: t, token: opts.Token, decryptTo: opts.DecryptTo}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

// URL 返回 mock 服务地址（与真实 Vault 兼容的 http://127.0.0.1:port 基址）。
func (s *Server) URL() string { return s.srv.URL }

// EncryptCount 返回收到的 encrypt 请求总数。
func (s *Server) EncryptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encryptCount
}

// DecryptCount 返回收到的 decrypt 请求总数。
func (s *Server) DecryptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decryptCount
}

// LastToken 返回最近一次请求收到的 X-Vault-Token 头。
func (s *Server) LastToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tokens) == 0 {
		return ""
	}
	return s.tokens[len(s.tokens)-1]
}

// LastContext 返回最近一次请求收到的 context 字段值（base64 形态 AAD）。
func (s *Server) LastContext() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.contexts) == 0 {
		return ""
	}
	return s.contexts[len(s.contexts)-1]
}

// LastPlaintextB64 返回最近一次 encrypt 请求的 plaintext 字段值（base64）。
func (s *Server) LastPlaintextB64() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.plaintexts) == 0 {
		return ""
	}
	return s.plaintexts[len(s.plaintexts)-1]
}

// SetDecryptTo 固定 decrypt 明文（nil 恢复镜像行为）。
func (s *Server) SetDecryptTo(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decryptTo = b
}

// SetDecryptError 让 decrypt 端点回指定状态码 + {"errors":[msg]}（模拟 403/503/5xx）。
func (s *Server) SetDecryptError(status int, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errStatus = status
	s.errBody = msg
}

// SetLookupSelfError 让 /v1/auth/token/lookup-self 端点回指定状态码 + {"errors":[msg]}
// （模拟 token 无效/权限不足，供启动探活 fail-fast 测试）。
func (s *Server) SetLookupSelfError(status int, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupStatus = status
	s.lookupErr = msg
}

// ServeHTTP 实现 mock Vault Transit 端点。记录请求；encrypt 回 ciphertext 镜像、
// decrypt 按覆写/DecryptTo/镜像回 plaintext。token 不一致通过 t.Errorf 上报
// （handler 运行在 httptest server goroutine，禁用 t.Fatalf）。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("vaultmock: 读取请求体失败: %v", err)
		writeVaultErrors(w, http.StatusBadRequest, "bad request body")
		return
	}
	var req struct {
		Plaintext  string `json:"plaintext"`
		Ciphertext string `json:"ciphertext"`
		Context    string `json:"context"`
	}
	// lookup-self 探活 POST 无 body；仅非空 body 需 JSON 解析。非 JSON 请求体：暴露测试请求
	// 构造 bug（M-3），fail-closed 拒绝而非静默回空密文。
	if len(body) > 0 {
		if uerr := json.Unmarshal(body, &req); uerr != nil {
			s.t.Errorf("vaultmock: 请求体非法 JSON（测试请求构造 bug?）: %v", uerr)
			writeVaultErrors(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}

	token := r.Header.Get("X-Vault-Token")
	op := vaultMockOp(r.URL.Path)

	s.mu.Lock()
	s.tokens = append(s.tokens, token)
	s.contexts = append(s.contexts, req.Context)
	switch op {
	case "encrypt":
		s.encryptCount++
		s.plaintexts = append(s.plaintexts, req.Plaintext)
	case "decrypt":
		s.decryptCount++
		s.ciphertexts = append(s.ciphertexts, req.Ciphertext)
	}
	decryptTo := s.decryptTo
	errStatus := s.errStatus
	errBody := s.errBody
	lookupStatus := s.lookupStatus
	lookupErr := s.lookupErr
	s.mu.Unlock()

	if s.token != "" && token != s.token {
		s.t.Errorf("vaultmock: X-Vault-Token = %q, want %q", token, s.token)
	}

	switch op {
	case "encrypt":
		// encrypt 镜像：ciphertext = "vault:v1:" + base64(明文)（自描述，decrypt 可往返）。
		writeVaultData(w, http.StatusOK, map[string]string{"ciphertext": "vault:v1:" + req.Plaintext})
	case "decrypt":
		if errStatus != 0 {
			writeVaultErrors(w, errStatus, errBody)
			return
		}
		if decryptTo != nil {
			writeVaultData(w, http.StatusOK, map[string]string{"plaintext": base64.StdEncoding.EncodeToString(decryptTo)})
			return
		}
		pt, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(req.Ciphertext, "vault:v1:"))
		if err != nil {
			writeVaultErrors(w, http.StatusBadRequest, "bad ciphertext base64")
			return
		}
		writeVaultData(w, http.StatusOK, map[string]string{"plaintext": base64.StdEncoding.EncodeToString(pt)})
	case "lookup-self":
		// token 自查端点（启动探活）：覆写优先；否则 token 匹配期望 → 200，不匹配 → 403。
		if lookupStatus != 0 {
			writeVaultErrors(w, lookupStatus, lookupErr)
			return
		}
		if s.token != "" && token != s.token {
			writeVaultErrors(w, http.StatusForbidden, "permission denied")
			return
		}
		writeVaultData(w, http.StatusOK, map[string]string{})
	default:
		writeVaultErrors(w, http.StatusNotFound, "unknown endpoint")
	}
}

// vaultMockOp 从 URL path 识别 mock 端点（encrypt/decrypt/lookup-self），无法识别返回空串。
func vaultMockOp(path string) string {
	switch {
	case strings.Contains(path, "/encrypt/"):
		return "encrypt"
	case strings.Contains(path, "/decrypt/"):
		return "decrypt"
	case strings.Contains(path, "/auth/token/lookup-self"):
		return "lookup-self"
	}
	return ""
}

// writeVaultData 以 {"data":{...}} 形态回 JSON（对齐 Vault Transit 成功响应）。
func writeVaultData(w http.ResponseWriter, status int, data map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// writeVaultErrors 以 {"errors":[...]} 形态回错误 JSON（对齐 Vault 错误响应结构）。
func writeVaultErrors(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{msg}})
}
