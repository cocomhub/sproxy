// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// stage_downloader_test.go —— 窗口级缓存下载器的正确性锁定（2026-10-25 v2）。
//
// 背景（真机实测）：百度网盘账号级限速 ~7.8MB/s；读路径按 blocklet 聚合窗口独立下载
// 可跳读、且窗口粒度让首窗命中无需等整块。
//
// 断言（各用例）：
//   - 首轮读**触发**目标窗口下载并回源；窗口就绪后二轮**零远端**；
//   - 缓存内容 = 底层原始密文（**不落明文**）；
//   - 窗口缓存被篡改 ⇒ 解密失败 ⇒ 回源（fail-closed）；
//   - 跳读：只下载含目标 blocklet 的窗口（不下载整块/其它窗口）；
//   - 未装配（SpanCacheDir 空）⇒ 零回归。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// waitWindows 轮询等待条件成立（staging 后台下载完成）。
// 用 testutil.WaitFor（条件轮询 2ms）替代手写 Sleep 循环：满足即返回（快），
// 且受 R14 睡眠棘轮约束（手写 Sleep 顶高全仓计数会被 archcheck 拦）。
func waitWindows(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	testutil.WaitFor(t, 10*time.Second, cond, msg)
}

// TestStageDL_WindowCachedThenAllLocal：窗口下载后二轮零远端。
func TestStageDL_WindowCachedThenAllLocal(t *testing.T) {
	t.Parallel()
	fs, inner, content := newCacheEnv(t, -1) // 关闭内存块缓存，只测窗口缓存
	stageDir := t.TempDir()
	fs.stageDL = newStageDL(stageDir, 64<<20, fs)
	if fs.stageDL == nil {
		t.Fatal("stageDL 装配失败")
	}

	readWindow := func(off, size int64) []byte {
		t.Helper()
		rc, err := fs.OpenRangeRead(context.Background(), "big.bin", off, size)
		if err != nil {
			t.Fatalf("OpenRangeRead(%d): %v", off, err)
		}
		b, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			t.Fatalf("ReadAll(%d): %v", off, rerr)
		}
		return b
	}

	// 首轮：4 个窗口（全部回源，因为窗口缓存未就绪；触发下载）。
	for off := int64(0); off < 1<<20; off += 256 << 10 {
		if got := readWindow(off, 256<<10); !bytes.Equal(got, content[off:off+256<<10]) {
			t.Fatalf("首轮窗口 %d 内容错误", off)
		}
	}
	firstRound := inner.reads.Load()
	if firstRound == 0 {
		t.Fatal("首轮应回源（窗口未就绪）")
	}

	// 等窗口缓存落盘完成（4MiB 内容 = 1 块；块内 4 个 256KiB blocklet ⇒ 1 个 4MiB 窗口）。
	waitWindows(t, func() bool { return countCacheFiles(stageDir) > 0 }, "窗口缓存落盘")

	// 二轮：同一批窗口应完全命中本地（零新增远端 Range）。
	for off := int64(0); off < 1<<20; off += 256 << 10 {
		if got := readWindow(off, 256<<10); !bytes.Equal(got, content[off:off+256<<10]) {
			t.Fatalf("二轮窗口 %d 内容错误", off)
		}
	}
	if inner.reads.Load() != firstRound {
		t.Fatalf("二轮应零远端：Range 次数 %d → %d", firstRound, inner.reads.Load())
	}
}

// countCacheFiles 统计缓存目录内的最终产物（排除 .tmp）。
func countCacheFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tmp") {
			n++
		}
	}
	return n
}

// TestStageDL_StoresCiphertextNotPlaintext：窗口缓存内容是**密文**（盘上不落明文）。
func TestStageDL_StoresCiphertextNotPlaintext(t *testing.T) {
	t.Parallel()
	fs, _, content := newCacheEnv(t, -1)
	stageDir := t.TempDir()
	fs.stageDL = newStageDL(stageDir, 64<<20, fs)

	rc, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, 4096)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	if _, rerr := io.ReadAll(rc); rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	_ = rc.Close()

	waitWindows(t, func() bool { return countCacheFiles(stageDir) > 0 }, "窗口缓存落盘")
	entries, _ := os.ReadDir(stageDir)
	plainSample := content[:4096]
	for _, ent := range entries {
		if strings.HasSuffix(ent.Name(), ".tmp") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(stageDir, ent.Name()))
		if rerr != nil {
			t.Fatalf("读缓存文件: %v", rerr)
		}
		if bytes.Contains(raw, plainSample) {
			t.Fatalf("缓存文件 %s 含明文样本 ⇒ 明文落盘违反安全纪律", ent.Name())
		}
	}
}

