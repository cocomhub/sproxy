// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// urlsig_test.go —— **SproxySig 的 URL 查询参数形态**（`?spx_sig=`）语义锁定（方案 B'）。
//
// 需求（2026-10-21 用户实测）：WebUI 的 `<video src="/download?filename=...&volume=...">`
// 401——浏览器原生标签无法自定义请求头。方案：把 SproxySig 头**放进 URL query**，由前端用
// 既有 `sclientSig.signHeader` 计算（登录会话已持 SK，零新增能力）。
//
// 与请求头形态的**两处差异**（本文件锁定的核心）：
//  1. **可重放**：`<video>` 固定 URL，浏览器发多个 Range 请求复用同一签名 ⇒ URL 形态**不查
//     nonce 池**（否则第二个请求 401）。这是「video 预览 401」的根因。
//  2. **TTL 放宽**：默认 1h（请求头形态 5min），长视频播放中途不因 exp 到期 401。

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// newURLSigRing 建带单个 AK/SK 条目的 Ring（返回 ring + AK + SK hex + skeyID）。
func newURLSigRing(t *testing.T) (*accesskey.Ring, string, string, string) {
	t.Helper()
	ring := accesskey.NewRing()
	sk := make([]byte, 32)
	for i := range sk {
		sk[i] = byte(i + 1)
	}
	const ak = "ak-urlsig-test"
	if _, _, err := ring.AddRegistration(ak, "owner-urlsig", sk, nil, accesskey.RoleUser, time.Hour); err != nil {
		t.Fatalf("AddRegistration: %v", err)
	}
	entries, ok := ring.Lookup(ak)
	if !ok || len(entries) == 0 {
		t.Fatalf("Lookup(%s) 无条目", ak)
	}
	return ring, ak, hex.EncodeToString(sk), entries[0].ID
}

// signURL 用 sproxysig 构造 URL 票（模拟前端 signHeader），query 为**不含票**的业务查询。
func signURL(t *testing.T, ak, sk, skeyID, method, path, query string, ttl time.Duration) string {
	t.Helper()
	now := time.Now()
	h := sproxysig.Header{
		Version: sproxysig.Version, AK: ak, EntryID: skeyID,
		TS: now.UnixMilli(), Exp: now.Add(ttl).UnixMilli(),
		Nonce: sproxysig.NewNonce(), BodySHA256: sproxysig.EmptyBodyHash(),
	}
	return sproxysig.SignAndFormat(sk, h, method, path, query)
}

// newURLSigHandlers 建带 Ring 的 Handlers。**带真实 noncePool**——否则「URL 形态可重放」
// 的断言是假绿（nil 池本就不查重放）；必须用真池证明「URL 形态主动绕过 nonce」.
func newURLSigHandlers(t *testing.T, ring *accesskey.Ring) *Handlers {
	t.Helper()
	auth := NewRingAuthenticator(ring, sproxysig.NewNoncePool(), WithURLSigTTL(time.Hour))
	return &Handlers{credentialRing: ring, noncePool: sproxysig.NewNoncePool(), authenticators: []Authenticator{auth}}
}

// TestURLSig_ReplayableMultipleRequests：**同一 URL 票可被多次请求**（video 多 Range 请求
// 复用同一 src）——这是本方案存在的根本理由（请求头形态第二次即被判重放 401）。
// **用真实 noncePool**：证明 URL 形态是主动**绕过** nonce 检查，而非「恰好没有池」。
func TestURLSig_ReplayableMultipleRequests(t *testing.T) {
	t.Parallel()
	ring, ak, sk, skeyID := newURLSigRing(t)
	h := newURLSigHandlers(t, ring) // 带真实 noncePool

	query := "filename=movie.mp4&volume=vault"
	token := signURL(t, ak, sk, skeyID, http.MethodGet, "/download", query, time.Hour)

	for i := range 3 {
		req := httptest.NewRequest(http.MethodGet, "/download?"+query+"&"+urlSigQueryKey+"="+url.QueryEscape(token), nil)
		cred, err := h.authenticators[0].Authenticate(req.Context(), req)
		if err != nil {
			t.Fatalf("第 %d 次请求应成功（URL 票绕过 nonce 可重放），got %v", i+1, err)
		}
		if cred == nil || cred.AK != ak {
			t.Fatalf("第 %d 次请求身份错误: %+v", i+1, cred)
		}
	}
}

