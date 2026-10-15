// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_move_quota_test.go 验证**跨卷 move 删外部源**的配额归还（复审 §8.1）：从嵌套
// 封装卷（委托子池 + 父链聚合到底座池）把文件搬出去时，源侧删源必须与「delete 删外部卷」
// **同一释放路径**——否则委托池/底座池保留幽灵占用（底座卷设容量时产生假 507，重启才归零）。
//
// 端到端真实副作用：config 声明嵌套封装卷（secretdata over enclosure 区域视图）+ 真实 move handler →
// 真实容量池账本；断言两池在搬出后**精确回落**到「只剩余留文件」的那一档，并在全部搬出 +
// 删净后归零（owner 全局账本同口径）。

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// actorMoveMux = actorFileWriteMux + POST /api/volumes/move（跨卷移动配额用例用）。
func actorMoveMux(h *Handlers, actor string) *http.ServeMux {
	mux := actorFileWriteMux(h, actor)
	mux.HandleFunc("POST /api/volumes/move", func(w http.ResponseWriter, r *http.Request) {
		h.moveVolumeHandler(w, r.WithContext(withActor(r.Context(), actor)))
	})
	return mux
}

// muxVolumeMove 走真实 move handler（POST /api/volumes/move），返回状态码与响应体。
func muxVolumeMove(t *testing.T, mux *http.ServeMux, fromVol, toVol, filename string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/volumes/move?from_volume="+url.QueryEscape(fromVol)+
		"&to_volume="+url.QueryEscape(toVol)+"&filename="+url.QueryEscape(filename), nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// TestWrapperMoveQuota_ExternalSourceReleasesPools 跨卷 move **源为嵌套封装卷**：搬出 f1 后
// 委托子池与底座池必须精确回落到「只剩 f2」；再搬出 f2 后两池归零；最后删净目标卷副本 →
// owner 全局 Scope 归零（无幽灵占用残留）。
//
// 目标卷取**独立本地卷 disk2**（而非封装卷的底座 main）：main 的池同时承载自身的直接写入，
// 用 disk2 才能让「底座池 == 委托子池父链聚合」这一断言保持单义（与删除链路用例同口径）。
//
// 变异（本测必红）：删源不归还委托池 ⇒ 第一次 move 后两池仍含 f1 记账（假 507）；
// 重复释放 ⇒ 回落到低于「只剩 f2」的一档（少计超配）。
func TestWrapperMoveQuota_ExternalSourceReleasesPools(t *testing.T) {
	t.Parallel()
	h := newNestedSecretWrapperHandlers(t, 0, VolumeConfig{Name: "disk2", Root: t.TempDir()})
	mux := actorMoveMux(h, "alice")

	pool := h.volSet.Pool("vault")
	basePool := h.volSet.Pool("main")
	scope := h.quotaScopeFor("alice", "user/m1.bin")
	if pool == nil || basePool == nil || scope == nil {
		t.Fatal("测试前提不成立：委托池/底座池/owner Scope 未装配")
	}
	if p := pool.Usage(); p != 0 {
		t.Fatalf("初始委托池 Usage()=%d, want 0", p)
	}

	f1 := bytes.Repeat([]byte("a"), 4096)
	f2 := bytes.Repeat([]byte("b"), 2048)
	if st, resp := writeMuxUpload(t, mux, "m1.bin", "vault", f1); st != http.StatusOK {
		t.Fatalf("上传 m1.bin = %d (body: %s)", st, resp)
	}
	charge1 := pool.Usage()
	if charge1 <= 0 {
		t.Fatalf("上传 m1 后委托池 Usage=%d：记账未接线（测试前提不成立）", charge1)
	}
	if st, resp := writeMuxUpload(t, mux, "m2.bin", "vault", f2); st != http.StatusOK {
		t.Fatalf("上传 m2.bin = %d (body: %s)", st, resp)
	}
	p2 := pool.Usage()
	if p2 <= charge1 {
		t.Fatalf("上传 m2 后委托池 Usage=%d 未超过 m1 档 %d", p2, charge1)
	}
	if got := basePool.Usage(); got != p2 {
		t.Fatalf("底座池 Usage()=%d ≠ 委托池 %d（父链聚合断裂）", got, p2)
	}
	restAfterMove := p2 - charge1 // 搬走 m1 后应有的占用（= m2 记账）

	// 跨卷 move：源为嵌套封装卷（外部卷 ⇒ 域侧组合路径 + 外部卷删源）。
	if st, resp := muxVolumeMove(t, mux, "vault", "disk2", "m1.bin"); st != http.StatusOK {
		t.Fatalf("move vault→disk2 = %d, want 200 (body: %s)", st, resp)
	}
	if got := pool.Usage(); got != restAfterMove {
		t.Fatalf("搬出后委托池 Usage()=%d, want %d（删源未归还委托池 ⇒ 幽灵占用 = 假 507）",
			got, restAfterMove)
	}
	if got := basePool.Usage(); got != restAfterMove {
		t.Fatalf("搬出后底座池 Usage()=%d, want %d（父链释放口径错误）", got, restAfterMove)
	}
	// owner 全局账本在搬出后仍有正占用（目标卷副本 + 可能的其它记账）：真正的判据是下面的
	// 「搬净 + 删净 → 归零」——任何来源的幽灵占用都会在那里暴露。
	if got := scope.Usage(); got <= 0 {
		t.Fatalf("搬出后 owner Scope Usage()=%d, want > 0（目标卷副本仍占账）", got)
	}

	// 再搬出 m2 → 源卷（封装卷）账本归零。
	if st, resp := muxVolumeMove(t, mux, "vault", "disk2", "m2.bin"); st != http.StatusOK {
		t.Fatalf("move m2 vault→disk2 = %d, want 200 (body: %s)", st, resp)
	}
	if got := pool.Usage(); got != 0 {
		t.Fatalf("搬净后委托池 Usage()=%d, want 0", got)
	}
	if got := basePool.Usage(); got != 0 {
		t.Fatalf("搬净后底座池 Usage()=%d, want 0", got)
	}

	// 删净目标卷副本 → owner 全局账本归零（任何来源的幽灵占用都会在此暴露）。
	for name, body := range map[string][]byte{"m1.bin": f1, "m2.bin": f2} {
		if st, resp := writeMuxDelete(t, mux, name, "disk2", sha256hex(body)); st != http.StatusOK {
			t.Fatalf("删除 disk2/%s = %d (body: %s)", name, st, resp)
		}
	}
	if got := scope.Usage(); got != 0 {
		t.Fatalf("删净后 owner Scope Usage()=%d, want 0（幽灵占用：源侧删除未归还全局账本）", got)
	}
}
