// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleAITag_CacheHitWithVolumeParam 钉住「带 volume 参数的 tag 请求必须命中缓存」。
//
// 缺陷（2026-11-02 实测）：读缓存用 `volName:filename`（volume 非空时），写缓存却只用
// `filename` ⇒ 带 volume 的请求**永远不会命中**，每次都重调 LLM（成本 + 延迟），且把结果
// 写进了读路径永远查不到的键。
func TestHandleAITag_CacheHitWithVolumeParam(t *testing.T) {
	// sproxy:serial: aiTestEnv 内部 t.Setenv（AI key），不可并行
	h, ai, calls := aiTestEnv(t)

	const url = "/api/ai/tag?filename=user/text.txt&volume=main"
	for i := range 2 {
		rec := httptest.NewRecorder()
		h.handleAITag(rec, httptest.NewRequest(http.MethodPost, url, nil), "anonymous", ai)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应 200，实际 %d", i+1, rec.Code)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("带 volume 的第二次请求应命中缓存（网关调用 1 次），实际 %d 次——缓存读写键不一致", n)
	}
}
