// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// self_credential_test.go 钉住 `Handlers.SelfCredential`（Y 二期 P3-d 装配前置）：
// A 侧 mesh 中继要经**本机** hub API（`/api/hub/services`、`/api/relay/stream`），故同进程内
// 组件需要一份**可用的自用凭据**（AK/SK/skeyID）。
//
// 本文件的强断言不是「返回值非空」，而是**拿它去对本机自签名并拿到 200**——凭据可用性
// 只能由真实认证链证明（否则返回一对看着对、签不过的字符串同样会绿）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newSelfCredFixture 装配带测试 Ring 的 *Handlers。
//
// 不起 HTTP server：强断言直接打 h.Handler()（同一 mux 与认证链），少一层网络噪声。
func newSelfCredFixture(t *testing.T) *Handlers {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	opts := RegisterRoutesOpts{
		Mux:     http.NewServeMux(),
		CfgPtr:  &cfgPtr,
		Version: "test",
		BuildAt: "test",
		Logger:  testLogger(),
	}
	withTestCreds(&opts) // 注入含 testAccessKey 的 Ring（既有测试辅助）
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// TestHandlers_SelfCredential_UsableForSelfAuth 钉住：返回的凭据**能被本机认证链接受**。
func TestHandlers_SelfCredential_UsableForSelfAuth(t *testing.T) {
	h := newSelfCredFixture(t)

	ak, sk, entryID, ok := h.SelfCredential()
	if !ok {
		t.Fatal("注入 Ring 后 SelfCredential 应返回 ok=true")
	}
	if ak != testAccessKey {
		t.Fatalf("AK=%q want %q（应取 Ring 中首个存活条目）", ak, testAccessKey)
	}
	if entryID == "" || !strings.HasPrefix(entryID, "skey-") {
		t.Fatalf("skeyID=%q 应为 skey-<12hex>（v2 签名必传）", entryID)
	}
	if len(sk) != 64 {
		t.Fatalf("SK 应为 64 hex 字符, got %d 字符", len(sk))
	}

	// 强断言：用返回的凭据对本机自签名，必须通过认证（非 401/403）。
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/api/files", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 直接 ServeHTTP 时须显式给非 nil Body：真实服务端请求经 net/http 规范化后 Body 恒非 nil
	// （空体为 http.NoBody），而 bodyValidator 会包装它。此处模拟真实形态。
	req.Body = http.NoBody
	req.ContentLength = 0
	signRequestEntry(req, ak, entryID, sk)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("自用凭据未通过本机认证：status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandlers_SelfCredential_EmptyRingFailClosed 钉住：Ring 为空时 ok=false（调用方据此
// 拒绝装配，而不是拿一对空字符串去签名）。
func TestHandlers_SelfCredential_EmptyRingFailClosed(t *testing.T) {
	h := &Handlers{} // credentialRing == nil
	if _, _, _, ok := h.SelfCredential(); ok {
		t.Fatal("Ring 为空时 SelfCredential 必须 ok=false（fail-closed）")
	}
}
