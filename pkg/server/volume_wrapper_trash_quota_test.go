// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_trash_quota_test.go —— **外部/封装卷的回收站边界**回归
// （设计 `2026-10-16-quota-volume-layer.md` §9.3 片 6 第 1 步）。
//
// **背景（漂移嫌疑的证伪）**：片 6 取证时怀疑「释放 ≠ 结算」——**释放侧**回收站走
// `trashEntryReleaseSize`（**逻辑长度**）+ `physicalCharge`（`RootPhysSizer` ⇒
// `storage.EncryptedPhysicalSize`，**公式**），而**结算侧**走 `settleSize`（**卷自报足迹**优先 ⇒
// secretdata 含 meta blob pad，公式**覆盖不到**）⇒ 理论上对 secretdata 会**少释放**。
//
// **实测结论：该漂移不可达** ✓ —— 外部/封装卷的软删被**显式拒绝**（`soft=true` ⇒ 400
// 「外部卷暂不支持回收站删除：请改用硬删」）⇒ **回收站释放路径只作用于本地卷**，而本地卷的
// 结算侧同样走公式（`RootPhysSizer`，无自报能力）⇒ **两族来源永不重叠** ⇒ `释放 == 结算` 成立 ✓。
// （硬删路径走 `DeleteFileInput.chargedHint`/`prevCharge`，本就是**卷自报足迹优先** ⇒ 同源 ✓。）
//
// **本用例钉住的不变式**（两条，均属「拒绝必须零副作用」类）：
//  ① 外部/封装卷软删 ⇒ **400 且不回退硬删**（fail-closed ⇒ 不得静默降级为永久删除 ✗）；
//  ② 被拒绝的软删 **零副作用**：配额不变（不得「先释放再报错」✗，也不得已改名进回收站 ✗）。
//
// 变异：把拒绝分支改为「回退硬删」⇒ ① 红；在拒绝**之前**先释放配额 ⇒ ② 红。

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// writeMuxSoftDelete 走真实 delete handler 软删（`?soft=true`），带校验和头。
func writeMuxSoftDelete(t *testing.T, mux *http.ServeMux, filename, vol, checksum string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/delete?filename="+filename+"&volume="+vol+"&soft=true", nil)
	req.Header.Set(headerFileChecksum, checksum)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// TestWrapperSoftDelete_RefusedFailClosedAndNoSideEffect 外部/封装卷软删：
// fail-closed 拒绝 + 配额零副作用（回收站边界不变式）。
func TestWrapperSoftDelete_RefusedFailClosedAndNoSideEffect(t *testing.T) {
	t.Parallel()
	h := newNestedSecretWrapperHandlers(t, 0)
	mux := actorFileWriteMux(h, "alice")
	scope := h.quotaScopeFor("alice", "user/f1.bin")
	if scope == nil {
		t.Fatal("owner 全局 Scope 未装配（测试前提不成立）")
	}
	pool := h.volSet.Pool("vault")

	f1 := bytes.Repeat([]byte("a"), 4096)
	if st, resp := writeMuxUpload(t, mux, "f1.bin", "vault", f1); st != 200 {
		t.Fatalf("上传 f1.bin = %d (body: %s)", st, resp)
	}
	chargedScope, chargedPool := scope.Usage(), pool.Usage()
	if chargedScope <= 0 {
		t.Fatalf("上传后 owner Scope 未记账（%d）", chargedScope)
	}

	// ① 软删被拒绝（fail-closed，不得静默降级为硬删）。
	st, resp := writeMuxSoftDelete(t, mux, "f1.bin", "vault", sha256hex(f1))
	if st != http.StatusBadRequest {
		t.Fatalf("外部/封装卷软删应 400（fail-closed），got %d (body: %s)", st, resp)
	}
	if !strings.Contains(resp, "外部卷暂不支持回收站删除") {
		t.Fatalf("拒绝理由应显式可归因，got body: %s", resp)
	}
	// ② **零副作用**：拒绝路径不得释放配额，也不得已把文件移入回收站。
	if got := scope.Usage(); got != chargedScope {
		t.Fatalf("被拒软删后 owner Scope=%d，want %d（拒绝必须零副作用）", got, chargedScope)
	}
	if got := pool.Usage(); got != chargedPool {
		t.Fatalf("被拒软删后委托池 Usage=%d，want %d（拒绝必须零副作用）", got, chargedPool)
	}
	// 文件必须仍可下载（未被静默删除/移入回收站）。
	if st, body := writeMuxDownload(t, mux, "f1.bin", "vault"); st != 200 || !bytes.Equal(body, f1) {
		t.Fatalf("被拒软删后文件应完好可下载，got st=%d len=%d want len=%d", st, len(body), len(f1))
	}
}
