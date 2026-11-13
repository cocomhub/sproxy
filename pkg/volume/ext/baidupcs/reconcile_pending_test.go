// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testReporter 记录上报（用于断言参数与次数）。
type testReporter struct {
	rels   []string
	copies []string
}

func (r *testReporter) ReportBlobMD5Pending(rel, localCopy string) {
	r.rels = append(r.rels, rel)
	r.copies = append(r.copies, localCopy)
}

// newFinishStagedStorage 造一个仅用于 finishStaged 的 Storage（不触网）。
func newFinishStagedStorage(t *testing.T, rep ReconcileReporter) *Storage {
	t.Helper()
	return &Storage{
		temp:      t.TempDir(),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		reconcile: rep,
	}
}

// mkStage 造一个 stage-* 目录 + 副本文件，返回 stagedUpload。
func mkStage(t *testing.T, s *Storage, key string, content string) (*stagedUpload, string) {
	t.Helper()
	// 用 os.MkdirAll 构造 stage-* 形态的目录（不裸调 os.MkdirTemp：archcheck 的
	// TempDirGate 要求测试改用 testutil.MkdirTemp/t.TempDir；此处需**位于 s.temp 之下**
	// 才能被 finishStaged 的 pending 逻辑覆盖）。目录名确定性派生，避免并发用例互相干扰。
	dir := filepath.Join(s.temp, "stage-"+strings.NewReplacer("/", "_", "\\", "_").Replace(key))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p := filepath.Join(dir, filepath.Base(key))
	if werr := os.WriteFile(p, []byte(content), 0o600); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	return &stagedUpload{tmpPath: p, md5: strings.Repeat("a", 32), blockMD5s: []string{strings.Repeat("a", 32)}}, dir
}

// TestFinishStaged_未置位时删除（零回归） 锁定既有行为：未标记保留 ⇒ 临时目录必须被删除。
func TestFinishStaged_未置位时删除(t *testing.T) {
	t.Parallel()
	rep := &testReporter{}
	s := newFinishStagedStorage(t, rep)
	st, dir := mkStage(t, s, "a/b.bin", "hello")
	s.finishStaged("a/b.bin", st)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("未置位时临时目录应被删除，仍存在: %v", err)
	}
	if len(rep.rels) != 0 {
		t.Fatalf("未置位时不得上报，actual=%v", rep.rels)
	}
}

// TestFinishStaged_置位时保留副本并上报 锁定 ④ 的核心：内容已在云端但 md5 未权威 ⇒
// **保留副本**（重传源）+ **上报事项**（rel/localCopy 必须逐参正确——错位会导致重传打错目标）。
func TestFinishStaged_置位时保留副本并上报(t *testing.T) {
	t.Parallel()
	rep := &testReporter{}
	s := newFinishStagedStorage(t, rep)
	st, dir := mkStage(t, s, "jav/a/b.bin", "payload")
	st.retainForReconcile = true
	s.finishStaged("jav/a/b.bin", st)

	// ① 上报参数正确
	if len(rep.rels) != 1 || rep.rels[0] != "jav/a/b.bin" {
		t.Fatalf("上报 rel 错误: %v", rep.rels)
	}
	copyPath := rep.copies[0]
	// ② 副本在 pending 根下且内容保留
	if !strings.HasPrefix(copyPath, s.pendingRoot()) {
		t.Fatalf("副本应在 pending 根下，actual=%s root=%s", copyPath, s.pendingRoot())
	}
	raw, err := os.ReadFile(copyPath)
	if err != nil || string(raw) != "payload" {
		t.Fatalf("副本内容应保留: err=%v raw=%q", err, raw)
	}
	// ③ 原 stage-* 目录已清理（不重复占盘）
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("原临时目录应被清理，仍存在: %v", err)
	}
	// ④ 启动兜底扫描能复原 key
	pend := s.PendingReconcile()
	if len(pend) != 1 || pend[0].Key != "jav/a/b.bin" || pend[0].LocalCopy != copyPath {
		t.Fatalf("PendingReconcile 复原错误: %+v", pend)
	}
	// ⑤ 终态删除可用，且**只删 pending 根下的目录**
	if rerr := s.RemovePendingReconcile(copyPath); rerr != nil {
		t.Fatalf("RemovePendingReconcile: %v", rerr)
	}
	if len(s.PendingReconcile()) != 0 {
		t.Fatalf("删除后不应再有待复查项")
	}
}

// TestFinishStaged_未注入上报口也要保留副本 锁定降级行为：reporter 为 nil ⇒
// **仍必须保留副本**（不丢重传源）+ 不 panic（仅少一次自动重试）。
func TestFinishStaged_未注入上报口也要保留副本(t *testing.T) {
	t.Parallel()
	s := newFinishStagedStorage(t, nil)
	st, _ := mkStage(t, s, "k.bin", "x")
	st.retainForReconcile = true
	s.finishStaged("k.bin", st)
	if len(s.PendingReconcile()) != 1 {
		t.Fatalf("未注入上报口时也必须保留副本（重传源不得丢失）")
	}
}

