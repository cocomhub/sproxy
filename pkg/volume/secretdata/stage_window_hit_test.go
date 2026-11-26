// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// stage_window_hit_test.go 锁「窗口缓存命中」的**返回契约**：
//
// 历史缺陷（2026-11-26 定位，CI 偶发 `blocklet 段 0 越出合并区间（rel=262 enc=262172 span=262144）`）：
//   - `windowCacheRead` 返回的是**已解密的明文**；
//   - 但 `segmentSpanFetch` 把它当作**密文 span** 原样返回（spanOff=0）；
//   - 调用方 `decryptSegmentWindows` 于是把明文再按 blocklet EncSize 切一段当密文解 ⇒
//     `rel + EncSize > len(span)` ⇒ 报「越出合并区间」。
//
// 触发条件是**时序**：`stageChunkRead` 的等待恰好超时（返回 false 回源），而窗口在此之后
// 立刻就绪 ⇒ 走到 `segmentSpanFetch` 的窗口缓存分支。CI 高并行负载下偶发（本地 -count=30 全绿）。
//
// 因此这里不依赖时序，直接锁定**契约**：命中窗口缓存时必须回「明文 + 命中标记」，
// 绝不能把明文塞进密文 span 字段。

import (
	"bytes"
	"context"
	"io"
	"path"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestSegmentSpanFetch_WindowHitReturnsPlaintext 锁契约：
// 窗口缓存已就绪时，`segmentSpanFetch` 必须报告「命中」，且返回内容与直接解密一致。
func TestSegmentSpanFetch_WindowHitReturnsPlaintext(t *testing.T) {
	t.Parallel()
	fs, _, content := newCacheEnv(t, -1)
	stageDir := t.TempDir()
	fs.stageDL = newStageDL(stageDir, 64<<20, fs)

	// 首读：触发窗口下载并落盘（之后 ready-only 查询必然命中）。
	rc, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, 256<<10)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	if _, rerr := io.ReadAll(rc); rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	_ = rc.Close()
	waitWindows(t, func() bool { return countCacheFiles(stageDir) > 0 }, "窗口缓存落盘")

	// 构造与整流路径相同的入参（blocklet 段读的 o/targets）。
	o, targets := rangeOptsFor(t, fs, "big.bin", 0, 256<<10)

	// 直接用「ready-only」窗口缓存读口验证契约：命中时返回的必须是**明文**。
	plain, hit := fs.windowCacheRead(o, 0, 256<<10)
	if !hit {
		t.Fatalf("窗口已就绪但 windowCacheRead 未命中（夹具问题）")
	}
	if !bytes.Equal(plain, content[:256<<10]) {
		t.Fatalf("windowCacheRead 命中结果与明文不符（len=%d want=%d）", len(plain), 256<<10)
	}
	// 明文长度 = 请求区间长度；密文长度会多出 GCM 开销（EncSize > Size）。
	if int64(len(plain)) != 256<<10 {
		t.Fatalf("windowCacheRead 应返回请求长度的明文（got %d），密文形态会多出 GCM 开销", len(plain))
	}
	_ = targets
}

// TestReadChunkRange_WindowReadyNotDoubleDecrypted 是端到端回归：
// 窗口缓存就绪后，逐 blocklet 段读必须**成功且内容正确**（此前会报「越出合并区间」）。
//
// 为了不依赖「等待超时」这个时序，这里直接调用段读入口（`readChunkRangeBySegments` 的
// 整流分支由 `segmentSpanFetch` 决定），并把 stageDL 置为「窗口就绪但整块读路径强制失败」
// ——通过把 budget 压到 0 之外的方式不可行，故改用断言 `segmentSpanFetch` 的返回：
// 命中时必须给 plain 而非 span（见上一个用例）。本用例补充「内容对得上」的用户视角断言。
func TestReadChunkRange_WindowReadyNotDoubleDecrypted(t *testing.T) {
	t.Parallel()
	fs, _, content := newCacheEnv(t, -1)
	stageDir := t.TempDir()
	fs.stageDL = newStageDL(stageDir, 64<<20, fs)

	rc, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, 64<<10)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	if _, rerr := io.ReadAll(rc); rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	_ = rc.Close()
	waitWindows(t, func() bool { return countCacheFiles(stageDir) > 0 }, "窗口缓存落盘")

	// 二轮读（窗口全就绪）：内容必须仍然正确。
	rc2, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, 64<<10)
	if err != nil {
		t.Fatalf("第二轮 OpenRangeRead: %v", err)
	}
	got, rerr := io.ReadAll(rc2)
	_ = rc2.Close()
	if rerr != nil {
		t.Fatalf("第二轮 ReadAll（窗口就绪后不得因双重解密失败）: %v", rerr)
	}
	if !bytes.Equal(got, content[:64<<10]) {
		t.Fatal("第二轮内容不符")
	}
}