// TestHeaderForm_ReplayBlocked（对照）：**请求头形态**在同一 noncePool 下第二次必被拒
// （nonce 重放防护仍在）——证明上述 URL 形态的「可重放」是**形态差异**而非全局关闭。
func TestHeaderForm_ReplayBlocked(t *testing.T) {
	t.Parallel()
	ring, ak, sk, skeyID := newURLSigRing(t)
	pool := sproxysig.NewNoncePool()
	auth := NewRingAuthenticator(ring, pool, WithURLSigTTL(time.Hour))

	token := signURL(t, ak, sk, skeyID, http.MethodGet, "/download", "filename=a.mp4", 5*time.Minute)
	first := httptest.NewRequest(http.MethodGet, "/download?filename=a.mp4", nil)
	first.Header.Set("Authorization", token)
	if _, err := auth.Authenticate(first.Context(), first); err != nil {
		t.Fatalf("首次请求应成功: %v", err)
	}
	second := httptest.NewRequest(http.MethodGet, "/download?filename=a.mp4", nil)
	second.Header.Set("Authorization", token) // 同一签名（同 nonce）
	if _, err := auth.Authenticate(second.Context(), second); err == nil {
		t.Fatal("请求头形态重复同一 nonce 应被拒（防重放未被削弱），却成功")
	}
}

// TestURLSig_TamperedQueryFails：**改业务参数即签名不符**（票绑定 method+path+query）——
// 拿到 URL 也不能改成别的文件名/卷取用。
func TestURLSig_TamperedQueryFails(t *testing.T) {
	t.Parallel()
	ring, ak, sk, skeyID := newURLSigRing(t)
	h := newURLSigHandlers(t, ring)

	token := signURL(t, ak, sk, skeyID, http.MethodGet, "/download", "filename=a.mp4&volume=vault", time.Hour)
	// 篡改 filename（签名时是 a.mp4，请求时用 b.mp4）。
	req := httptest.NewRequest(http.MethodGet, "/download?filename=b.mp4&volume=vault&"+urlSigQueryKey+"="+url.QueryEscape(token), nil)
	if _, err := h.authenticators[0].Authenticate(req.Context(), req); err == nil {
		t.Fatal("篡改 filename 后应验签失败，却成功")
	}
}

