// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// ---- 本 lane 专用日志捕获（auth logger seam）----

// authLogBuffer 是并发安全的日志缓冲：被测服务端在 httptest 的 serve goroutine 中
// 写日志，与本测试 goroutine 的读取并发，需加锁（-race 下必须）。
type authLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *authLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *authLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Len 返回当前已写入字节数：缓冲只追加，故可作「请求期增量」的基线偏移。
func (b *authLogBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// captureAuthLogs 返回写入内存缓冲的 DEBUG 级 text logger 与其缓冲。
// DEBUG 级是为了让「认证路径是否落到注入 logger」可被完整观察（InfoContext 请求
// 日志也在内）。
func captureAuthLogs() (*slog.Logger, *authLogBuffer) {
	buf := &authLogBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, buf
}

// authLogWarnLines 返回捕获日志中所有 level=WARN 的行（text handler 格式）。
func authLogWarnLines(captured string) []string {
	var out []string
	for line := range strings.SplitSeq(captured, "\n") {
		if strings.Contains(line, "level=WARN") {
			out = append(out, line)
		}
	}
	return out
}

// authLogHasMessage 判断捕获日志中是否存在指定 msg 值（text handler 的 msg="..." 段）。
func authLogHasMessage(captured, msg string) bool {
	return strings.Contains(captured, "msg="+fmt.Sprintf("%q", msg))
}

// authLogUploadFile 上传 multipart 文件，返回状态码。
// 硬规则（AGENTS.md §17）：测试禁用 http.DefaultClient / 共享 DefaultTransport——
// 并行用例的 CloseIdleConnections 会打断其它用例在途请求，故每测试自建 client。
func authLogUploadFile(t *testing.T, client *http.Client, baseURL, filename string, body []byte) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err = mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req, err := http.NewRequest("POST", baseURL+"/upload", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	sum := sha256.Sum256(body)
	req.Header.Set("X-File-Checksum", hex.EncodeToString(sum[:]))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// authLogNewServer 走**生产装配**（RegisterRoutes）起测试服务：空 Ring +
// AllowInsecureLoopback（等价 CI Benchmark job 的 benchServer 装配），并把注入的
// logger 同时用作业务日志与审计日志（同 CI 基准的做法）。
//
// 不复用同包的 newAuthSeamServer：它默认把 Logger/AuditLogger 设成 testLogger()，而本用例的
// 断言要求 logger 完全受控（「请求期零 WARN」/「WARN 必须进注入 logger」），任何默认 logger
// 兜底都会让断言失真。若日后合并两个 helper，务必先保证注入的 logger 不被默认值覆盖。
func authLogNewServer(t *testing.T, logger *slog.Logger, optsMod func(*RegisterRoutesOpts)) string {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	noAuth := defaultNoAuthRegOpts()
	opts := RegisterRoutesOpts{
		Mux:                   http.NewServeMux(),
		CfgPtr:                &cfgPtr,
		Version:               "test-authlog",
		BuildAt:               "test-authlog",
		Logger:                logger,
		AuditLogger:           logger,
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	}
	if optsMod != nil {
		optsMod(&opts)
	}
	h := RegisterRoutes(t.Context(), opts)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	return ts.URL
}

// ---- (a) 生产装配 + 回环上传：未带 Authorization 头不得产生任何 WARN ----

// TestAuthLog_ProductionAssemblyAnonymousUploadNoWarn 复现 CI Benchmark job 的日志噪音
// （run 34967474774 实测 pkg/server 段 39868 行 WARN）并锁定修复。
//
// 语义缺陷：生产装配（RegisterRoutes）+ 空 Ring + AllowInsecureLoopback 下，回环上传
// **完全不携带 Authorization 头**（基准、健康探测、无认证调试的常态）曾被
// verifySproxySigFromRing 判为「非法 SproxySig 头」——ParseHeader("") 返回
// ErrMalformed。无头请求不是「非法头」而是「未携带凭据」，属认证链的常规失败路径，
// 不应 WARN。
//
// 双通道断言（缺一不可）：
//  1. 注入 logger（RegisterRoutesOpts.Logger → h.logger）在**请求处理期**不得出现任何
//     WARN（装配期日志——如卷根 F1 裁决 WARN——与请求路径无关，用基线下标排除）；
//  2. 包级 slog.Default 里不得出现「非法 SproxySig 头」——修复前该行正是经**包级
//     slog** 漏出（注入的 opts.Logger 关不掉），未修时本断言必红。
//
// 第 2 条按消息过滤而非「无 WARN」：本用例改写进程级 slog.Default（见 sproxy:serial
// 标记），并行用例在「直接构造 Handlers（logger 为 nil）→ 回退 slog.Default」路径上的
// 合法 WARN 会落到同一 handler，按消息过滤才能既确定又无误报。
func TestAuthLog_ProductionAssemblyAnonymousUploadNoWarn(t *testing.T) {
	// sproxy:serial: 改写进程级 slog.Default（用于捕获误走包级 logger 的认证日志），
	// 该全局 handler 与并行用例共享，不可与其他用例并发执行。
	logger, injected := captureAuthLogs()
	defaultBuf := &authLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(defaultBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	url := authLogNewServer(t, logger, nil)
	// 装配期日志（卷根 F1 裁决等）与请求路径无关：冻结基线下标后只断言请求期增量。
	baseline := injected.Len()

	client := &http.Client{Transport: netutil.IsolatedTransport()}
	t.Cleanup(client.CloseIdleConnections)

	// 无 Authorization 头（生产常态）→ 回环兜底放行 → 200。
	if code := authLogUploadFile(t, client, url, "authlog-anon.bin", []byte("auth-log-anon")); code != http.StatusOK {
		t.Fatalf("无 Authorization 头上传 status = %d, want 200（回环兜底放行）", code)
	}

	requestPhase := injected.String()[baseline:]
	if warns := authLogWarnLines(requestPhase); len(warns) > 0 {
		t.Fatalf("请求处理期注入 logger 收到 %d 行 WARN，认证路径必须全部走注入 logger 且无头请求不打 WARN：\n%s",
			len(warns), strings.Join(warns, "\n"))
	}
	if authLogHasMessage(defaultBuf.String(), "auth: 非法 SproxySig 头") {
		t.Fatalf("包级 slog.Default 仍收到「非法 SproxySig 头」误报（无 Authorization 头不是非法头）：\n%s",
			defaultBuf.String())
	}
}

// ---- (b) 格式非法但存在的 Authorization 头：WARN 必须落到注入 logger ----

// TestAuthLog_MalformedHeaderWarnGoesToInjectedLogger 是 seam 的判别性用例：Authorization
// 头**存在但格式非法**是真实可疑事件，必须仍留 WARN 痕迹——但只能经注入 logger
// （RegisterRoutesOpts.Logger → h.logger → RingAuthenticator.logger），
// 不得再走包级 slog（包级 slog 既不受 opts.Logger 控制，也不经
// telemetry.WithContextHandler，丢 trace_id）。
func TestAuthLog_MalformedHeaderWarnGoesToInjectedLogger(t *testing.T) {
	t.Parallel()

	logger, injected := captureAuthLogs()
	url := authLogNewServer(t, logger, func(opts *RegisterRoutesOpts) {
		withTestCreds(opts) // 非空 Ring：链全失败 → 401（不走回环兜底）
	})

	client := &http.Client{Transport: netutil.IsolatedTransport()}
	t.Cleanup(client.CloseIdleConnections)

	req, err := http.NewRequest("GET", url+"/api/files", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "SproxySig v=2 ak=broken") // 存在但格式非法（缺 skey-id/ts/exp/sig）
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("非法 SproxySig 头 status = %d, want 401", resp.StatusCode)
	}

	captured := injected.String()
	if !authLogHasMessage(captured, "auth: 非法 SproxySig 头") {
		t.Fatalf("注入 logger 未收到「非法 SproxySig 头」WARN——认证日志仍走包级 slog（seam 未生效）。捕获：\n%s", captured)
	}
	if warns := authLogWarnLines(captured); len(warns) == 0 {
		t.Fatalf("注入 logger 无任何 WARN：\n%s", captured)
	}
}

// ---- (c) 无 Authorization 头：哨兵错误 + 零日志（白盒，直连认证器 seam）----

// TestAuthLog_NoAuthorizationHeaderSilentSentinel 白盒锁定语义修复：请求完全不携带
// Authorization 头时 verifySproxySigFromRing 返回 errMissingAuthorization 哨兵
// （authMiddleware 据此继续尝试链中后续成员、最终收敛 401 或回环兜底），且**不写任何
// 日志**；同一用例接着验证「头存在但格式非法」**必须**经注入 logger 留 WARN——两条
// 路径的区分正是本次修复的核心。
func TestAuthLog_NoAuthorizationHeaderSilentSentinel(t *testing.T) {
	t.Parallel()

	logger, buf := captureAuthLogs()
	a := NewRingAuthenticator(ringForTestCreds(), sproxysig.NewNoncePool(), WithRingLogger(logger))

	// 无 Authorization 头 → 哨兵错误、零日志（CI Benchmark job 39868 行 WARN 的根因）。
	r := httptest.NewRequest("GET", "/api/files", nil)
	p, err := a.Authenticate(r.Context(), r)
	if !errors.Is(err, errMissingAuthorization) {
		t.Fatalf("无 Authorization 头 error = %v, want errMissingAuthorization", err)
	}
	if p != nil {
		t.Fatalf("失败时 Principal 应为 nil，got %+v", p)
	}
	if got := buf.String(); got != "" {
		t.Fatalf("无 Authorization 头不得产生任何日志，got:\n%s", got)
	}

	// 头存在但格式非法（同一注入 logger）→ 必须 WARN 留痕（seam 生效）。
	bad := httptest.NewRequest("GET", "/api/files", nil)
	bad.Header.Set("Authorization", "not-a-sproxy-sig")
	if _, err := a.Authenticate(bad.Context(), bad); err == nil {
		t.Fatal("格式非法头应返回 error")
	}
	if !authLogHasMessage(buf.String(), "auth: 非法 SproxySig 头") {
		t.Fatalf("格式非法头必须经注入 logger 留 WARN。捕获：\n%s", buf.String())
	}
}

// ---- (d) handleNoBearerToken 的 seam（free func 加 logger 参数）----

// TestAuthLog_MissingBearerTokenWarnGoesToInjectedLogger 验证 api_keys 场景下
// 「缺少 Bearer token」的 WARN 也走注入 logger（handleNoBearerToken 的 logger 参数）。
func TestAuthLog_MissingBearerTokenWarnGoesToInjectedLogger(t *testing.T) {
	t.Parallel()

	logger, injected := captureAuthLogs()
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{APIKeys: APIKeyConfig{Enabled: true, Keys: []APIKey{{Key: "k", Permission: "write"}}}})

	h := &Handlers{cfgPtr: cfgPtr, logger: logger}
	called := false
	handler := h.authMiddleware(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest("GET", "/api/files", nil) // 无 Authorization 头
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("api_keys 开启且无 Bearer token status = %d, want 401", w.Code)
	}
	if called {
		t.Fatal("无 Bearer token 不应放行")
	}
	if !authLogHasMessage(injected.String(), "auth: missing bearer token") {
		t.Fatalf("注入 logger 未收到「missing bearer token」WARN。捕获：\n%s", injected.String())
	}
}
