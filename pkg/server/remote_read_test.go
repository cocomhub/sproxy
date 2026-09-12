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

	"github.com/cocomhub/sproxy/pkg/files"
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

// remoteReadTestConfigACL 构造「单卷 main + 给定 ACL」的已校验 cfg（不写入任何文件）。
// 供需要自定义 ACL 的用例（如让请求方 owner 也能过 ACL，见 F1 用例）复用装配逻辑。
func remoteReadTestConfigACL(t *testing.T, acl *VolumeACLConfig) *Config {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(t.TempDir(), "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot, ACL: acl}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config 非法: %v", err)
	}
	return cfg
}

// writeRemoteUserFile 在卷根上写入 <owner>/user/<rel>（自动建中间目录）。
func writeRemoteUserFile(t *testing.T, cfg *Config, owner, rel, body string) {
	t.Helper()
	abs := filepath.Join(cfg.StorageRoot, owner, "user", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// remoteReadTestConfig 构造「单卷 + 一条 mesh_readers + 磁盘上 docs/hello.txt」的已校验 cfg。
func remoteReadTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := remoteReadTestConfigACL(t, &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testReaderOwner},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testReaderOwner,
		}},
	})
	writeRemoteUserFile(t, cfg, testReaderOwner, testReaderRel, testReaderBody)
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
	var lr files.ListResponse
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

