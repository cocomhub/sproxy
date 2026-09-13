// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// remote_write_metrics_test.go 钉住 W4：写面**授权拒绝**被计入带标签指标
// `sproxy_remote_write_denied_total{reason,node}`（供运维回答「谁在被拒、为什么」）。
//
// 只断言「拒绝进指标」这条链路（reason 码 + node 标签）；渲染格式由 metrics_labeled_test.go 覆盖。
// 断言的是**渲染后的文本**而非内部 map —— 用户可见契约才算数。

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// renderRemoteWriteDenied 渲染拒绝指标族（与 MetricsHandler 用同一实现）。
func renderRemoteWriteDenied(h *Handlers) string {
	var b strings.Builder
	writeLabeledCounter(&b, "sproxy_remote_write_denied_total", "test", h.metrics.remoteWriteDeniedSamples())
	return b.String()
}

func TestRemoteWrite_DenialsCountedWithReason(t *testing.T) {
	const target = "/remote/mkdir?volume=main&path=d"

	cases := []struct {
		name       string
		peerFP     string
		scope      string
		wantReason string
		wantNode   string
	}{
		{
			name: "scope 不授予写 → reason=scope_denied，带 node 标签",
			// node 标签来自**指纹反查**（配置里绑定的节点名），不是请求参数——这正是带标签的价值。
			peerFP: testReaderFP, scope: volume.MeshScopeRead,
			wantReason: "scope_denied", wantNode: testReaderNodeA,
		},
		{
			name:   "指纹未 pin → reason=not_pinned（无 node 可标）",
			peerFP: "sha256:" + strings.Repeat("0", 64), scope: volume.MeshScopeRW,
			wantReason: "not_pinned", wantNode: "",
		},
		{
			name:   "未认证 → reason=unauthenticated",
			peerFP: "", scope: volume.MeshScopeRW,
			wantReason: "unauthenticated", wantNode: "",
		},
		{
			name:   "卷不存在 → reason=volume_unknown",
			peerFP: testReaderFP, scope: volume.MeshScopeRW,
			wantReason: "volume_unknown", wantNode: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := remoteWriteTestConfig(t, tc.scope)
			h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
			wh := h.newRemoteWriteHandler(fakePeerFingerprint{fp: tc.peerFP})

			req := target
			if tc.name == "卷不存在 → reason=volume_unknown" {
				req = "/remote/mkdir?volume=nope&path=d"
			}
			rec := doRemoteWrite(t, wh, http.MethodPost, req, nil, "")
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusUnauthorized {
				t.Fatalf("本用例应被拒（404/401），得到 %d", rec.Code)
			}

			body := renderRemoteWriteDenied(h)
			want := `sproxy_remote_write_denied_total{node="` + tc.wantNode + `",reason="` + tc.wantReason + `"} 1`
			if !strings.Contains(body, want) {
				t.Fatalf("缺少拒绝计数 %q:\n%s", want, body)
			}
		})
	}
}

// TestRemoteWrite_AllowedRequestNotCounted 钉住「放行不计入拒绝」（否则指标失去意义）。
func TestRemoteWrite_AllowedRequestNotCounted(t *testing.T) {
	cfg := remoteWriteTestConfig(t, volume.MeshScopeRW)
	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	wh := h.newRemoteWriteHandler(fakePeerFingerprint{fp: testReaderFP})

	if rec := doRemoteWrite(t, wh, http.MethodPost, "/remote/mkdir?volume=main&path=d", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("放行用例应 200，得到 %d（body=%s）", rec.Code, rec.Body.String())
	}
	// 只有 HELP/TYPE 行，没有任何样本行。
	if body := renderRemoteWriteDenied(h); strings.Contains(body, "sproxy_remote_write_denied_total{") {
		t.Fatalf("放行请求不应产生拒绝计数:\n%s", body)
	}
}