// TestRemovePendingReconcile_拒绝越界删除 锁定安全边界：只允许删 pending 根下的目录，
// 防路径穿越/误传导致删除任意目录。
func TestRemovePendingReconcile_拒绝越界删除(t *testing.T) {
	t.Parallel()
	s := newFinishStagedStorage(t, nil)
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePendingReconcile(filepath.Join(outside, "f.bin")); err == nil {
		t.Fatal("越界路径必须被拒绝")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("越界目录不得被删除: %v", err)
	}
}

// TestRetryBlobMD5Reconcile_收敛后清理副本 锁定重试动作的核心契约：
//   - 从**保留的本地副本**重传后复查；远端 md5 已权威（fake adapter 的 ETag=内容 md5）⇒ 返回 nil
//     且**清理保留副本**（终态，防副本堆积）；
//   - 参数缺失/副本不存在 ⇒ **返回错误**（转人工），不得静默成功；
//   - 必须走 `PutFromStagedPath`（只读副本）——若误用 `Put`，会在"未收敛"时**再保留一份副本**
//     ⇒ 副本无限增殖（本用例以"重试后 pending 数量不增加"间接锁定）。
func TestRetryBlobMD5Reconcile_收敛后清理副本(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)

	// 造一份"保留中"的副本（模拟上传后判定 md5 未权威时的保留动作）
	rep := &testReporter{}
	s.reconcile = rep
	st, _ := mkStage(t, s, "jav/x/y.bin", "retry-payload")
	st.retainForReconcile = true
	s.finishStaged("jav/x/y.bin", st)
	if len(s.PendingReconcile()) != 1 {
		t.Fatalf("前置：应有 1 个保留副本")
	}
	copyPath := rep.copies[0]

	// 重试：fake adapter 的 Meta 返回 ETag=内容 md5（权威）⇒ 收敛
	if err := s.RetryBlobMD5Reconcile(t.Context(), "jav/x/y.bin", copyPath); err != nil {
		t.Fatalf("收敛场景应返回 nil，得到 %v", err)
	}
	if n := len(s.PendingReconcile()); n != 0 {
		t.Fatalf("收敛后应清理保留副本，仍有 %d 个", n)
	}
}

// TestRetryBlobMD5Reconcile_缺参或副本丢失必须报错 锁定 fail-closed（绝静默成功）。
func TestRetryBlobMD5Reconcile_缺参或副本丢失必须报错(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	if err := s.RetryBlobMD5Reconcile(t.Context(), "", "/tmp/x"); err == nil {
		t.Error("缺 key 必须报错")
	}
	if err := s.RetryBlobMD5Reconcile(t.Context(), "k", ""); err == nil {
		t.Error("缺副本路径必须报错")
	}
	missing := filepath.Join(t.TempDir(), "not-exist.bin")
	if err := s.RetryBlobMD5Reconcile(t.Context(), "k", missing); err == nil {
		t.Error("副本不存在必须报错（无源可传 ⇒ 转人工）")
	}
}

// fakeQuota 记录预留/释放（实现 syncpkg.StagingQuotaTracker）。
type fakeQuota struct {
	reserved []int64
	released []int64
	failWith error
}

func (f *fakeQuota) ReserveUsage(_ context.Context, size int64) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.reserved = append(f.reserved, size)
	return nil
}
func (f *fakeQuota) ReleaseUsage(size int64) { f.released = append(f.released, size) }

// TestFinishStaged_保留时按字节记账并在清理时释放 锁定方案 A 的配额契约：
//   - 保留 ⇒ `ReserveUsage(副本字节数)`（否则保留期占用不受配额约束）；
//   - 清理 ⇒ `ReleaseUsage(同一字节数)`，且该值**从 pending.json 读回** ⇒ 进程重启后仍正确释放
//     （避免额度泄漏）；
//   - 未注入配额 ⇒ 仍保留副本（不 panic，仅不记账）。
func TestFinishStaged_保留时按字节记账并在清理时释放(t *testing.T) {
	t.Parallel()
	fq := &fakeQuota{}
	s := newFinishStagedStorage(t, nil)
	s.SetStagingQuota(fq)

	st, _ := mkStage(t, s, "k.bin", "0123456789") // 10 字节
	st.retainForReconcile = true
	s.finishStaged("k.bin", st)

	if len(fq.reserved) != 1 || fq.reserved[0] != 10 {
		t.Fatalf("保留时未按字节预留：%v", fq.reserved)
	}
	pend := s.PendingReconcile()
	if len(pend) != 1 {
		t.Fatalf("应保留 1 个副本")
	}
	if err := s.RemovePendingReconcile(pend[0].LocalCopy); err != nil {
		t.Fatalf("清理: %v", err)
	}
	if len(fq.released) != 1 || fq.released[0] != 10 {
		t.Fatalf("清理时未释放同一字节数：%v", fq.released)
	}
}

