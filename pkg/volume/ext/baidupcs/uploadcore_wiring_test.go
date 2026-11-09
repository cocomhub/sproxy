// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// uploadcore_wiring_test.go —— 锁定 `UploadCore` 开关语义与降级行为（离线；不发任何请求）。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

// wiringFakeAdapter 记录 Upload 调用（其余方法不参与上传一步）。
type wiringFakeAdapter struct {
	uploads int
	err     error
	// badClient 为真时 coreClient() 返回 nil（模拟"无可用库 client"的卷，如 binary-only）。
	badClient bool
}

func (c *wiringFakeAdapter) Upload(context.Context, string, string, bool) error {
	c.uploads++
	return c.err
}
func (c *wiringFakeAdapter) Download(context.Context, string, string) error { return ErrUnsupported }
func (c *wiringFakeAdapter) Move(context.Context, string, string) error     { return ErrUnsupported }
func (c *wiringFakeAdapter) Copy(context.Context, string, string) error     { return ErrUnsupported }

// coreClient/coreRepair 实现 coreClientProvider（让内核"可用"，从而能验证降级路径）。
func (c *wiringFakeAdapter) coreClient() *Client {
	if c.badClient {
		return nil
	}
	cl, _ := NewClient("fake-bduss-for-wiring-test", "")
	return cl
}

func (c *wiringFakeAdapter) coreRepair() func(context.Context, string, *stagedUpload) (bool, error) {
	return func(context.Context, string, *stagedUpload) (bool, error) { return true, nil }
}

