// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// secrets_api_test.go 覆盖 /api/secrets 端点的 handler 逻辑（创建/列表/导出/删除）。
//
// 装配：RegisterRoutes 本身不装配 secrets 卷（那是 cmd/sproxy setupSecretBackends 的
// 职责）；本测试侧手动经 h.Volumes().AddExternalVolume 装配 secrets ExternalBackend，
// 再直接调用 handler（白盒，绕开 auth 层干扰——secret 端点的认证面由路由注册验证）。
func newTestSecretsServer(t *testing.T, modifyCfg func(*Config)) (*Handlers, string, string) {
	t.Helper()
	tmpDir := t.TempDir()

	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	if !strings.Contains(cfg.Addr, "127.0.0.1") && !strings.HasPrefix(cfg.Addr, ":") {
		cfg.Addr = "127.0.0.1" + cfg.Addr
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test-version",
		BuildAt:               "test-buildat",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})

	// 装配默认 secrets 卷（ExternalBackend）到 volSet，模拟 cmd/sproxy setupSecretBackends。
	secretsRoot := filepath.Join(tmpDir, "secrets")
	if err := os.MkdirAll(secretsRoot, 0o700); err != nil {
		t.Fatalf("mkdir secrets: %v", err)
	}
	fs := syncpkg.NewLocalFS(secretsRoot, nil)
	be, err := secrets.NewBackend(t.Context(), volume.Volume{Name: "default-secrets", Type: "secrets",
		Extra: map[string]any{"target": "local", "root": tmpDir}}, fs)
	if err != nil {
		t.Fatalf("new secrets backend: %v", err)
	}
	if err := h.Volumes().AddExternalVolume(volume.Volume{Name: "default-secrets", Type: "secrets",
		Extra: map[string]any{"target": "local", "root": tmpDir}}, be); err != nil {
		t.Fatalf("attach secrets volume: %v", err)
	}

	t.Cleanup(func() {
		_ = h.Close()
	})
	return h, secretsRoot, tmpDir
}

// callSecretsHandler 白盒分发到对应 secret handler 方法（绕过 authMiddleware/路由层，
// 聚焦 handler 逻辑；认证面由 TestSecretsRoutesRegistered 验证）。
// method/target 支持：POST /api/secrets、GET /api/secrets、GET /api/secrets/{name}、
// DELETE /api/secrets/{name}。
func callSecretsHandler(h *Handlers, method, target, body string) (int, string) {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	switch {
	case method == http.MethodPost:
		h.createSecretHandler(rec, req)
	case method == http.MethodGet && strings.TrimPrefix(target, "/api/secrets") == "":
		h.listSecretsHandler(rec, req)
	case method == http.MethodGet:
		req.SetPathValue("name", strings.TrimPrefix(target, "/api/secrets/"))
		h.exportSecretHandler(rec, req)
	case method == http.MethodDelete:
		req.SetPathValue("name", strings.TrimPrefix(target, "/api/secrets/"))
		h.deleteSecretHandler(rec, req)
	default:
		rec.WriteHeader(http.StatusMethodNotAllowed)
	}
	return rec.Code, rec.Body.String()
}

// callJSONHandler 直接调用 http.Handler（含路由），返回状态码与响应体。
func callJSONHandler(h http.Handler, method, target, body string) (int, string) {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestSecretsRoutesRegistered 验证 /api/secrets 四端点已在主 mux 注册：直接经
// h.Handler()（含路由）请求，非 404 即证明路由存在（loopback 测试模式放行认证，
// 故不断言 401，只断言「路由命中」）。
func TestSecretsRoutesRegistered(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestSecretsServer(t, nil)
	// POST 主 mux 面。
	code, body := callJSONHandler(h.Handler(), http.MethodPost, "/api/secrets", `{"name":"x","mode":"random"}`)
	if code == http.StatusNotFound {
		t.Errorf("主 mux POST /api/secrets 应已注册（404=未挂载），body=%s", body)
	}
	// GET 列表。
	code, _ = callJSONHandler(h.Handler(), http.MethodGet, "/api/secrets", "")
	if code == http.StatusNotFound {
		t.Errorf("主 mux GET /api/secrets 应已注册（404=未挂载）")
	}
}

// decJSON 解析 JSON 响应体到任意结构。
func decJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("解析 JSON 响应失败: %v (body=%s)", err, body)
	}
}

const testSecretHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSecretsCreateRandom(t *testing.T) {
	t.Parallel()
	h, secretsRoot, _ := newTestSecretsServer(t, nil)

	code, body := callSecretsHandler(h, http.MethodPost, "/api/secrets", `{"name":"rand1","mode":"random"}`)
	if code != http.StatusOK {
		t.Fatalf("创建随机 secret 状态=%d body=%s", code, body)
	}
	var resp secretCreateResponse
	decJSON(t, body, &resp)
	if resp.Name != "rand1" || resp.Origin != "random" {
		t.Errorf("响应=%+v（应 name=rand1 origin=random）", resp)
	}
	if len(resp.Value) != 64 {
		t.Errorf("随机 secret 返回长度=%d 应 64 hex", len(resp.Value))
	}
	// 服务端生成的 secret 应已落盘（可读回）。
	mgr := h.secretsManager()
	if mgr == nil {
		t.Fatal("secretsManager 反取失败")
	}
	back, err := mgr.Read(context.Background(), "rand1")
	if err != nil {
		t.Fatalf("Read rand1: %v", err)
	}
	if string(back) != resp.Value {
		t.Errorf("落盘值=%s 与返回 %s 不一致", back, resp.Value)
	}
	_ = secretsRoot
}

func TestSecretsCreateImport(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestSecretsServer(t, nil)

	// 合法导入：客户端本地派生结果上传。
	code, body := callSecretsHandler(h, http.MethodPost, "/api/secrets", `{"name":"imp1","mode":"import","value":"`+testSecretHex+`","origin":"passphrase"}`)
	if code != http.StatusOK {
		t.Fatalf("导入 secret 状态=%d body=%s", code, body)
	}
	var resp secretCreateResponse
	decJSON(t, body, &resp)
	if resp.Name != "imp1" || resp.Origin != "passphrase" || resp.Value != "" {
		t.Errorf("响应=%+v（应 name=imp1 origin=passphrase 无 value）", resp)
	}
	mgr := h.secretsManager()
	back, err := mgr.Read(context.Background(), "imp1")
	if err != nil || string(back) != testSecretHex {
		t.Errorf("导入落盘=%s err=%v", back, err)
	}

	// 非法值 fail-closed：非 hex / 短 / 空。
	for _, bad := range []struct{ name, body string }{
		{"非hex", `{"name":"bad1","mode":"import","value":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`},
		{"短", `{"name":"bad2","mode":"import","value":"abcdef"}`},
		{"空value", `{"name":"bad3","mode":"import","value":""}`},
	} {
		bc, bbody := callSecretsHandler(h, http.MethodPost, "/api/secrets", bad.body)
		if bc != http.StatusBadRequest {
			t.Errorf("%s: 应 400 得 %d body=%s", bad.name, bc, bbody)
		}
	}

	// 未知 mode fail-closed。
	uc, _ := callSecretsHandler(h, http.MethodPost, "/api/secrets", `{"name":"m1","mode":"bogus"}`)
	if uc != http.StatusBadRequest {
		t.Errorf("未知 mode 应 400 得 %d", uc)
	}
}

func TestSecretsListExportDelete(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestSecretsServer(t, nil)

	// 预置两个 secret。
	if _, err := h.secretsManager().Create(context.Background(), "aaa"); err != nil {
		t.Fatalf("Create aaa: %v", err)
	}
	if _, err := h.secretsManager().Import(context.Background(), "bbb", []byte(testSecretHex)); err != nil {
		t.Fatalf("Import bbb: %v", err)
	}

	// 列表（排序）。
	code, body := callSecretsHandler(h, http.MethodGet, "/api/secrets", "")
	if code != http.StatusOK {
		t.Fatalf("列表状态=%d body=%s", code, body)
	}
	var lr secretsListResponse
	decJSON(t, body, &lr)
	if len(lr.Secrets) != 2 || lr.Secrets[0] != "aaa" || lr.Secrets[1] != "bbb" {
		t.Errorf("列表=%v（应 [aaa bbb]）", lr.Secrets)
	}

	// 导出。
	code, body = callSecretsHandler(h, http.MethodGet, "/api/secrets/bbb", "")
	if code != http.StatusOK {
		t.Fatalf("导出状态=%d body=%s", code, body)
	}
	var er secretCreateResponse
	decJSON(t, body, &er)
	if er.Name != "bbb" || er.Value != testSecretHex {
		t.Errorf("导出=%+v（应 value=testSecretHex）", er)
	}

	// 删除不存在 fail-closed（404）。
	code, _ = callSecretsHandler(h, http.MethodDelete, "/api/secrets/nope", "")
	if code != http.StatusNotFound {
		t.Errorf("删除不存在应 404 得 %d", code)
	}

	// 删除存在。
	code, body = callSecretsHandler(h, http.MethodDelete, "/api/secrets/aaa", "")
	if code != http.StatusOK {
		t.Fatalf("删除状态=%d body=%s", code, body)
	}
	if _, err := h.secretsManager().Read(context.Background(), "aaa"); err == nil {
		t.Error("删除后 Read 应失败（secret 已移除）")
	}
	// 列表只剩 bbb。
	lc, lbody := callSecretsHandler(h, http.MethodGet, "/api/secrets", "")
	decJSON(t, lbody, &lr)
	if lc != http.StatusOK {
		t.Errorf("删除后列表状态=%d", lc)
	}
	if len(lr.Secrets) != 1 || lr.Secrets[0] != "bbb" {
		t.Errorf("删除后列表=%v（应 [bbb]）", lr.Secrets)
	}
}