// rangeOptsFor 构造 blocklet 段读入参（与 readChunkRangeBytesUncached 同源）：
// 从真实索引条目取 chunkInfo / keyBytes / salt，使断言走**生产同一条**数据。
func rangeOptsFor(t *testing.T, fs *SecretdataFS, rel string, offset, end int64) (chunkRangeOpts, []shardseal.BlockletInfo) {
	t.Helper()
	fs.mu.RLock()
	e, ok := fs.index[rel]
	fs.mu.RUnlock()
	if !ok || e.meta == nil || len(e.meta.Chunks) == 0 {
		t.Fatalf("夹具缺少 %q 的 chunk 元信息", rel)
	}
	ci := e.meta.Chunks[0]
	keyBytes, salt, kerr := fs.fileLevelKey(e)
	if kerr != nil {
		t.Fatalf("fileLevelKey: %v", kerr)
	}
	o := chunkRangeOpts{e: e, keyBytes: keyBytes, salt: salt, ci: ci, rel: rel}
	return o, selectRangeBlocklets(ci.Blocklets, offset, end)
}

// TestSegmentSpanFetch_WindowHit_NotFedToCiphertextPath 是**缺陷复现**：
// 复刻 `readChunkRangeBySegments` 的调用序列（segmentSpanFetch → decryptSegmentWindows），
// 在窗口缓存已就绪时断言不报错且内容正确。
//
// 历史缺陷：`segmentSpanFetch` 在窗口缓存命中时返回**已解密明文**，但调用方仍把它当密文
// 送进 `decryptSegmentWindows` ⇒ `rel + EncSize > len(span)` ⇒
// 「blocklet 段 0 越出合并区间（rel=262 enc=262172 span=262144）」（CI 偶发）。
func TestSegmentSpanFetch_WindowHit_NotFedToCiphertextPath(t *testing.T) {
	t.Parallel()
	fs, _, content := newCacheEnv(t, -1)
	stageDir := t.TempDir()
	fs.stageDL = newStageDL(stageDir, 64<<20, fs)

	const off, size = int64(0), int64(256 << 10)
	rc, err := fs.OpenRangeRead(context.Background(), "big.bin", off, size)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	if _, rerr := io.ReadAll(rc); rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	_ = rc.Close()
	waitWindows(t, func() bool { return countCacheFiles(stageDir) > 0 }, "窗口缓存落盘")

	o, targets := rangeOptsFor(t, fs, "big.bin", off, off+size)
	rr := syncpkg.AssertRangeReader(fs.inner)
	if rr == nil {
		t.Fatal("夹具底层应支持 RangeReader")
	}
	segPath := path.Join(fs.dataSeg(o.e), o.ci.FileName)

	// 窗口缓存此时必命中 ⇒ 走的就是有缺陷的那条分支。
	fr, ok, serr := fs.segmentSpanFetch(context.Background(), rr, segPath, o, off, off+size, targets)
	if serr != nil || !ok {
		t.Fatalf("segmentSpanFetch: ok=%v err=%v", ok, serr)
	}
	// 契约：命中窗口缓存时 **必须** 标记 hit 且给出明文，**不得**把明文塞进密文 span。
	if !fr.hit {
		t.Fatalf("窗口已就绪却未标记命中（会走密文路径）")
	}
	if fr.span != nil {
		t.Fatalf("命中时不应同时给出密文 span（语义混淆的根源）")
	}
	// 复刻调用方动作。
	out := fr.plain
	if !fr.hit {
		var derr error
		out, derr = decryptSegmentWindows(o, fr.span, fr.spanOff, off, off+size, targets)
		if derr != nil {
			t.Fatalf("密文路径段读失败: %v", derr)
		}
	}
	if !bytes.Equal(out, content[off:off+size]) {
		t.Fatalf("段读内容不符（len=%d want=%d）", len(out), size)
	}
}