// TestStageDL_TamperedFallsBackToRemote：窗口缓存被篡改 ⇒ 解密失败 → 回源（fail-closed）。
func TestStageDL_TamperedFallsBackToRemote(t *testing.T) {
	t.Parallel()
	fs, _, content := newCacheEnv(t, -1)
	stageDir := t.TempDir()
	fs.stageDL = newStageDL(stageDir, 64<<20, fs)

	readWindow := func(off, size int64) []byte {
		t.Helper()
		rc, err := fs.OpenRangeRead(context.Background(), "big.bin", off, size)
		if err != nil {
			t.Fatalf("OpenRangeRead(%d): %v", off, err)
		}
		b, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			t.Fatalf("ReadAll(%d): %v", off, rerr)
		}
		return b
	}

	if got := readWindow(0, 4096); !bytes.Equal(got, content[:4096]) {
		t.Fatal("首读内容错误")
	}
	waitWindows(t, func() bool { return countCacheFiles(stageDir) > 0 }, "窗口缓存落盘")

	entries, _ := os.ReadDir(stageDir)
	var target string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tmp") {
			target = filepath.Join(stageDir, e.Name())
			break
		}
	}
	if target == "" {
		t.Fatal("缓存目录应有文件")
	}
	raw, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatalf("读缓存: %v", rerr)
	}
	raw[len(raw)/2] ^= 0xFF // 改中间一个字节
	if werr := os.WriteFile(target, raw, 0o600); werr != nil {
		t.Fatalf("写回篡改内容: %v", werr)
	}

	got := readWindow(0, 4096)
	if !bytes.Equal(got, content[:4096]) {
		t.Fatal("篡改后回源内容错误（应 fail-closed 回源得正确明文）")
	}
}

// TestStageDL_DisabledZeroRegression：未装配 stageDL ⇒ 读路径零回归。
func TestStageDL_DisabledZeroRegression(t *testing.T) {
	t.Parallel()
	fs, inner, content := newCacheEnv(t, -1)
	if fs.stageDL != nil {
		t.Fatal("未配置 SpanCacheDir 时 stageDL 应为 nil")
	}
	before := inner.reads.Load()
	rc, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, 256<<10)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if !bytes.Equal(got, content[:256<<10]) {
		t.Fatal("内容错误")
	}
	if inner.reads.Load() == before {
		t.Fatal("未装配缓存时应走既有远端路径（零回归）")
	}
}

// _ 编译期断言：shardseal/syncpkg 在本测试中被间接使用（保持 import 稳定）。
var (
	_ = shardseal.SaltLen
	_ syncpkg.FS
)

// TestStageDL_WindowCacheHitIsFinalPlaintext 锁定「窗口缓存命中」的返回契约：
// 命中时拿到的是**已解密且已裁剪到 [offset,end) 的明文**，调用方不得再当密文跨度二次解密。
//
// 为什么需要它（2026-11-26 实测）：`segmentSpanFetch` 命中窗口缓存时返回
// `(明文, spanOff=0, ok=true)`，而调用方无条件走 `decryptSegmentWindows(span, spanOff, …)` ⇒
// 把 4 KiB 明文当密文跨度、按 blocklet 的 EncOffset(≈214) 越界的密文长度(262172) 校验 ⇒
// 「blocklet 段 0 越出合并区间」。触发条件是「第一次读时后台窗口下载已完成」（竞态），
// 故表现为偶发 flake（整包并发跑必现、单跑常常不现）。
func TestStageDL_WindowCacheHitIsFinalPlaintext(t *testing.T) {
	t.Parallel()
	fs, _, content := newCacheEnv(t, -1) // 关闭内存块缓存，只走窗口缓存
	stageDir := t.TempDir()
	fs.stageDL = newStageDL(stageDir, 64<<20, fs)

	// 第一轮：触发后台窗口下载并等它落盘。
	rc, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, 4096)
	if err != nil {
		t.Fatalf("OpenRangeRead #1: %v", err)
	}
	if _, rerr := io.ReadAll(rc); rerr != nil {
		t.Fatalf("ReadAll #1: %v", rerr)
	}
	_ = rc.Close()
	// 等到**窗口就绪**（windowCacheRead 真的命中），而不是只等「有缓存文件」——前者才是
	// 触发缓存命中分支的充要条件（后者可以早于就绪标记）。用轮询把竞态变成确定性前置。
	waitWindows(t, func() bool {
		fs.mu.RLock()
		e := fs.index["big.bin"]
		fs.mu.RUnlock()
		if e == nil || e.meta == nil {
			return false
		}
		key, salt, kerr := fs.fileLevelKey(e)
		if kerr != nil {
			return false
		}
		o := chunkRangeOpts{e: e, rel: "big.bin", ci: e.meta.Chunks[0], keyBytes: key, salt: salt}
		_, ok := fs.windowCacheRead(o, 0, 4096)
		return ok
	}, "窗口缓存就绪")

	// 第二轮：此时窗口已就绪 ⇒ 走缓存命中路径，必须返回正确明文且不报错。
	rc2, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, 4096)
	if err != nil {
		t.Fatalf("OpenRangeRead #2: %v", err)
	}
	defer rc2.Close()
	got, rerr := io.ReadAll(rc2)
	if rerr != nil {
		t.Fatalf("窗口缓存命中后读应成功（不得把明文当密文跨度二次解密）: %v", rerr)
	}
	if !bytes.Equal(got, content[:4096]) {
		t.Fatalf("缓存命中返回内容不符：len=%d want=%d", len(got), 4096)
	}
}
