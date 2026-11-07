// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// failAfterWriteFS 包装内层 FS：对满足 pred 的目标路径**先落盘再返回错误**，模拟 baidupcs
// 「上传成功但复核失败」（write-then-fail——内层报错时对象可能已在远端提交）。用于断言失败
// 回滚不留下远端 blob 孤儿（P2/F3，2026-10-19）。
type failAfterWriteFS struct {
	syncpkg.FS
	pred  func(p string) bool
	mu    sync.Mutex // 块级并发上传后可能多 goroutine 同时写：fired 必须加锁
	fired bool
}

func (f *failAfterWriteFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	if err := f.FS.WriteFile(ctx, key, p, r, size, mtime); err != nil {
		return err
	}
	if f.pred(p) {
		f.mu.Lock()
		f.fired = true
		f.mu.Unlock()
		return errors.New("fail-after-write: 内层写成功但复核失败")
	}
	return nil
}

// firedAt 是 fired 的并发安全读取。
func (f *failAfterWriteFS) firedAt() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

var _ syncpkg.FS = (*failAfterWriteFS)(nil)

// seekableSource 把 payload 写成临时文件并返回可 seek 句柄（走流式写路径：canStream 且非
// Dedup/Erasure）。
func seekableSource(t *testing.T, payload []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(p, payload, 0o644); err != nil {
		t.Fatalf("写源: %v", err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("打开源: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// residualDataBlobs 遍历底层根目录，返回**非目录 meta**的残留 blob（分块 + file meta）。
// 目录 meta 是路径解析锚点（容器语义），不计入孤儿。
func residualDataBlobs(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if shardseal.ClassifyName(d.Name()) != shardseal.KindDirMeta {
			out = append(out, d.Name())
		}
		return nil
	})
	return out
}

// TestWriteFileStream_MetaUploadFail_NoMetaOrphan（F3，2026-10-19）：流式写路径在**数据分块
// 全部上传成功后**上传 meta 时若内层「写成功但复核失败」，失败分支必须回滚删掉已上传分块与
// meta——此前失败用例只覆盖分块上传分支（`differential_target_test.go`），meta 上传（`secretdata.go`
// 的 `iu.uploaded = append(... metaPath)` + `uploadBlob`）的 write-then-fail 无用例 ⇒ 若去掉
// 「先登记后上传」或回滚，本用例必红。
func TestWriteFileStream_MetaUploadFail_NoMetaOrphan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	inner := &failAfterWriteFS{FS: syncpkg.NewLocalFS(root, nil), pred: func(p string) bool {
		return shardseal.IsMetaName(path.Base(p))
	}}
	sfs, err := NewFS(inner, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block: shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if merr := sfs.MakeDir(ctx, "d"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	payload := bytes.Repeat([]byte("meta-fail-"), 16*1024) // 128KiB / 32KiB = 4 块
	src := seekableSource(t, payload)
	werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", src, int64(len(payload)), 0)
	if werr == nil {
		t.Fatal("meta 上传 write-then-fail ⇒ WriteFile 必须失败 ✗")
	}
	if !strings.Contains(werr.Error(), "上传 meta 失败") {
		t.Fatalf("夹具未命中流式 meta 上传失败分支: %v", werr)
	}
	if !inner.firedAt() {
		t.Fatal("夹具失效：未观测到 file meta 的内层写")
	}
	sfs.mu.RLock()
	e := sfs.index["d/f.bin"]
	sfs.mu.RUnlock()
	if e != nil {
		t.Fatal("meta 上传失败后不得留下索引条目（半态）✗")
	}
	if orphans := residualDataBlobs(t, root); len(orphans) != 0 {
		t.Fatalf("meta 上传失败回滚后底层残留 %d 个非目录 blob（远端孤儿）✗: %v", len(orphans), orphans)
	}
}

// TestWriteFileStream_MemoryDeliveryUploadFail_NoChunkOrphan（F3，2026-10-19）：回退链②内存
// 交付（本地 staging 不可用 + 源可重读）下的**分块上传** write-then-fail——此前失败用例只覆盖
// 本地路径交付分支；内存交付 sinks（`iu.uploaded = append(...)` + `uploadBlob`）的失败回滚无用例。
// 若内存交付 sink 去掉「先登记后上传」或失败回滚，本用例必红。
func TestWriteFileStream_MemoryDeliveryUploadFail_NoChunkOrphan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	inner := &failAfterWriteFS{FS: syncpkg.NewLocalFS(root, nil), pred: func(p string) bool {
		return shardseal.ClassifyName(path.Base(p)) == shardseal.KindChunk
	}}
	sfs, err := NewFS(inner, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block: shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if merr := sfs.MakeDir(ctx, "d"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	q := &fakeStagingQuota{limit: 1} // 任何预留给失败 ⇒ 回退链②内存交付（源可重读）
	sfs.WithStagingQuota(q)
	payload := bytes.Repeat([]byte("mem-sink-fail-"), 16*1024) // 128KiB / 32KiB = 4 块
	src := seekableSource(t, payload)
	werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", src, int64(len(payload)), 0)
	if werr == nil {
		t.Fatal("内存交付下分块上传 write-then-fail ⇒ WriteFile 必须失败 ✗")
	}
	// 块级并发后错误由 iu.finish() 统一汇总（文案「分块上传失败」），仍须是分块上传失败。
	if !strings.Contains(werr.Error(), "分块上传") {
		t.Fatalf("夹具未命中内存交付分块上传失败分支: %v", werr)
	}
	if !inner.firedAt() {
		t.Fatal("夹具失效：未观测到数据分块的内层写")
	}
	if _, _, calls := q.snapshot(); calls == 0 {
		t.Fatal("探针未生效：ReserveUsage 未被调用（判据无效）")
	}
	if orphans := residualDataBlobs(t, root); len(orphans) != 0 {
		t.Fatalf("内存交付失败回滚后底层残留 %d 个非目录 blob（远端孤儿）✗: %v", len(orphans), orphans)
	}
}

// TestStagingReserveInterleaved_NoMetaPadOverReserve（F9/P2，2026-10-19）：块级交错路径的
// staging 预留 = **单块密文上界**（cipherPeak(min(size, policy.Max))）——该路径 meta 走内存态
// 直传、不落 temp，**不得叠加 2×metaPad**（默认 1–2MiB），否则小块文件预留 3–5× 实际 temp、
// 触发不必要的内存降级。
//
// 判据：把配额限额**恰设为单块密文上界**——去掉 meta pad 即成功且峰值 == 限额；若仍叠加
// meta pad ⇒ 预留超限 ⇒ 回退内存交付（峰值 0、无本地交付）⇒ 本用例必红。
func TestStagingReserveInterleaved_NoMetaPadOverReserve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tempDir := t.TempDir()
	probe := &interleaveProbeFS{FS: syncpkg.NewLocalFS(t.TempDir(), nil), tempDir: tempDir, stageReads: 1}
	sfs, err := NewFS(probe, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: tempDir,
		Block: shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	want := cipherPeak(32 << 10)
	q := &fakeStagingQuota{limit: want}
	sfs.WithStagingQuota(q)
	payload := bytes.Repeat([]byte("interleave-alloc-"), 16*1024) // 128KiB / 32KiB = 4 块
	src := seekableSource(t, payload)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", src, int64(len(payload)), 0); werr != nil {
		t.Fatalf("限额恰为单块密文上界时写应成功（不得被 meta pad 顶爆）: %v", werr)
	}
	used, peak, _ := q.snapshot()
	if peak != want {
		t.Fatalf("交错预留峰值 = %d，应为单块密文上界 %d（不得叠加 meta pad）", peak, want)
	}
	if used != 0 {
		t.Fatalf("写后预留未释放: used=%d", used)
	}
	staged := 0
	for _, rec := range probe.recordsSnapshot() {
		if rec.staged {
			staged++
		}
	}
	if staged == 0 {
		t.Fatal("夹具失效：未观测到本地路径交付（应为交错本地交付，而非内存降级）")
	}
}

// TestStagingReserve_ErasureAccountsParity（F1/P2，2026-10-19）：纠错卷的本地 temp 除数据分块
// 密文外还会落**一个 parity 段**（`writeErasureParity` → `encryptContentSingle`），此前
// `stagingReserve` 只按数据密文上界记账 ⇒ 低估（2 分块文件可低估约 50%）。修后预留 =
// cipherPeak(size) + cipherPeak(policy.Max) + 2×metaPad。
//
// 判据：MetaPadBytes=1 使 metaPad 恒为 196（RandN(1)=0）⇒ 预留值确定；去掉 parity 项 ⇒ 本用例必红。
func TestStagingReserve_ErasureAccountsParity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil), Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block:   shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
		Erasure: true, MetaPadBytes: 1,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if mp := sfs.metaPadTarget(); mp != 196 {
		t.Fatalf("夹具前提失效：MetaPadBytes=1 时 metaPadTarget 应为 196，实测 %d", mp)
	}
	q := &fakeStagingQuota{limit: 1 << 30}
	sfs.WithStagingQuota(q)
	payload := bytes.Repeat([]byte("parity-alloc-"), 1024) // 16KiB（内存写路径：非 seek 源）
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "p/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	want := cipherPeak(int64(len(payload))) + cipherPeak(32<<10) + 2*196
	if _, peak, _ := q.snapshot(); peak != want {
		t.Fatalf("纠错卷 staging 预留峰值 = %d，应含 parity 上界 %d", peak, want)
	}
}

// TestWriteFileStream_MemoryDeliveryUploadFail_FastFailLabelStable 是 CI flake 回归：块级并发下
// 「分块上传先于加密完成而失败」会走 sink 的 fast-fail 分支（submit 返回 firstErr / 队列已中止），
// 修复前该分支被归类为「流式分块加密失败」，与「分块上传失败」路径同一根因两种文案 ⇒ 同一用例
// 因时序不同而偶发失败（CI #1052/#1053 各命中一次）。
//
// 用 UploadConcurrency=1 + 多块，使首块失败后 sink 必然继续 submit 后续块 ⇒ 确定性命中
// fast-fail 分支；断言归类恒为「分块上传失败」。
func TestWriteFileStream_MemoryDeliveryUploadFail_FastFailLabelStable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	inner := &failAfterWriteFS{FS: syncpkg.NewLocalFS(root, nil), pred: func(p string) bool {
		return shardseal.ClassifyName(path.Base(p)) == shardseal.KindChunk
	}}
	sfs, err := NewFS(inner, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block:             shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
		UploadConcurrency: 1, // 串行 worker ⇒ 首块失败后 sink 仍会 submit 后续块（fast-fail 分支）
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if merr := sfs.MakeDir(ctx, "d"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	q := &fakeStagingQuota{limit: 1} // 任何预留给失败 ⇒ 回退链②内存交付（源可重读）
	sfs.WithStagingQuota(q)
	payload := bytes.Repeat([]byte("fast-fail-label-"), 64*1024) // 16B × 64Ki = 1MiB / 32KiB = 32 块
	src := seekableSource(t, payload)
	werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/ff.bin", src, int64(len(payload)), 0)
	if werr == nil {
		t.Fatal("内存交付下分块上传 write-then-fail ⇒ WriteFile 必须失败 ✗")
	}
	if !strings.Contains(werr.Error(), "分块上传失败") {
		t.Fatalf("fast-fail 分支也必须归类为「分块上传失败」（不得因时序报「流式分块加密失败」）: %v", werr)
	}
	if !inner.firedAt() {
		t.Fatal("夹具失效：未观测到数据分块的内层写")
	}
	if orphans := residualDataBlobs(t, root); len(orphans) != 0 {
		t.Fatalf("fast-fail 失败回滚后底层残留 %d 个非目录 blob（远端孤儿）✗: %v", len(orphans), orphans)
	}
}
