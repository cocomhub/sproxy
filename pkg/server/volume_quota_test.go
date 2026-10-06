// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_quota_test.go 验证封装卷配额委托（方案B，2026-10-06）：封装卷不建独立容量池，
// 配额 = 底层卷池的占用子目录 Scope（父链聚合）——
//   - 设 capacity：建卷前置校验 capacity ≤ 底层池余量（超 → 400「超出底层卷可用配额」）；
//   - 不设（0）：Scope 0 不限，底层池满时封装卷上传 507（整体拦截，核心诉求）；
//   - 上传封装卷 → 底层池 Usage 父链增加；删文件 → 减少。
//
// 端到端真实副作用：走真实 upload/delete handler + 真实容量池账本（TryReserve→Commit/
// ReleaseCommitted 父链聚合），断言底层池字节变化。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newNestedWrapperQuotaHandlers 装配封装卷配额委托测试 Handlers（main 本地卷 capacity 由
// 调用方给定，用于「底层池满 / 余量校验」场景）。
func newNestedWrapperQuotaHandlers(t *testing.T, mainCapacity int64) (*Handlers, *UserVolumeStore) {
	t.Helper()
	registerUserVolWrapBackend() // wrapper 类型（Schema target, allow_wrapper）
	registerNestedRootBackend()  // 可读写底层 backend（FS = LocalFS(root)）
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot, VolCapacity: ByteSize(mainCapacity)}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	store := NewUserVolumeStore(cfg.StorageRoot)
	h.SetUserVolumeStore(store)
	return h, store
}

// wrapperVol 走 POST /api/volumes/user 创建嵌套封装卷（target=main/<subdir>，capacity 可选），
// 返回响应 recorder。
func wrapperVol(t *testing.T, mux *http.ServeMux, name, subdir string, capacity int64) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{
		"name": name, "type": nestedRootType,
		"extra": map[string]any{"root": t.TempDir(), "target": "main/" + subdir},
	}
	if capacity > 0 {
		body["capacity"] = capacity
	}
	return postUserVol(t, mux, body)
}

// TestWrapperQuota_UploadAggregatesToBase_DeleteReleases 核心：上传封装卷 → 底层池 Usage 增加
// （父链聚合），删除（ReleaseCommitted 释放，与写面删除路径同方法）→ 底层池 Usage 减少。
// 注：外部封装卷的 HTTP 删除定位（files.locateForRead 按 Root 探测）尚未接线，此处直接调用
// 委托池的释放方法验证父链释放语义（真实账本副作用）。
func TestWrapperQuota_UploadAggregatesToBase_DeleteReleases(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperQuotaHandlers(t, 0) // main 不限容量
	uvmux := userVolWrap(h, "alice")
	fmux := actorFileWriteMux(h, "alice")

	if rec := wrapperVol(t, uvmux, "vault", "videos", 0); rec.Code != http.StatusOK {
		t.Fatalf("创建封装卷 vault = %d (body: %s)", rec.Code, rec.Body.String())
	}
	body := []byte("wrapped secret payload")

	// 上传封装卷 → 200；底层 main 池 Usage 增加（父链聚合）。
	status, resp := writeMuxUpload(t, fmux, "secret.bin", "vault", body)
	if status != http.StatusOK {
		t.Fatalf("上传 vault/secret.bin = %d, want 200 (body: %s)", status, resp)
	}
	basePool := h.volSet.Pool("main")
	if got := basePool.Usage(); got != int64(len(body)) {
		t.Fatalf("上传后 main 池 Usage()=%d, want %d（子 Scope 父链聚合）", got, len(body))
	}
	// 封装卷自身池（委托的子 Scope）Usage 为自身字节。
	if got := h.volSet.Pool("vault").Usage(); got != int64(len(body)) {
		t.Fatalf("vault 委托池 Usage()=%d, want %d", got, len(body))
	}

	// 删除文件按实际字节 ReleaseCommitted（写面删除路径同方法，父链传播到底层池）。
	h.volSet.Pool("vault").ReleaseCommitted(int64(len(body)))
	if got := basePool.Usage(); got != 0 {
		t.Fatalf("释放后 main 池 Usage()=%d, want 0（父链释放）", got)
	}
}