// TestRemoteRead_PathNormalizationSymmetric 钉住路径归一化的一致性（审查 F6）：
// /docs、//docs、///docs 必须等价。只剥一层斜杠会让残留首斜杠被 ValidateFilePath 判为
// 绝对路径而 400——语义分叉。归一化只做归一，穿越仍被拒（见 PathTraversalRejected）。
//
// list 与 download 分开断言，因为两者掩盖程度不同（实测）：listFiles 自身会
// TrimPrefix 一个斜杠，故对 list 而言 //docs 是「被掩盖」的——把 TrimLeft 改回
// TrimPrefix 后 //docs 仍 200，只有 ///docs 变红；download 侧无此掩盖，// 前缀直接
// 变红。两处一起断言，避免整条归一化约束被某一侧的掩盖行为放行。
func TestRemoteRead_PathNormalizationSymmetric(t *testing.T) {
	h, _, _ := newRemoteReadFixture(t, testReaderFP)

	for _, p := range []string{"//docs", "///docs"} {
		rec := doRemote(t, h, http.MethodGet, "/remote/list?volume=main&path="+url.QueryEscape(p))
		if rec.Code != http.StatusOK {
			t.Fatalf("list path=%q 应与 /docs 等价（应 200）, got %d body=%s", p, rec.Code, rec.Body.String())
		}
		var lr files.ListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &lr); err != nil {
			t.Fatalf("list path=%q 响应非法 JSON: %v", p, err)
		}
		if len(lr.Files) != 1 || lr.Files[0].Name != "hello.txt" {
			t.Fatalf("list path=%q 结果应与 /docs 一致: %+v", p, lr.Files)
		}
	}

	rec := doRemote(t, h, http.MethodGet, "/remote/download?volume=main&path="+url.QueryEscape("//"+testReaderRel))
	if rec.Code != http.StatusOK {
		t.Fatalf("download path=//%s 应与 /%s 等价（应 200）, got %d", testReaderRel, testReaderRel, rec.Code)
	}
	if rec.Body.String() != testReaderBody {
		t.Fatalf("download 双斜杠内容不符: %q", rec.Body.String())
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
		{"已授权但文件不存在", testReaderFP, "/remote/download?volume=main&path=/nope.txt", http.StatusNotFound},
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

// TestRemoteRead_UnassembledVolSetIs500 钉住「服务端装配错误」与「对象不存在」的区分（审查 F5）：
// volSet 未装配是服务端自身错误，必须回 500 而非 404，否则冒充「不存在」会误导排障；
// 审计仍记 result=error。RegisterRoutes 装配失败会 panic，故此处手工构造零值 Handlers
// （该分支生产不可达，是防御性拒绝）。
func TestRemoteRead_UnassembledVolSetIs500(t *testing.T) {
	auditBuf := &bytes.Buffer{}
	h := &Handlers{auditLogger: slog.New(slog.NewJSONHandler(auditBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	if h.volSet != nil {
		t.Fatal("前提不成立：零值 Handlers 的 volSet 应为 nil")
	}
	rh := h.newRemoteReadHandler(fakePeerFingerprint{fp: testReaderFP})
	rec := doRemote(t, rh, http.MethodGet, "/remote/list?volume=main&path=/docs")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("卷集合未装配应 500（服务端错误，非 404）, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(auditBuf.String(), `"result":"error"`) {
		t.Fatalf("该路径审计应记 result=error: %s", auditBuf.String())
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
	// 写路径的既有路由（如 /upload）不在远程面上：只读白名单是手写的，未注册即不存在，
	// 故必然是 404（405 的牙已由上方写方法用例提供，此处不重复）。
	if rec := doRemote(t, h, http.MethodPost, "/upload"); rec.Code != http.StatusNotFound {
		t.Fatalf("/upload 不应出现在远程面（应 404）, got %d", rec.Code)
	}
}

func TestRemoteRead_PathTraversalRejected(t *testing.T) {
	h, _, _ := newRemoteReadFixture(t, testReaderFP)
	// 末两项专钉「路径归一化没有吃掉 ..」：TrimLeft 只剥前导斜杠，`..` 仍须被
	// ValidateFilePath 拒（归一化不得成为穿越的旁路）。
	for _, p := range []string{"/../../etc/passwd", "/docs/../../secret", "../../etc", "/a\x00b", "/../etc", "//../etc"} {
		rec := doRemote(t, h, http.MethodGet, "/remote/download?volume=main&path="+url.QueryEscape(p))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("路径 %q 应 400, got %d", p, rec.Code)
		}
	}
}

const testBobOwner = "bob"

const testBobBody = "bob-secret"

// TestRemoteRead_OwnerFromConfigNotRequest 钉住红线：owner 恒由配置（mesh_readers 条目）
// 决定，请求参数里带的 owner/actor 一律无效。
//
// 关键前提（否则本用例无牙）：bob 也必须能过卷 main 的 ACL。若 bob 被 ACL 拒，那么即便
// 实现真的错误地取用请求参数 owner=bob，内层 locateForRead 也会因 ACL 失败直接 404——
// 「读不到 bob-secret」在任何实现下都成立，断言恒真。故此处显式把 bob 列入白名单，
// 使「owner 只能来自配置」成为**唯一**的阻止条件，并断言 404 + body 不含密文。
func TestRemoteRead_OwnerFromConfigNotRequest(t *testing.T) {
	cfg := remoteReadTestConfigACL(t, &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testReaderOwner, testBobOwner},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testReaderOwner,
		}},
	})
	writeRemoteUserFile(t, cfg, testBobOwner, "secret.txt", testBobBody)

	auditBuf := &bytes.Buffer{}
	h := newRemoteReadHandlers(t, cfg, auditBuf)
	// 前提自检：bob 确实能过本卷 ACL——「读不到」不可能被 ACL 兜底掩盖。
	vol, ok := h.volSet.ByName("main")
	if !ok || !vol.Authorize(testBobOwner) {
		t.Fatal("前提不成立：bob 应能过卷 main 的 ACL（否则本用例恒真无牙）")
	}
	// 前提自检：bob 的文件确实在盘上、且内容可辨识。
	if raw, err := os.ReadFile(filepath.Join(cfg.StorageRoot, testBobOwner, "user", "secret.txt")); err != nil || string(raw) != testBobBody {
		t.Fatalf("前提不成立：bob 的文件应存在且为 %q, err=%v", testBobBody, err)
	}

	rh := h.newRemoteReadHandler(fakePeerFingerprint{fp: testReaderFP})
	rec := doRemote(t, rh, http.MethodGet,
		"/remote/download?volume=main&path=/secret.txt&owner="+testBobOwner+"&actor="+testBobOwner)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("请求参数 owner 必须被忽略（应 404）, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), testBobBody) {
		t.Fatalf("body 不得含 bob 的文件内容: %s", rec.Body.String())
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
