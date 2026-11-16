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
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// countingInvisibleFS 包装 LocalFS 实现 BatchVerifier：恒返回「不可见」（模拟百度新目录树的
// 元数据一致性延迟），并记录调用次数 + 每次 ctx 是否带**有界** deadline。
type countingInvisibleFS struct {
	syncpkg.FS
	mu         sync.Mutex
	calls      int
	allBounded bool // 首次调用初始化；任一调用无 deadline 或 deadline 过宽 ⇒ false
}

func (f *countingInvisibleFS) VerifyMany(ctx context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	dl, ok := ctx.Deadline()
	f.mu.Lock()
	if f.calls == 0 {
		f.allBounded = true
	}
	f.calls++
	if !ok || time.Until(dl) > 5*verifyBatchTimeout {
		f.allBounded = false
	}
	f.mu.Unlock()
	out := make([]syncpkg.VerifyResult, len(items))
	for i, it := range items {
		out[i] = syncpkg.VerifyResult{Item: it, State: syncpkg.VerifyInvisible, Reason: "夹具：恒不可见"}
	}
	return out
}

func (f *countingInvisibleFS) stats() (calls int, allBounded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.allBounded
}

// TestVerifyBatch_BoundedSinglePassPerAttempt 锁定批量校验的**有界 + 每轮一次**语义
// （2026-11-17 用户纪律：不得出现「几十轮重传 / 5 分钟无进展」）：
//   - 后端恒「不可见」⇒ 每次写入尝试只调 **1 次** VerifyMany（旧实现固定 3 轮，把耗时放大到分钟级）；
//   - 整文件 **覆盖写重传一次**（源可重读）⇒ 共 **2 次** 上传 / 2 次校验；
//   - 每次 VerifyMany 的 ctx 都带**有界** deadline（`verifyBatchTimeout`）。
//
// 变异：把单次校验改回 `rounds=3` 循环 ⇒ 调用次数变 6（本用例红）；删掉 `verifyManyBounded`
// 的 `WithTimeout` ⇒ allBounded=false（红）。
func TestVerifyBatch_BoundedSinglePassPerAttempt(t *testing.T) {
	t.Parallel()
	inner := &countingInvisibleFS{FS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	sfs, err := NewFS(inner, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block:             shardseal.BlockPolicy{Mode: "random", Min: 32 << 10, Max: 32 << 10},
		UploadConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	payload := bytes.Repeat([]byte("bounded-verify-"), 16*1024) // 224 KiB / 32 KiB ≈ 7 块
	src, serr := os.CreateTemp(t.TempDir(), "src-*")
	if serr != nil {
		t.Fatalf("create src: %v", serr)
	}
	defer func() { _ = src.Close() }()
	if _, werr := src.Write(payload); werr != nil {
		t.Fatalf("write src: %v", werr)
	}
	if _, serr2 := src.Seek(0, io.SeekStart); serr2 != nil {
		t.Fatalf("seek src: %v", serr2)
	}

	werr := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "d/f.bin", src, int64(len(payload)), 0)
	if !errors.Is(werr, ErrBatchVerifyFailed) {
		t.Fatalf("恒不可见应返回 ErrBatchVerifyFailed，got %v", werr)
	}
	calls, allBounded := inner.stats()
	if calls != 2 {
		t.Fatalf("期望恰好 2 次 VerifyMany（首传 + 覆盖写重传各 1 次），got %d（旧实现为 3 轮/次）", calls)
	}
	if !allBounded {
		t.Fatalf("每次 VerifyMany 必须带**有界** deadline（批量校验不得被后端可见性等待拖到分钟级）")
	}
}

// TestWriteFile_RetryViaSeekableNonPathSource 锁定「覆盖写重传」对**非本地文件路径的可重读源**
// 也可达：HTTP multipart 的**内存** seekable 源不是 `*os.File`（`replayableSourcePath` 返回 ""），
// 但 `writeFileOpt` 只在 `canStream(r)` 时路由到流式路径 ⇒ 它必然可 Seek 回起点重读
// ⇒ 必须仍能自动重传（用户实测：百度首次上传后远端元数据未收敛，再传一次即一致）。
//
// 变异：把重传条件改回「必须 `replayableSourcePath != ""`」（删掉 Seek 分支）⇒ 本用例红
// （首轮篡改后不重传，写入失败）。
func TestWriteFile_RetryViaSeekableNonPathSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := &tamperOnceFS{verifiableFS: &verifiableFS{FS: syncpkg.NewLocalFS(root, nil)}}
	sfs, err := NewFS(inner, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block:             shardseal.BlockPolicy{Mode: "random", Min: 32 << 10, Max: 32 << 10},
		UploadConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	payload := bytes.Repeat([]byte("seek-retry-"), 32*1024) // 352 KiB / 32 KiB ≈ 11 块
	// 源 = io.SectionReader：可 Seek / ReaderAt，但**非** *os.File ⇒ 不可按路径重开。
	src := io.NewSectionReader(bytes.NewReader(payload), 0, int64(len(payload)))

	werr := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "d/f.bin", src, int64(len(payload)), 0)
	if !inner.didTamper() {
		t.Fatal("夹具失效：未触发分块篡改")
	}
	if werr != nil {
		t.Fatalf("覆盖写重传后应成功（非路径可重读源也必须重传），got err=%v", werr)
	}
	rc, rerr := sfs.OpenRead(context.Background(), "d/f.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	got, rerr2 := io.ReadAll(rc)
	_ = rc.Close()
	if rerr2 != nil {
		t.Fatalf("读回失败: %v", rerr2)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("读回内容与源不一致：%d vs %d 字节", len(got), len(payload))
	}
}

// captureVerifyFS 在 verifiableFS 之上捕获全部 `VerifyItem`（供同源不变量断言）。
type captureVerifyFS struct {
	*verifiableFS
	mu    sync.Mutex
	items []syncpkg.VerifyItem
}

func (f *captureVerifyFS) VerifyMany(ctx context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	f.mu.Lock()
	f.items = append(f.items, items...)
	f.mu.Unlock()
	return f.verifiableFS.VerifyMany(ctx, items)
}

func (f *captureVerifyFS) snapshot() []syncpkg.VerifyItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]syncpkg.VerifyItem, len(f.items))
	copy(out, f.items)
	return out
}