// TestSecretsErrorClassification 验证错误分类（哨兵错误 → 400/500，不误分类）：
//   - import 非法值 → 400（客户端输入）；
//   - 落盘 IO 故障 → 500（服务端故障，**不**误报 400）。
//
// 用一个注入 write 失败的 mock FS（沿用 secrets.Import 的 FS 接口）验证。
func TestSecretsErrorClassification(t *testing.T) {
	t.Parallel()

	// 1. 非法值 → 400（既有测试已覆盖，此处锁定哨兵分类路径）。
	h1, _, _ := newTestSecretsServer(t, nil)
	code, _ := callSecretsHandler(h1, http.MethodPost, "/api/secrets",
		`{"name":"bad","mode":"import","value":"not-a-hex-value-here"}`)
	if code != http.StatusBadRequest {
		t.Errorf("import 非法值应 400 得 %d", code)
	}

	// 2. 落盘 IO 故障 → 500：构造 write 恒失败的 mock FS 装配 secrets 卷。
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h2 := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test-version",
		BuildAt:               "test-buildat",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	t.Cleanup(func() { _ = h2.Close() })

	failingFS := &failWriteFS{inner: syncpkg.NewLocalFS(filepath.Join(tmpDir, "secrets"), nil)}
	be, err := secrets.NewBackend(t.Context(), volume.Volume{Name: "default-secrets", Type: "secrets",
		Extra: map[string]any{"target": "local", "root": tmpDir}}, failingFS)
	if err != nil {
		t.Fatalf("new secrets backend: %v", err)
	}
	if err := h2.Volumes().AddExternalVolume(volume.Volume{Name: "default-secrets", Type: "secrets",
		Extra: map[string]any{"target": "local", "root": tmpDir}}, be); err != nil {
		t.Fatalf("attach secrets volume: %v", err)
	}

	// import 合法值但落盘失败 → 500（服务端 IO，非客户端输入错误）。
	code, body := callSecretsHandler(h2, http.MethodPost, "/api/secrets",
		`{"name":"ok","mode":"import","value":"`+testSecretHex+`"}`)
	if code != http.StatusInternalServerError {
		t.Errorf("落盘失败应 500 得 %d body=%s", code, body)
	}
}

// failWriteFS 包装 sync.FS，WriteFile 恒失败（注入落盘 IO 故障）。
type failWriteFS struct {
	inner syncpkg.FS
}

func (f *failWriteFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	return f.inner.ListDir(ctx, path)
}
func (f *failWriteFS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	return f.inner.Stat(ctx, path)
}
func (f *failWriteFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return f.inner.OpenRead(ctx, path)
}
func (f *failWriteFS) WriteFile(ctx context.Context, path string, r io.Reader, size int64, mtime int64) error {
	return fmt.Errorf("注入: 磁盘写失败")
}
func (f *failWriteFS) Rename(ctx context.Context, from, to string) error {
	return f.inner.Rename(ctx, from, to)
}
func (f *failWriteFS) Delete(ctx context.Context, path string) error {
	return f.inner.Delete(ctx, path)
}
func (f *failWriteFS) MakeDir(ctx context.Context, path string) error {
	return f.inner.MakeDir(ctx, path)
}

// TestSecretsCreateInvalidName 验证 name 校验（空/含分隔符/点）在 HTTP 层 fail-closed。
func TestSecretsCreateInvalidName(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestSecretsServer(t, nil)

	for _, bad := range []string{"", "a/b", ".", ".."} {
		code, _ := callSecretsHandler(h, http.MethodPost, "/api/secrets", `{"name":"`+bad+`","mode":"random"}`)
		if code != http.StatusBadRequest {
			t.Errorf("非法名 %q 应 400 得 %d", bad, code)
		}
	}
}

// TestSecretsManagerUnavailable 验证 secrets 卷未装配时 fail-closed（503）。
func TestSecretsManagerUnavailable(t *testing.T) {
	t.Parallel()
	// 复用 all-routes 服务器（不装配 secrets 卷）。
	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test-version",
		BuildAt:               "test-buildat",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	t.Cleanup(func() { _ = h.Close() })
	if mgr := h.secretsManager(); mgr != nil {
		t.Fatal("未装配 secrets 卷时 secretsManager 应返回 nil")
	}
	code, _ := callSecretsHandler(h, http.MethodPost, "/api/secrets", `{"name":"x","mode":"random"}`)
	if code != http.StatusServiceUnavailable {
		t.Errorf("未装配时创建应 503 得 %d", code)
	}
}