// TestWrapperQuota_CapacitySet_LimitsWrapperUpload 封装卷设 capacity → 子 Scope 上限拦截
// （上传超 capacity → 507），底层池不超计。
func TestWrapperQuota_CapacitySet_LimitsWrapperUpload(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperQuotaHandlers(t, 0) // main 不限，封装自身 capacity 验证子 Scope 上限
	uvmux := userVolWrap(h, "alice")
	fmux := actorFileWriteMux(h, "alice")

	const wrappedCap = int64(30)
	if rec := wrapperVol(t, uvmux, "vault", "videos", wrappedCap); rec.Code != http.StatusOK {
		t.Fatalf("创建封装卷 vault = %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := h.volSet.Pool("vault").MaxBytes(); got != wrappedCap {
		t.Fatalf("vault 委托池 MaxBytes()=%d, want %d", got, wrappedCap)
	}
	body := make([]byte, 20)
	if st, _ := writeMuxUpload(t, fmux, "v1.bin", "vault", body); st != http.StatusOK {
		t.Fatalf("上传 20B 到 vault = %d, want 200", st)
	}
	if got := h.volSet.Pool("main").Usage(); got != 20 {
		t.Fatalf("main 池 Usage()=%d, want 20", got)
	}
	// 已有 20，再传 20 → 超出 capacity 30 → 507（子 Scope 上限拦截）。
	if st, resp := writeMuxUpload(t, fmux, "v2.bin", "vault", body); st != http.StatusInsufficientStorage {
		t.Fatalf("再传 20 到 vault = %d, want 507 (body: %s)", st, resp)
	}
	if got := h.volSet.Pool("main").Usage(); got != 20 {
		t.Fatalf("507 后 main 池 Usage()=%d, want 20（不超计）", got)
	}
}

// TestWrapperQuota_CapacityExceedsBaseRemaining_400 建封装卷 capacity > 底层池余量 → 400，
// 不落盘（无卷泄漏）。
func TestWrapperQuota_CapacityExceedsBaseRemaining_400(t *testing.T) {
	t.Parallel()
	h, store := newNestedWrapperQuotaHandlers(t, 100) // main 容量 100
	uvmux := userVolWrap(h, "alice")
	fmux := actorFileWriteMux(h, "alice")

	// 先向 main 直接传 60 → 余量 40。
	if st, _ := writeMuxUpload(t, fmux, "photos/a.bin", "main", make([]byte, 60)); st != http.StatusOK {
		t.Fatalf("占位 main 上传 = %d, want 200", st)
	}
	// capacity 80 > 余量 40 → 400。
	rec := postUserVol(t, uvmux, map[string]any{
		"name": "vault", "type": nestedRootType, "capacity": 80,
		"extra": map[string]any{"root": t.TempDir(), "target": "main/movies"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("capacity>余量 = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "底层卷可用配额") {
		t.Fatalf("400 文案缺「底层卷可用配额」: %s", rec.Body.String())
	}
	// 不落盘（回滚）。
	if v, _ := store.Get("alice", "vault"); v != nil {
		t.Fatalf("capacity 超余量的 vault 不应创建: %+v", v)
	}
	// main 池 Usage 保持 60（未泄漏）。
	if got := h.volSet.Pool("main").Usage(); got != 60 {
		t.Fatalf("400 后 main 池 Usage()=%d, want 60", got)
	}
}

// TestWrapperQuota_NoCapacity_BaseFull_507 不设 capacity（Scope 0 不限）→ 底层池满 → 封装
// 卷上传 507（整体拦截：父链聚合到底层池）。
func TestWrapperQuota_NoCapacity_BaseFull_507(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperQuotaHandlers(t, 10) // main 容量 10
	uvmux := userVolWrap(h, "alice")
	fmux := actorFileWriteMux(h, "alice")

	if rec := wrapperVol(t, uvmux, "vault", "videos", 0); rec.Code != http.StatusOK {
		t.Fatalf("创建封装卷 vault = %d (body: %s)", rec.Code, rec.Body.String())
	}
	// 传 6B → 200；再传 5B → 底层池 11>10 → 507。
	if st, _ := writeMuxUpload(t, fmux, "a.bin", "vault", make([]byte, 6)); st != http.StatusOK {
		t.Fatalf("vault 传 6B = %d, want 200", st)
	}
	if st, resp := writeMuxUpload(t, fmux, "b.bin", "vault", make([]byte, 5)); st != http.StatusInsufficientStorage {
		t.Fatalf("底层已满再传 5B = %d, want 507 (body: %s)", st, resp)
	}
	if got := h.volSet.Pool("main").Usage(); got != 6 {
		t.Fatalf("main 池 Usage()=%d, want 6（507 不超计）", got)
	}
}

// TestWrapperQuota_ChainedNested_AggregatesToBase 链式嵌套（AllowWrapper 语义）：wrapA 占
// main/videos，wrapB 占 wrapA/xyz（mount 在 wrapA 的子 Scope 上）——上传 wrapB 经三层父链
// 聚合到原始卷 main 池。
func TestWrapperQuota_ChainedNested_AggregatesToBase(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperQuotaHandlers(t, 0) // main 不限
	uvmux := userVolWrap(h, "alice")
	fmux := actorFileWriteMux(h, "alice")

	if rec := wrapperVol(t, uvmux, "wrapA", "videos", 0); rec.Code != http.StatusOK {
		t.Fatalf("创建 wrapA = %d (body: %s)", rec.Code, rec.Body.String())
	}
	// wrapB 底层为 wrapA（嵌套封装卷也可作底层，链式叠加）。
	rec := postUserVol(t, uvmux, map[string]any{
		"name": "wrapB", "type": nestedRootType,
		"extra": map[string]any{"root": t.TempDir(), "target": "wrapA/xyz"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("创建 wrapB（链式嵌套）= %d (body: %s)", rec.Code, rec.Body.String())
	}
	body := []byte("chained wrapped payload")
	if st, resp := writeMuxUpload(t, fmux, "deep.bin", "wrapB", body); st != http.StatusOK {
		t.Fatalf("上传 wrapB/deep.bin = %d, want 200 (body: %s)", st, resp)
	}
	if got := h.volSet.Pool("main").Usage(); got != int64(len(body)) {
		t.Fatalf("链式上传后 main 池 Usage()=%d, want %d（三层父链聚合）", got, len(body))
	}
	if got := h.volSet.Pool("wrapB").Usage(); got != int64(len(body)) {
		t.Fatalf("wrapB 委托池 Usage()=%d, want %d", got, len(body))
	}
}