// TestWriteFile_VerifyKeyEqualsMetaChunkName 锁定**三者同源**不变量：批量校验用的 Key
// 必须 == 容器 + meta 记录的 `ChunkInfo.FileName`（**读路径**据此定位 blob）⇒ 「写入名 /
// meta 记录名 / 校验名」恒来自同一来源（加密器对同一个密文 blob 给出的名字），
// 不存在第二处推导（否则会出现「对象已在网盘却报不可见」）。
//
// 同时锁定：每个校验项都带 `BlockMD5s`（大对象与远端 block_list 逐块对账的前提）。
//
// 变异：把校验 Key 改成任意其它推导（如 `path.Join(container, "x")`）⇒ 本用例红。
func TestWriteFile_VerifyKeyEqualsMetaChunkName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := &captureVerifyFS{verifiableFS: &verifiableFS{FS: syncpkg.NewLocalFS(root, nil)}}
	sfs, err := NewFS(inner, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block:             shardseal.BlockPolicy{Mode: "random", Min: 32 << 10, Max: 32 << 10},
		UploadConcurrency: 1, // 串行 ⇒ 登记顺序 == meta.Chunks 顺序
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	payload := bytes.Repeat([]byte("same-source-"), 32*1024) // 384 KiB / 32 KiB ≈ 12 块
	src, serr := os.CreateTemp(t.TempDir(), "src-*")
	if serr != nil {
		t.Fatalf("create src: %v", serr)
	}
	defer func() { _ = src.Close() }()
	if _, werr := src.Write(payload); werr != nil {
		t.Fatalf("write src: %v", werr)
	}
	if _, serr2 := src.Seek(0, io.SeekStart); serr2 != nil {
		t.Fatalf("seek src: %v", serr2)
	}
	if werr := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "d/f.bin", src, int64(len(payload)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}

	sfs.mu.RLock()
	e, ok := sfs.index["d/f.bin"]
	var container string
	var chunkNames []string
	if ok {
		container = e.dirSeg
		for _, c := range e.meta.Chunks {
			chunkNames = append(chunkNames, c.FileName)
		}
	}
	sfs.mu.RUnlock()
	if !ok {
		t.Fatal("索引无条目 d/f.bin")
	}
	items := inner.snapshot()
	if len(items) != len(chunkNames) {
		t.Fatalf("校验项数 %d != meta 分块数 %d", len(items), len(chunkNames))
	}
	for i, it := range items {
		want := path.Join(container, chunkNames[i])
		if it.Key != want {
			t.Fatalf("校验名与 meta 记录名不同源：item[%d]=%q, meta=%q", i, it.Key, want)
		}
		if len(it.BlockMD5s) == 0 {
			t.Fatalf("校验项 %d 缺 BlockMD5s（大对象对账前提）", i)
		}
	}
}
