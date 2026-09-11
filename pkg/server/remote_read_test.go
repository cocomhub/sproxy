// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

const (
	testReaderNodeA = "nodeA"
	testReaderOwner = "alice"
	testReaderRel   = "docs/hello.txt"
	testReaderBody  = "hello-remote-read\n"
)

// fakePeerFingerprint 是 peerFingerprintProvider 的测试实现（伪造已认证对端指纹）。
type fakePeerFingerprint struct{ fp string }

func (f fakePeerFingerprint) PeerFingerprint() string { return f.fp }

// remoteReadTestConfig 构造「单卷 + 一条 mesh_readers + 磁盘上 docs/hello.txt」的已校验 cfg。
func remoteReadTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(t.TempDir(), "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Volumes = []VolumeConfig{{
		Name: "main", Root: cfg.StorageRoot,
		ACL: &VolumeACLConfig{
			Mode:   VolumeACLAllow,
			Owners: []string{testReaderOwner},
			MeshReaders: []VolumeMeshReaderConfig{{
				Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testReaderOwner,
			}},
		},
	}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config 非法: %v", err)
	}
	userDir := filepath.Join(cfg.StorageRoot, testReaderOwner, "user", "docs")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StorageRoot, testReaderOwner, "user", filepath.FromSlash(testReaderRel)), []byte(testReaderBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// newRemoteReadHandlers 用给定 cfg 装配 *Handlers（审计 JSON 写入 auditBuf）。
// T5 的 in-process 中测复用本函数（同包），避免重复装配逻辑。
func newRemoteReadHandlers(t *testing.T, cfg *Config, auditBuf *bytes.Buffer) *Handlers {
	t.Helper()
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(cfg)
	opts := defaultNoAuthRegOpts() // 既有测试辅助（server_test_common_test.go:34）
	opts.Mux = http.NewServeMux()
	opts.CfgPtr = cfgPtr
	opts.Version = "test"
	opts.BuildAt = "test"
	opts.Logger = testLogger()
	opts.AuditLogger = slog.New(slog.NewJSONHandler(auditBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() }) // (*Handlers).Close 返回 error，t.Cleanup 需 func()
	return h
}

// newRemoteReadFixture 返回 (只读面 handler, cfg, 审计缓冲)。
func newRemoteReadFixture(t *testing.T, peerFP string) (http.Handler, *Config, *bytes.Buffer) {
	t.Helper()
	cfg := remoteReadTestConfig(t)
	auditBuf := &bytes.Buffer{}
	h := newRemoteReadHandlers(t, cfg, auditBuf)
	return h.newRemoteReadHandler(fakePeerFingerprint{fp: peerFP}), cfg, auditBuf
}

func doRemote(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRemoteRead_AuthorizedListAndDownload(t *testing.T) {
	h, _, _ := newRemoteReadFixture(t, testReaderFP)

	rec := doRemote(t, h, http.MethodGet, "/remote/list?volume=main&path=/docs")
	if rec.Code != http.StatusOK {
		t.Fatalf("list 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var lr listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &lr); err != nil {
		t.Fatalf("list 响应非法 JSON: %v", err)
	}
	if len(lr.Files) != 1 || lr.Files[0].Name != "hello.txt" || lr.Files[0].Volume != "main" {
		t.Fatalf("list 结果不符: %+v", lr.Files)
	}

	rec = doRemote(t, h, http.MethodGet, "/remote/download?volume=main&path=/"+testReaderRel)
	if rec.Code != http.StatusOK {
		t.Fatalf("download 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != testReaderBody {
		t.Fatalf("下载内容不符: %q", got)
	}

	rec = doRemote(t, h, http.MethodHead, "/remote/stat?volume=main&path=/"+testReaderRel)
	if rec.Code != http.StatusOK {
		t.Fatalf("stat 应 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-File-Size"); got != strconv.Itoa(len(testReaderBody)) {
		t.Fatalf("X-File-Size 不符: %q", got)
	}
}

func TestRemoteRead_AuthorizationMatrix(t *testing.T) {
	cases := []struct {
		name     string
		peerFP   string
		target   string
		wantCode int
	}{
		{"未认证指纹（空）", "", "/remote/list?volume=main&path=/docs", http.StatusUnauthorized},
		{"指纹未列入 mesh_readers", "sha256:" + strings.Repeat("b", 64), "/remote/list?volume=main&path=/docs", http.StatusNotFound},
		{"卷不存在", testReaderFP, "/remote/list?volume=nope&path=/docs", http.StatusNotFound},
		{"缺 volume 参数", testReaderFP, "/remote/list?path=/docs", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newRemoteReadFixture(t, tc.peerFP)
			if rec := doRemote(t, h, http.MethodGet, tc.target); rec.Code != tc.wantCode {
				t.Fatalf("want %d, got %d body=%s", tc.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRemoteRead_MeshReaderForAloneIsInsufficient 钉住双重约束（上一块审查发现的脚枪）：
// MeshReaderFor 只做指纹反查，可能返回一个 owner 已被本卷 ACL 拉黑的绑定条目。
// 若拿它当唯一授权依据，持有效指纹的对端就能读到 ACL 外 owner 的命名空间。
//
// 本用例先确立前提（MeshReaderFor 确实命中该非法绑定），再断言双重约束拦下它。
func TestRemoteRead_MeshReaderForAloneIsInsufficient(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = filepath.Join(t.TempDir(), "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	// mode=allow 白名单只列 alice；mesh_readers 却把 mallory 绑给同一指纹。
	cfg.Volumes = []VolumeConfig{{
		Name: "main", Root: cfg.StorageRoot,
		ACL: &VolumeACLConfig{
			Mode:   VolumeACLAllow,
			Owners: []string{testReaderOwner},
			MeshReaders: []VolumeMeshReaderConfig{{
				Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: "mallory",
			}},
		},
	}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config 非法: %v", err)
	}

	auditBuf := &bytes.Buffer{}
	h := newRemoteReadHandlers(t, cfg, auditBuf)
	vol, ok := h.volSet.ByName("main")
	if !ok {
		t.Fatal("卷 main 未装配")
	}

	// 前提：MeshReaderFor 命中（它不施加第二重约束）——这正是脚枪本身。
	mr, ok := vol.MeshReaderFor(testReaderFP)
	if !ok || mr.Owner != "mallory" || mr.Node != testReaderNodeA {
		t.Fatalf("前提不成立：MeshReaderFor 应命中 mallory 绑定, got %+v ok=%v", mr, ok)
	}
	// 第二重约束不满足：mallory 不在本卷 allow 白名单内。
	if vol.AuthorizeMeshRead(testReaderNodeA, testReaderFP, "mallory") {
		t.Fatal("AuthorizeMeshRead 不应放行 ACL 白名单外的 owner")
	}

	rh := h.newRemoteReadHandler(fakePeerFingerprint{fp: testReaderFP})
	rec := doRemote(t, rh, http.MethodGet, "/remote/list?volume=main&path=/docs")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ACL 外 owner 的绑定应 404（不泄命名空间）, got %d body=%s", rec.Code, rec.Body.String())
	}
	// 状态码本身不足以钉住本约束（实测：把 AuthorizeMeshRead 短路掉，请求会被委派给
	// 既有 listFiles，其自身的卷 ACL 检查同样产出 404）。故断言审计的**结果与原因**：
	// 授权层拒绝落 "denied"，而委派后的不存在落 "error"。
	lines := auditBuf.String()
	if !strings.Contains(lines, `"result":"denied"`) {
		t.Fatalf("应由授权层拒绝（result=denied），而非委派后失败: %s", lines)
	}
	if !strings.Contains(lines, "授权三元组未通过") {
		t.Fatalf("审计应记录拒绝原因（双重约束未满足）: %s", lines)
	}
}

func TestRemoteRead_WriteMethodsRejected(t *testing.T) {
	h, _, _ := newRemoteReadFixture(t, testReaderFP)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if rec := doRemote(t, h, m, "/remote/list?volume=main&path=/docs"); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s 应 405（只读白名单）, got %d", m, rec.Code)
		}
	}
	// 写路径的既有路由（如 /upload）不在远程面上。
	if rec := doRemote(t, h, http.MethodPost, "/upload"); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Fatalf("/upload 不应出现在远程面, got %d", rec.Code)
	}
}

func TestRemoteRead_PathTraversalRejected(t *testing.T) {
	h, _, _ := newRemoteReadFixture(t, testReaderFP)
	for _, p := range []string{"/../../etc/passwd", "/docs/../../secret", "../../etc", "/a\x00b"} {
		rec := doRemote(t, h, http.MethodGet, "/remote/download?volume=main&path="+url.QueryEscape(p))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("路径 %q 应 400, got %d", p, rec.Code)
		}
	}
}

func TestRemoteRead_OwnerFromConfigNotRequest(t *testing.T) {
	h, cfg, _ := newRemoteReadFixture(t, testReaderFP)
	// 磁盘上再放一个 bob 的文件，请求方试图用 owner 参数越权。
	bobDir := filepath.Join(cfg.StorageRoot, "bob", "user")
	if err := os.MkdirAll(bobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bobDir, "secret.txt"), []byte("bob-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRemote(t, h, http.MethodGet, "/remote/download?volume=main&path=/secret.txt&owner=bob&actor=bob")
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "bob-secret") {
		t.Fatal("owner 必须由配置决定，不接受请求参数指定（越权枚举）")
	}
}

// TestRemoteRead_LargeFileStreamsWithoutBuffering 用超过 tunnel 分块帧（64 KiB）的载荷
// 验证大文件经「授权 → 委派 → remoteStatusWriter 包装」后仍字节精确地完整回传
// （逐字节 SHA-256 比对，而非只看状态码）。
//
// 说明：httptest.ResponseRecorder 自身会缓冲全部响应体，故本用例**不能**证明服务端
// 「未把文件整体读入内存」——它钉住的是端到端的字节保真度；真正的流式行为由 T5/T6
// 的隧道链路验证。
func TestRemoteRead_LargeFileStreamsWithoutBuffering(t *testing.T) {
	h, cfg, _ := newRemoteReadFixture(t, testReaderFP)
	const size = 6 << 20 // 6 MiB > chunk 帧 64 KiB
	big := bytes.Repeat([]byte("x"), size)
	if err := os.WriteFile(filepath.Join(cfg.StorageRoot, testReaderOwner, "user", "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doRemote(t, h, http.MethodGet, "/remote/download?volume=main&path=/big.bin")
	if rec.Code != http.StatusOK {
		t.Fatalf("大文件下载应 200, got %d", rec.Code)
	}
	if rec.Body.Len() != size {
		t.Fatalf("字节数不符: got %d want %d", rec.Body.Len(), size)
	}
	sum := sha256.Sum256(rec.Body.Bytes())
	if got := hex.EncodeToString(sum[:]); got != testutil.SHA256Hex(big) {
		t.Fatalf("SHA-256 不符: got %s", got)
	}
}

func TestRemoteRead_RangeSupported(t *testing.T) {
	h, _, _ := newRemoteReadFixture(t, testReaderFP)
	req := httptest.NewRequest(http.MethodGet, "/remote/download?volume=main&path=/"+testReaderRel, nil)
	req.Header.Set("Range", "bytes=0-4")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range 应 206, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != testReaderBody[:5] {
		t.Fatalf("Range 内容不符: %q", got)
	}
}

func TestRemoteRead_AuditRecordsAllowAndDeny(t *testing.T) {
	h, _, auditBuf := newRemoteReadFixture(t, testReaderFP)
	doRemote(t, h, http.MethodGet, "/remote/list?volume=main&path=/docs")
	lines := auditBuf.String()
	// RecordAudit 以 slog kv 形式输出小写键（audit.go:65-74）：action/actor/mesh/.../result。
	if !strings.Contains(lines, `"action":"mesh_read"`) || !strings.Contains(lines, `"mesh":"nodeA"`) {
		t.Fatalf("放行路径应记审计（含 mesh 字段）: %s", lines)
	}

	h2, _, audit2 := newRemoteReadFixture(t, "sha256:"+strings.Repeat("b", 64))
	doRemote(t, h2, http.MethodGet, "/remote/list?volume=main&path=/docs")
	if !strings.Contains(audit2.String(), `"action":"mesh_read"`) ||
		!strings.Contains(audit2.String(), `"result":"denied"`) {
		t.Fatalf("拒绝路径也须记审计: %s", audit2.String())
	}
}