func newWiringStorage(t *testing.T, mode string, ad Adapter) *Storage {
	t.Helper()
	cfg := StorageConfig{Root: "/r", TempDir: t.TempDir(), Adapter: ad, UploadCore: mode,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st, err := NewStorage(cfg)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return st
}

// TestNormalizeUploadCoreMode 归一化：空/未知 ⇒ auto（硬规则 7c：默认空字符串）。
func TestNormalizeUploadCoreMode(t *testing.T) {
	t.Parallel()
	cases := map[string]string{"": "auto", "  ": "auto", "auto": "auto", "AUTO": "auto",
		"nucleus": "nucleus", "legacy": "legacy", "bogus": "auto"}
	for in, want := range cases {
		if got := normalizeUploadCoreMode(in); got != want {
			t.Fatalf("normalize(%q)=%q want %q", in, got, want)
		}
	}
}

// TestUploadStaged_LegacyModeUsesAdapter legacy 模式必须直接走既有 adapter（不碰内核）。
func TestUploadStaged_LegacyModeUsesAdapter(t *testing.T) {
	t.Parallel()
	ad := &wiringFakeAdapter{}
	st := newWiringStorage(t, "legacy", ad)
	if err := st.uploadStaged(context.Background(), "k", "/r/k", &stagedUpload{tmpPath: "/tmp/x", size: 1}); err != nil {
		t.Fatalf("legacy 应走 adapter 成功: %v", err)
	}
	if ad.uploads != 1 {
		t.Fatalf("adapter.Upload 应被调用 1 次，got %d", ad.uploads)
	}
}

// TestUploadStaged_NucleusWithoutLibraryFails nucleus 模式下无库 adapter 必须**显式报错**
// （不得静默降级 —— nucleus 语义是"行为最可预测"）。
func TestUploadStaged_NucleusWithoutLibraryFails(t *testing.T) {
	t.Parallel()
	ad := &wiringFakeAdapter{}
	st := newWiringStorage(t, "nucleus", ad)
	err := st.uploadStaged(context.Background(), "k", "/r/k", &stagedUpload{tmpPath: "/tmp/x", size: 1})
	if err == nil {
		t.Fatal("nucleus 且无库 adapter 应报错")
	}
	if ad.uploads != 0 {
		t.Fatalf("报错时不得回退 adapter，got %d 次", ad.uploads)
	}
}

// TestUploadStaged_AutoFallsBackAndDisables 关键：auto 模式下内核失败 ⇒ 降级既有路径 + 置位标记。
// 用「不存在的暂存文件」让内核在 planRapid 阶段失败（**不发任何请求**）。
func TestUploadStaged_AutoFallsBackAndDisables(t *testing.T) {
	t.Parallel()
	ad := &wiringFakeAdapter{}
	st := newWiringStorage(t, "auto", ad)
	missing := filepath.Join(t.TempDir(), "missing.bin")
	if err := st.uploadStaged(context.Background(), "k", "/r/k",
		&stagedUpload{tmpPath: missing, size: 10, md5: "aa"}); err != nil {
		t.Fatalf("内核失败后应降级既有路径并成功: %v", err)
	}
	if ad.uploads == 0 {
		t.Fatal("降级后必须调用既有 adapter")
	}
	if !st.coreDisabled.Load() {
		t.Fatal("auto 模式内核失败一次后必须置位降级标记（避免每个文件都重踩）")
	}
	// 已降级 ⇒ 后续上传不再尝试内核（adapter 再被调用一次）。
	if err := st.uploadStaged(context.Background(), "k2", "/r/k2",
		&stagedUpload{tmpPath: missing, size: 10, md5: "bb"}); err != nil {
		t.Fatalf("降级后应直接成功: %v", err)
	}
	if ad.uploads != 2 {
		t.Fatalf("降级后两次都应走 adapter，got %d", ad.uploads)
	}
}

// TestCoreDepsOf_BinaryAdapterTransparent binaryAdapter 必须经 Fallback 透传内核依赖。
func TestCoreDepsOf_BinaryAdapterTransparent(t *testing.T) {
	t.Parallel()
	cl, err := NewClient("fake-bduss-for-wiring-test", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	lib := newLibraryAdapter(cl, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ba := newBinaryAdapter(AdapterConfig{BinaryPath: filepath.Join(t.TempDir(), "nope"), Fallback: lib})
	st := newWiringStorage(t, "auto", ba)
	pcs, repair, ok := st.coreDepsOf()
	if !ok || pcs == nil || repair == nil {
		t.Fatalf("binaryAdapter 应透传内核依赖，got ok=%v pcs=%v repair!=nil=%v", ok, pcs, repair != nil)
	}
}

// TestUploadStaged_AdapterErrorPropagates 既有路径错误必须原样传播（不得被开关吞掉）。
func TestUploadStaged_AdapterErrorPropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	ad := &wiringFakeAdapter{err: boom}
	st := newWiringStorage(t, "legacy", ad)
	if err := st.uploadStaged(context.Background(), "k", "/r/k", &stagedUpload{tmpPath: "/tmp/x", size: 1}); !errors.Is(err, boom) {
		t.Fatalf("应传播 adapter 错误，got %v", err)
	}
}

// TestUploadCoreWAL_NoAuditUsesNop 未装配 audit ⇒ 用空 WAL（流程一致、无跨进程续传）。
func TestUploadCoreWAL_NoAuditUsesNop(t *testing.T) {
	t.Parallel()
	st := newWiringStorage(t, "auto", &wiringFakeAdapter{})
	rec, resume, err := st.uploadCoreWAL(context.Background(), "k")
	if err != nil {
		t.Fatalf("uploadCoreWAL: %v", err)
	}
	if rec == nil || resume != nil {
		t.Fatalf("无 audit 时应返回空 WAL 且无恢复态，got rec=%v resume=%v", rec, resume)
	}
	// 空 WAL 必须满足内核契约且调用不 panic。
	if err := rec.Stage(context.Background(), "x", nil); err != nil {
		t.Fatalf("NopRecorder.Stage: %v", err)
	}
	if err := rec.MarkDone(context.Background()); err != nil {
		t.Fatalf("NopRecorder.MarkDone: %v", err)
	}
	if rec == nil {
		t.Fatal("WAL 不得为 nil（内核要求非 nil）")
	}
}

// TestUploadCoreStats_PathCounting 锁定上传内核的**路径分因计数**（生产可观测：
// "内核到底在不在跑、为何没跑"）。四类分因各一例，全离线（不发网络请求）。
func TestUploadCoreStats_PathCounting(t *testing.T) {
	t.Parallel()

	// ① legacy：显式关闭内核
	ad1 := &wiringFakeAdapter{}
	st1 := newWiringStorage(t, "legacy", ad1)
	if err := st1.uploadStaged(context.Background(), "k", "/r/k",
		&stagedUpload{tmpPath: "/tmp/x", size: 1}); err != nil {
		t.Fatalf("legacy: %v", err)
	}
	if s := st1.UploadCoreStats(); s.LegacyMode != 1 || s.Used != 0 {
		t.Fatalf("legacy 计数不符: %+v", s)
	}

	// ② auto + 内核失败（不存在的暂存文件 ⇒ planRapid 阶段失败）：Used+Failed+Degraded 各 1
	ad3 := &wiringFakeAdapter{}
	st3 := newWiringStorage(t, "auto", ad3)
	if err := st3.uploadStaged(context.Background(), "k", "/r/k",
		&stagedUpload{tmpPath: "/nonexistent", size: 1024}); err != nil {
		t.Fatalf("auto 失败应降级并成功返回: %v", err)
	}
	if s := st3.UploadCoreStats(); s.Used != 1 || s.Failed != 1 || s.Degraded != 1 {
		t.Fatalf("降级计数不符: %+v", s)
	}
	if !st3.CoreDisabled() {
		t.Fatal("内核失败后应置位 coreDisabled")
	}

	// ③ 缺库 client（coreOK=false）⇒ NoDeps 计数（wiringFakeAdapter 用非法 BDUSS ⇒ NewClient 失败）
	ad4 := &wiringFakeAdapter{badClient: true}
	st4 := newWiringStorage(t, "auto", ad4)
	if err := st4.uploadStaged(context.Background(), "k", "/r/k",
		&stagedUpload{tmpPath: "/nonexistent", size: 1024}); err != nil {
		t.Fatalf("无库依赖应走既有路径: %v", err)
	}
	if s := st4.UploadCoreStats(); s.NoDeps != 1 || s.Used != 0 {
		t.Fatalf("无库依赖计数不符: %+v", s)
	}
}