// TestFinishStaged_配额不足时不保留且不失败上传 锁定：磁盘将满 ⇒ **不保留副本**
// （不让未受管占用累积）且**上传仍成功**（内容已在云端）；上报副本路径为空（交人工）。
func TestFinishStaged_配额不足时不保留且不失败上传(t *testing.T) {
	t.Parallel()
	fq := &fakeQuota{failWith: syncpkgErrNoSpace{}}
	s := newFinishStagedStorage(t, &testReporter{})
	s.SetStagingQuota(fq)

	st, dir := mkStage(t, s, "k.bin", "payload")
	st.retainForReconcile = true
	s.finishStaged("k.bin", st) // 不得 panic、不得阻塞

	if n := len(s.PendingReconcile()); n != 0 {
		t.Fatalf("配额不足时不应保留副本，实际 %d 个", n)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("配额不足时连原临时目录也应清理（避免更占盘），仍存在: %v", err)
	}
}

// syncpkgErrNoSpace 模拟配额实现返回的"无空间"错误（任意非 nil 即可）。
type syncpkgErrNoSpace struct{}

func (syncpkgErrNoSpace) Error() string { return "no space" }

// fakeSample 造一个 sampleReader 替身（返回固定字节或错误）。
func fakeSample(data []byte, err error) func(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return func(context.Context, string, int64, int64) (io.ReadCloser, error) {
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
}

// TestSampleReadback_一致与否的判定 锁定 P0-1 缓解的核心：
//   - 读回块的 md5 与本地分块 md5 相等 ⇒ (ok=true, decidable=true)；
//   - **不相等** ⇒ (ok=false, decidable=true) —— 这是"远端 block_list 一致但内容损坏"的唯一拦截点；
//   - 取不到（无直链/读失败）⇒ (false,false) **不判定**（不得据此判失败）。
func TestSampleReadback_一致与否的判定(t *testing.T) {
	t.Parallel()
	payload := []byte("0123456789abcdef")
	want := md5HexOf(payload) // 本地分块 md5（测试内计算）
	mkSt := func() *stagedUpload {
		return &stagedUpload{size: int64(len(payload)), blockMD5s: []string{want}}
	}
	mkS := func(fn func(context.Context, string, int64, int64) (io.ReadCloser, error)) *Storage {
		s := newFinishStagedStorage(t, nil)
		s.sampleReader = fn
		return s
	}

	// ① 一致
	if ok, dec := mkS(fakeSample(payload, nil)).sampleReadback(context.Background(), "k", mkSt()); !dec || !ok {
		t.Fatalf("一致时应 (ok=true, decidable=true)，实际 ok=%v dec=%v", ok, dec)
	}
	// ② 不一致 ⇒ 必须判定（供重传）
	bad := append([]byte(nil), payload...)
	bad[0] ^= 0xFF
	if ok, dec := mkS(fakeSample(bad, nil)).sampleReadback(context.Background(), "k", mkSt()); !dec || ok {
		t.Fatalf("不一致时应 (ok=false, decidable=true)，实际 ok=%v dec=%v", ok, dec)
	}
	// ③ 取不到 ⇒ 不可判定
	if _, dec := mkS(fakeSample(nil, errors.New("no dlink"))).sampleReadback(context.Background(), "k", mkSt()); dec {
		t.Fatal("取不到样本时必须判不可判定")
	}
	// ④ 未完整装配（无 dlinkCache）⇒ 不可判定且**不 panic**
	s := newFinishStagedStorage(t, nil)
	if _, dec := s.sampleReadback(context.Background(), "k", mkSt()); dec {
		t.Fatal("未完整装配时应判不可判定")
	}
}

// TestAfterUploadByBlockList_抽样不一致判重传 锁定**接线**：block_list 一致但 md5 未权威时，
// 若抽样读回不一致 ⇒ 必须返回 (done=false, decided=true)（走重传），而不是放行。
func TestAfterUploadByBlockList_抽样不一致判重传(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 32)
	payload := []byte("payload-bytes")
	// 本地分块 md5 与读回内容**不符** ⇒ 抽样失败
	s := newFinishStagedStorage(t, nil)
	s.sampleReader = fakeSample([]byte("different-content"), nil)
	st := &stagedUpload{md5: strings.Repeat("f", 32), size: int64(len(payload)), blockMD5s: []string{a}}
	// meta: block_list 与本地一致；md5 未权威（ETag 为空 ⇒ md5Converged=false）⇒ 触发抽样
	done, decided := s.afterUploadByBlockList(context.Background(), "k", &ObjectMeta{BlockList: []string{a}}, st)
	if !decided || done {
		t.Fatalf("抽样不一致应判重传 (done=false, decided=true)，实际 done=%v decided=%v", done, decided)
	}
}

// md5HexOf 计算字节的 md5 hex（测试助手）。
func md5HexOf(b []byte) string {
	h := md5.New() //nolint:gosec // 测试用途
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