// TestURLSig_ExpiredFails：超过 TTL 的票不可用（exp 参与签名 ⇒ 也无法篡改延期）。
func TestURLSig_ExpiredFails(t *testing.T) {
	t.Parallel()
	ring, ak, sk, skeyID := newURLSigRing(t)
	h := newURLSigHandlers(t, ring)

	// 已过期 1 分钟的票（ttl = -1min）。
	token := signURL(t, ak, sk, skeyID, http.MethodGet, "/download", "filename=a.mp4", -time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/download?filename=a.mp4&"+urlSigQueryKey+"="+url.QueryEscape(token), nil)
	if _, err := h.authenticators[0].Authenticate(req.Context(), req); err == nil {
		t.Fatal("过期票应验签失败，却成功")
	}
}

// TestURLSig_LongTTLAcceptedWithinCfgLimit：TTL 长于 sproxysig 默认 5min（如 30min）仍被
// 接受——`download_token_ttl` 上限生效（长视频播放不中途 401）。
func TestURLSig_LongTTLAcceptedWithinCfgLimit(t *testing.T) {
	t.Parallel()
	ring, ak, sk, skeyID := newURLSigRing(t)
	h := newURLSigHandlers(t, ring) // WithURLSigTTL(time.Hour)

	token := signURL(t, ak, sk, skeyID, http.MethodGet, "/download", "filename=a.mp4", 30*time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/download?filename=a.mp4&"+urlSigQueryKey+"="+url.QueryEscape(token), nil)
	if _, err := h.authenticators[0].Authenticate(req.Context(), req); err != nil {
		t.Fatalf("30min TTL 应在 1h 配置上限内被接受，got %v", err)
	}
}

// TestURLSig_BeyondCfgTTLFails：TTL 超过配置上限（如 2h > 1h）被拒（`URLSigTTL` 上限生效）。
func TestURLSig_BeyondCfgTTLFails(t *testing.T) {
	t.Parallel()
	ring, ak, sk, skeyID := newURLSigRing(t)
	h := newURLSigHandlers(t, ring) // 上限 1h

	token := signURL(t, ak, sk, skeyID, http.MethodGet, "/download", "filename=a.mp4", 2*time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/download?filename=a.mp4&"+urlSigQueryKey+"="+url.QueryEscape(token), nil)
	if _, err := h.authenticators[0].Authenticate(req.Context(), req); err == nil {
		t.Fatal("2h TTL 超出 1h 上限应被拒，却成功")
	}
}

// TestURLSig_HeaderFormUnaffected：**未带 URL 票**时请求头形态照常工作（零回归）。
func TestURLSig_HeaderFormUnaffected(t *testing.T) {
	t.Parallel()
	ring, ak, sk, skeyID := newURLSigRing(t)
	h := newURLSigHandlers(t, ring)

	// 请求头形态（含 nonce 池为 nil ⇒ 不查重放）应成功。
	token := signURL(t, ak, sk, skeyID, http.MethodGet, "/download", "filename=a.mp4", 5*time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/download?filename=a.mp4", nil)
	req.Header.Set("Authorization", token)
	cred, err := h.authenticators[0].Authenticate(req.Context(), req)
	if err != nil {
		t.Fatalf("请求头形态应成功（零回归），got %v", err)
	}
	if cred == nil || cred.AK != ak {
		t.Fatalf("身份错误: %+v", cred)
	}
}

// TestURLSig_StripQueryKeyPreservesOrder：票参数剥离**保留其余参数原顺序/编码**
// （签名按 RawQuery 原串比对，重建会乱序导致误判）。
func TestURLSig_StripQueryKeyPreservesOrder(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("/download?volume=vault&filename=a%20b.mp4&spx_sig=xxx&subdir=d%2Fe")
	_, _, query := urlSigRequestPath(http.MethodGet, u)
	// 期望：保序且原编码，仅去掉 spx_sig 段。
	if !strings.Contains(query, "volume=vault&filename=a%20b.mp4") {
		t.Fatalf("剥离后参数顺序/编码被破坏: %q", query)
	}
	if strings.Contains(query, urlSigQueryKey) {
		t.Fatalf("票参数未被剥离: %q", query)
	}
}

// TestURLSig_MethodRestrictedToGetHead（P2 修复，2026-10-21）：URL 票形态**仅**认 GET/HEAD
// （浏览器原生标签读取场景）；POST/PUT/DELETE 等带 body 方法**不认票**——URL 形态无 body
// 完整性校验（bodyValidator 只在请求头形态挂载），放开写方法会让已捕获的 URL 形态请求可
// 被改 body 而不被发现。此用例锁死该语义（POST 带合法票 + 同 body_sha256 签名仍被拒）。
func TestURLSig_MethodRestrictedToGetHead(t *testing.T) {
	t.Parallel()
	ring, ak, sk, skeyID := newURLSigRing(t)
	a := NewRingAuthenticator(ring, sproxysig.NewNoncePool(), WithURLSigTTL(time.Hour))

	// POST 带**正确签名**的票（若 tryURLSig 不限制方法则会被放行 ⇒ 本用例红）。
	postTicket := signURL(t, ak, sk, skeyID, http.MethodPost, "/api/cloud/download", "filename=x", time.Hour)
	postURL, _ := url.Parse("/api/cloud/download?filename=x&" + urlSigQueryKey + "=" + url.QueryEscape(postTicket))
	postReq, _ := http.NewRequest(http.MethodPost, postURL.String(), strings.NewReader("body"))
	if cred := a.tryURLSig(postReq); cred != nil {
		t.Fatalf("POST 不应认 URL 票（方法限制）：tryURLSig 应返回 nil（回落请求头形态，保留 body 校验）")
	}

	// 对照：GET 同票应被认（方法限制只挡写方法）。
	getTicket := signURL(t, ak, sk, skeyID, http.MethodGet, "/download", "filename=x", time.Hour)
	getURL, _ := url.Parse("/download?filename=x&" + urlSigQueryKey + "=" + url.QueryEscape(getTicket))
	getReq, _ := http.NewRequest(http.MethodGet, getURL.String(), nil)
	if cred := a.tryURLSig(getReq); cred == nil {
		t.Fatalf("GET 同票应被认（方法限制不挡读方法）")
	}
}
