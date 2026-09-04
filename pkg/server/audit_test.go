// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// ---- 测试辅助 ----

// newAuditTestServer 构建一个带 SproxySig 认证 + 可捕获审计日志的测试服务。
// 返回 base URL、cfgPtr 与审计日志 buffer。
func newAuditTestServer(t *testing.T, modifyCfg func(*Config)) (string, *atomic.Pointer[Config], *bytes.Buffer) {
	t.Helper()
	tmpDir := t.TempDir()

	cfg := Default()
	cfg.StorageRoot = tmpDir
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	// 凭据 store 化：注入带 testAccessKey 的 Ring（取代 cfg.AccessKeys）。
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	var auditBuf bytes.Buffer
	auditLogger := slog.New(slog.NewJSONHandler(&auditBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mux := http.NewServeMux()
	opts := RegisterRoutesOpts{
		Mux:         mux,
		CfgPtr:      &cfgPtr,
		Version:     "test",
		BuildAt:     "test",
		Logger:      testLogger(),
		AuditLogger: auditLogger,
	}
	withTestCreds(&opts)
	h := RegisterRoutes(t.Context(), opts)

	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	return ts.URL, &cfgPtr, &auditBuf
}

// writeUploadFile 直接在存储根写入一个文件（内容 + 返回其 sha256），
// 供 delete/rename 测试无需走完整上传链路。
// 审计测试服务配置了 access_keys（testAccessKey），请求 owner 恒为 testAccessKey，
// P2 delete/rename 已迁移到 Tenant API，故文件落在 <storageRoot>/<owner>/user/<rel>
// （新布局；与服务端 UserRel 映射一致）。
func writeUploadFile(t *testing.T, cfgPtr *atomic.Pointer[Config], name string, content []byte) {
	t.Helper()
	full := filepath.Join(cfgPtr.Load().StorageRoot, testAccessKey, "user", filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// signBodyRequest 给带 body 的请求打上合法 SproxySig 头（body_sha256 预计算）。
func signBodyRequest(r *http.Request, ak, sk string, body []byte) {
	now := time.Now()
	h := sproxysig.Header{
		Version: sproxysig.Version, AK: ak,
		TS: now.UnixMilli(), Exp: now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      testNonce(),
		BodySHA256: sha256hex(body),
	}
	h.Sig = sproxysig.Sign(sk, h, r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
	r.Header.Set("Authorization", formatSigAuth(h))
}

// uploadFileSigned 带 SproxySig 签名上传一个文件（multipart），返回状态码。
func uploadFileSigned(t *testing.T, baseURL, filename string, body []byte) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, werr := part.Write(body); werr != nil {
		t.Fatalf("write body: %v", werr)
	}
	if cerr := mw.Close(); cerr != nil {
		t.Fatalf("close multipart: %v", cerr)
	}
	req, err := http.NewRequest("POST", baseURL+"/upload", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", sha256hex(body))
	signBodyRequest(req, testAccessKey, testAccessSecret, buf.Bytes())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// auditLines 读取并解析审计 buffer 中的所有 JSON 行。
func auditLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("解析审计行失败 %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// requestAudit 发起一次带 SproxySig 签名的 GET /api/audit 请求（audit_handler 黑盒）。
func requestAudit(t *testing.T, url, query string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/api/audit"+query, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/audit: %v", err)
	}
	return resp
}

// decodeAuditResponse 解析审计响应体（复用生产 auditResponse 类型）。
func decodeAuditResponse(t *testing.T, resp *http.Response) auditResponse {
	t.Helper()
	defer resp.Body.Close()
	var got auditResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode /api/audit body: %v", err)
	}
	return got
}

// findAudit 返回第一个 action 匹配的审计行。
func findAudit(t *testing.T, buf *bytes.Buffer, action string) map[string]any {
	t.Helper()
	for _, m := range auditLines(t, buf) {
		if m["action"] == action {
			return m
		}
	}
	t.Fatalf("未找到 action=%q 的审计行，实际：%v", action, auditLines(t, buf))
	return nil
}

// ---- audit.go 单元测试 ----

func TestRecordAudit_OutputsStructuredJSON(t *testing.T) {
	var buf bytes.Buffer
	h := &Handlers{auditLogger: slog.New(slog.NewJSONHandler(&buf, nil))}
	ts := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	h.RecordAudit(context.Background(), AuditEvent{
		Action:     "delete",
		Actor:      "sk-test",
		Mesh:       "mesh-a",
		ObjectType: "file",
		Object:     "dir/a.txt",
		Result:     AuditResultSuccess,
		Detail:     "ok",
		TS:         ts,
	})

	lines := auditLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("期望 1 条审计行，实际 %d", len(lines))
	}
	m := lines[0]
	if m["action"] != "delete" {
		t.Errorf("action = %v, want delete", m["action"])
	}
	if m["actor"] != "sk-test" {
		t.Errorf("actor = %v, want sk-test", m["actor"])
	}
	if m["mesh"] != "mesh-a" {
		t.Errorf("mesh = %v, want mesh-a", m["mesh"])
	}
	if m["object_type"] != "file" {
		t.Errorf("object_type = %v, want file", m["object_type"])
	}
	if m["object"] != "dir/a.txt" {
		t.Errorf("object = %v, want dir/a.txt", m["object"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
	if m["detail"] != "ok" {
		t.Errorf("detail = %v, want ok", m["detail"])
	}
	if m["ts"] != "2026-09-01T12:00:00Z" {
		t.Errorf("ts = %v, want 2026-09-01T12:00:00Z", m["ts"])
	}
}

func TestRecordAudit_AutoActorMeshFromContext(t *testing.T) {
	var buf bytes.Buffer
	h := &Handlers{auditLogger: slog.New(slog.NewJSONHandler(&buf, nil))}
	ctx := withActor(withMesh(context.Background(), "mesh-b"), "ak-1")
	h.RecordAudit(ctx, AuditEvent{
		Action: "rename", ObjectType: "file", Object: "x", Result: AuditResultSuccess,
	})

	m := auditLines(t, &buf)[0]
	if m["actor"] != "ak-1" {
		t.Errorf("actor = %v, want ak-1", m["actor"])
	}
	if m["mesh"] != "mesh-b" {
		t.Errorf("mesh = %v, want mesh-b", m["mesh"])
	}
}

func TestRecordAudit_DefaultTS(t *testing.T) {
	var buf bytes.Buffer
	h := &Handlers{auditLogger: slog.New(slog.NewJSONHandler(&buf, nil))}
	h.RecordAudit(context.Background(), AuditEvent{
		Action: "delete", Result: AuditResultSuccess,
	})

	m := auditLines(t, &buf)[0]
	if _, ok := m["ts"]; !ok {
		t.Error("ts 字段缺失，期望自动填充当前时间")
	}
}

// ---- actor 注入 ----

func TestAuthMiddleware_SproxySigActorInjected(t *testing.T) {
	t.Parallel()
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr, noncePool: sproxysig.NewNoncePool(), credentialRing: ringForTestCreds()}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ActorFrom(r.Context()); got != testAccessKey {
			t.Errorf("ActorFrom() = %q, want %q", got, testAccessKey)
		}
		if got := MeshFrom(r.Context()); got != tunnel.AccessKeyMesh(testAccessKey) {
			t.Errorf("MeshFrom() = %q, want %q", got, tunnel.AccessKeyMesh(testAccessKey))
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/upload", nil)
	signRequest(r, testAccessKey, testAccessSecret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_APIKeyActorInjected(t *testing.T) {
	t.Parallel()
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{
		APIKeys: APIKeyConfig{
			Enabled: true,
			Keys:    []APIKey{{Name: "ops-token", Key: "mykey", Permission: "write"}},
		},
	})
	h := &Handlers{cfgPtr: cfgPtr}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ActorFrom(r.Context()); got != "ops-token" {
			t.Errorf("ActorFrom() = %q, want ops-token", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("POST", "/upload", nil)
	r.Header.Set("Authorization", "Bearer mykey")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_APIKeyActorFallsBackToKey(t *testing.T) {
	t.Parallel()
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{
		APIKeys: APIKeyConfig{
			Enabled: true,
			Keys:    []APIKey{{Key: "mykey", Permission: "write"}},
		},
	})
	h := &Handlers{cfgPtr: cfgPtr}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 安全审查 MEDIUM：Name 为空时 actor 用 key 的 SHA-256 摘要前缀（key_<12hex>），
		// **绝不把原始 API key 落日志**。
		if got := ActorFrom(r.Context()); got != "key_5e50f405ace6" {
			t.Errorf("ActorFrom() = %q, want %q（Name 为空回退摘要而非原始 key）", got, "key_5e50f405ace6")
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("POST", "/upload", nil)
	r.Header.Set("Authorization", "Bearer mykey")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_NoAuthActorEmpty(t *testing.T) {
	t.Parallel()
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr, allowInsecureLoopback: true}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ActorFrom(r.Context()); got != "" {
			t.Errorf("ActorFrom() = %q, want empty", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/upload", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ---- 敏感 handler 记审计 ----

func TestDeleteHandler_RecordsAuditSuccess(t *testing.T) {
	url, cfgPtr, auditBuf := newAuditTestServer(t, nil)
	body := []byte("delete-me")
	writeUploadFile(t, cfgPtr, "to-delete.txt", body)

	req, _ := http.NewRequest("POST", url+"/delete?filename=to-delete.txt", nil)
	req.Header.Set("X-File-Checksum", sha256hex(body))
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	m := findAudit(t, auditBuf, "delete")
	if m["actor"] != testAccessKey {
		t.Errorf("actor = %v, want %q", m["actor"], testAccessKey)
	}
	if m["mesh"] != tunnel.AccessKeyMesh(testAccessKey) {
		t.Errorf("mesh = %v, want %q", m["mesh"], tunnel.AccessKeyMesh(testAccessKey))
	}
	if m["object_type"] != "file" {
		t.Errorf("object_type = %v, want file", m["object_type"])
	}
	if m["object"] != "to-delete.txt" {
		t.Errorf("object = %v, want to-delete.txt", m["object"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
}

func TestDeleteHandler_RecordsAuditError_FileNotFound(t *testing.T) {
	url, _, auditBuf := newAuditTestServer(t, nil)

	req, _ := http.NewRequest("POST", url+"/delete?filename=no-such.txt", nil)
	req.Header.Set("X-File-Checksum", strings.Repeat("0", 64))
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	m := findAudit(t, auditBuf, "delete")
	if m["result"] != "error" {
		t.Errorf("result = %v, want error", m["result"])
	}
	if m["actor"] != testAccessKey {
		t.Errorf("actor = %v, want %q", m["actor"], testAccessKey)
	}
	if m["object"] != "no-such.txt" {
		t.Errorf("object = %v, want no-such.txt", m["object"])
	}
}

func TestDeleteHandler_RecordsAuditDenied_ChecksumMismatch(t *testing.T) {
	url, cfgPtr, auditBuf := newAuditTestServer(t, nil)
	writeUploadFile(t, cfgPtr, "keep.txt", []byte("keep"))

	req, _ := http.NewRequest("POST", url+"/delete?filename=keep.txt", nil)
	req.Header.Set("X-File-Checksum", strings.Repeat("0", 64))
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	m := findAudit(t, auditBuf, "delete")
	if m["result"] != "denied" {
		t.Errorf("result = %v, want denied", m["result"])
	}
	if m["object"] != "keep.txt" {
		t.Errorf("object = %v, want keep.txt", m["object"])
	}
}

func TestRenameHandler_RecordsAuditSuccess(t *testing.T) {
	url, cfgPtr, auditBuf := newAuditTestServer(t, nil)
	body := []byte("rename-me")
	writeUploadFile(t, cfgPtr, "old.txt", body)

	req, _ := http.NewRequest("POST", url+"/rename?from=old.txt&to=new.txt", nil)
	req.Header.Set("X-File-Checksum", sha256hex(body))
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	m := findAudit(t, auditBuf, "rename")
	if m["actor"] != testAccessKey {
		t.Errorf("actor = %v, want %q", m["actor"], testAccessKey)
	}
	if m["object_type"] != "file" {
		t.Errorf("object_type = %v, want file", m["object_type"])
	}
	if m["object"] != "old.txt" {
		t.Errorf("object = %v, want old.txt", m["object"])
	}
	if m["detail"] != "to=new.txt" {
		t.Errorf("detail = %v, want to=new.txt", m["detail"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
}

func TestConfigUpdate_RecordsAudit(t *testing.T) {
	url, _, auditBuf := newAuditTestServer(t, nil)
	body := []byte(`{"log_level":"debug"}`)

	req, _ := http.NewRequest("PUT", url+"/api/config", bytes.NewReader(body))
	signBodyRequest(req, testAccessKey, testAccessSecret, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("config update: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	m := findAudit(t, auditBuf, "config_update")
	if m["actor"] != testAccessKey {
		t.Errorf("actor = %v, want %q", m["actor"], testAccessKey)
	}
	if m["object_type"] != "config" {
		t.Errorf("object_type = %v, want config", m["object_type"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
}

func TestConfigUpdate_RecordsAuditDenied_InvalidValue(t *testing.T) {
	url, _, auditBuf := newAuditTestServer(t, nil)
	body := []byte(`{"log_level":"bogus"}`)

	req, _ := http.NewRequest("PUT", url+"/api/config", bytes.NewReader(body))
	signBodyRequest(req, testAccessKey, testAccessSecret, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("config update: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	m := findAudit(t, auditBuf, "config_update")
	if m["result"] != "denied" {
		t.Errorf("result = %v, want denied", m["result"])
	}
	if m["actor"] != testAccessKey {
		t.Errorf("actor = %v, want %q", m["actor"], testAccessKey)
	}
}

func TestVersionRestore_RecordsAudit(t *testing.T) {
	url, _, auditBuf := newAuditTestServer(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	})

	// 上传两版，产生一个版本历史
	if st := uploadFileSigned(t, url, "ver.txt", []byte("version 1")); st != http.StatusOK {
		t.Fatalf("upload v1 status = %d", st)
	}
	if st := uploadFileSigned(t, url, "ver.txt", []byte("version 2")); st != http.StatusOK {
		t.Fatalf("upload v2 status = %d", st)
	}

	// 列出版本拿到 version_id
	listReq, _ := http.NewRequest("GET", url+"/api/versions?filename=ver.txt", nil)
	signRequest(listReq, testAccessKey, testAccessSecret)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	var listResult struct {
		Versions []VersionInfo `json:"versions"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&listResult)
	listResp.Body.Close()
	if len(listResult.Versions) == 0 {
		t.Fatal("expected versions")
	}
	versionID := listResult.Versions[0].VersionID

	restoreURL := fmt.Sprintf("%s/api/versions/restore?filename=ver.txt&version_id=%d", url, versionID)
	restoreReq, _ := http.NewRequest("POST", restoreURL, nil)
	signRequest(restoreReq, testAccessKey, testAccessSecret)
	restoreResp, err := http.DefaultClient.Do(restoreReq)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	defer restoreResp.Body.Close()
	if restoreResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on restore, got %d", restoreResp.StatusCode)
	}

	m := findAudit(t, auditBuf, "version_restore")
	if m["actor"] != testAccessKey {
		t.Errorf("actor = %v, want %q", m["actor"], testAccessKey)
	}
	if m["object"] != "ver.txt" {
		t.Errorf("object = %v, want ver.txt", m["object"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
}

func TestVersionDelete_RecordsAudit(t *testing.T) {
	url, _, auditBuf := newAuditTestServer(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	})

	if st := uploadFileSigned(t, url, "delver.txt", []byte("v1")); st != http.StatusOK {
		t.Fatalf("upload v1 status = %d", st)
	}
	if st := uploadFileSigned(t, url, "delver.txt", []byte("v2")); st != http.StatusOK {
		t.Fatalf("upload v2 status = %d", st)
	}

	listReq, _ := http.NewRequest("GET", url+"/api/versions?filename=delver.txt", nil)
	signRequest(listReq, testAccessKey, testAccessSecret)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	var listResult struct {
		Versions []VersionInfo `json:"versions"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&listResult)
	listResp.Body.Close()
	if len(listResult.Versions) == 0 {
		t.Fatal("expected versions")
	}
	versionID := listResult.Versions[0].VersionID

	delURL := fmt.Sprintf("%s/api/versions?filename=delver.txt&version_id=%d", url, versionID)
	delReq, _ := http.NewRequest("DELETE", delURL, nil)
	signRequest(delReq, testAccessKey, testAccessSecret)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("delete version: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on delete version, got %d", delResp.StatusCode)
	}

	m := findAudit(t, auditBuf, "version_delete")
	if m["actor"] != testAccessKey {
		t.Errorf("actor = %v, want %q", m["actor"], testAccessKey)
	}
	if m["object"] != "delver.txt" {
		t.Errorf("object = %v, want delver.txt", m["object"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
}

func TestCloudCancelTask_RecordsAuditError(t *testing.T) {
	url, _, auditBuf := newAuditTestServer(t, nil)

	req, _ := http.NewRequest("POST", url+"/api/cloud/tasks/no-such-id/cancel", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cloud cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	m := findAudit(t, auditBuf, "cloud_cancel")
	if m["actor"] != testAccessKey {
		t.Errorf("actor = %v, want %q", m["actor"], testAccessKey)
	}
	if m["object"] != "no-such-id" {
		t.Errorf("object = %v, want no-such-id", m["object"])
	}
	if m["result"] != "error" {
		t.Errorf("result = %v, want error", m["result"])
	}
}

func TestCloudDeleteTask_RecordsAuditError(t *testing.T) {
	url, _, auditBuf := newAuditTestServer(t, nil)

	req, _ := http.NewRequest("DELETE", url+"/api/cloud/tasks/no-such-id", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cloud delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	m := findAudit(t, auditBuf, "cloud_delete")
	if m["actor"] != testAccessKey {
		t.Errorf("actor = %v, want %q", m["actor"], testAccessKey)
	}
	if m["object"] != "no-such-id" {
		t.Errorf("object = %v, want no-such-id", m["object"])
	}
	if m["result"] != "error" {
		t.Errorf("result = %v, want error", m["result"])
	}
}

// ---- requestlog 带 actor ----

func TestRequestLog_RecordsActor(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	var logBuf bytes.Buffer
	mux := http.NewServeMux()
	opts := RegisterRoutesOpts{
		Mux:         mux,
		CfgPtr:      &cfgPtr,
		Version:     "test",
		BuildAt:     "test",
		Logger:      slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		AuditLogger: testLogger(),
	}
	withTestCreds(&opts)
	h := RegisterRoutes(t.Context(), opts)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})

	req, _ := http.NewRequest("GET", ts.URL+"/api/stats", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	found := false
	for line := range strings.SplitSeq(strings.TrimSpace(logBuf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["msg"] != "请求完成" {
			continue
		}
		found = true
		if m["actor"] != testAccessKey {
			t.Errorf("请求完成日志 actor = %v, want %q", m["actor"], testAccessKey)
		}
	}
	if !found {
		t.Fatalf("未找到「请求完成」日志：%s", logBuf.String())
	}
}

// TestRenameHandler_RecordsAuditDenied_NoFalseSuccess 验证（审查 I-1 回归）：
// rename 目标已存在（409）时只记 denied 审计，**不追加假的 success 审计行**
// （原 executeRename 的 `return err` 在文件存在时 err 恰为 nil，调用方误判成功）。
func TestRenameHandler_RecordsAuditDenied_NoFalseSuccess(t *testing.T) {
	url, cfgPtr, auditBuf := newAuditTestServer(t, nil)
	body := []byte("rename-me")
	writeUploadFile(t, cfgPtr, "old.txt", body)
	writeUploadFile(t, cfgPtr, "exists.txt", []byte("target exists"))

	req, _ := http.NewRequest("POST", url+"/rename?from=old.txt&to=exists.txt", nil)
	req.Header.Set("X-File-Checksum", sha256hex(body))
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("目标已存在应返回 409，got %d", resp.StatusCode)
	}

	// 应有 denied 审计（目标已存在）。
	denied := findAudit(t, auditBuf, "rename")
	if denied["result"] != "denied" {
		t.Fatalf("目标已存在的 rename 审计应 result=denied，got %v", denied["result"])
	}
	// 不应有 success 审计行（I-1 假成功回归）。
	for _, m := range auditLines(t, auditBuf) {
		if m["action"] == "rename" && m["result"] == "success" {
			t.Fatalf("被拒绝的 rename 不应产生 success 审计行（审查 I-1 假成功）：%v", m)
		}
	}
}
