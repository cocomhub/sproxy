// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// stage_downloader_test.go —— 整块 staging 下载器的正确性锁定（2026-10-23）。
//
// 背景（真机实测）：外部卷读侧逐 4MiB 窗 Range GET ≈ 0.55MB/s，而底层 OpenRead 整块
// （baidupcs 库 Downloader：Range 并行 + 断点）≈ 2.13MB/s。staging 下载器把整块 blob
// 一次下到本地后，该块所有窗口读都命中磁盘。
//
// 断言（各用例）：
//   - 整块 staging 命中 ⇒ 远端 OpenRead 只发生**一次**（后续窗口零远端）；
//   - staging 内容 = 底层原始密文（**不落明文**：盘上字节 != 明文）；
//   - 解密失败（staging 被篡改）⇒ 回源（fail-closed 不静默返回错内容）；
//   - 未装配（SpanCacheDir 空）⇒ 零回归（不产生 staging 文件）。

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
)

// TestStageDownloader_BackgroundPrefetchThenAllLocal：后台整块预取语义（2026-10-23 优化）。
// 首轮窗口读**不等待**（全部回源 Range）并触发一次后台整块下载；等 staging 就绪后，
// 第二轮所有窗口读**零远端**（命中本地文件段读）。
func TestStageDownloader_BackgroundPrefetchThenAllLocal(t *testing.T) {
	t.Parallel()
	fs, inner, content := newCacheEnv(t, -1) // 关闭内存块缓存，只测 staging
	stageDir := t.TempDir()
	fs.stageDL = newStageDownloader(stageDir, 64<<20, fs)
	if fs.stageDL == nil {
		t.Fatal("stageDownloader 装配失败")
	}

	readWindow := func(off int64) []byte {
		t.Helper()
		rc, err := fs.OpenRangeRead(context.Background(), "big.bin", off, 256<<10)
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

	// 首轮：4 个窗口（全部走远端 Range，因为 staging 未就绪；后台触发一次整块下载）。
	for off := int64(0); off < 1<<20; off += 256 << 10 {
		if got := readWindow(off); !bytes.Equal(got, content[off:off+256<<10]) {
			t.Fatalf("首轮窗口 %d 内容错误", off)
		}
	}
	firstRound := inner.reads.Load()
	if firstRound == 0 {
		t.Fatal("首轮应回源（staging 未就绪）")
	}

	// 等后台整块下载完成（整块 1 次 OpenRead）。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && inner.wholeReads.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	// 整文件预取：4MiB 内容 / 2MiB 块 = 2 块 ⇒ 应恰好 2 次整块下载（每块一次）。
	wantWhole := int64(2)
	if inner.wholeReads.Load() != wantWhole {
		t.Fatalf("后台整块下载次数=%d，应为 %d（整文件预取，每块一次）", inner.wholeReads.Load(), wantWhole)
	}
	time.Sleep(100 * time.Millisecond) // 等待 rename 完成

	// 二轮：同一批窗口应完全命中本地（零新增远端 Range）。
	for off := int64(0); off < 1<<20; off += 256 << 10 {
		if got := readWindow(off); !bytes.Equal(got, content[off:off+256<<10]) {
			t.Fatalf("二轮窗口 %d 内容错误", off)
		}
	}
	if inner.reads.Load() != firstRound {
		t.Fatalf("二轮应零远端：Range 次数 %d → %d", firstRound, inner.reads.Load())
	}
	if inner.wholeReads.Load() != wantWhole {
		t.Fatalf("整块下载不应重复：%d 次（应 %d）", inner.wholeReads.Load(), wantWhole)
	}
}

// TestStageDownloader_StoresCiphertextNotPlaintext：staging 文件内容是**密文**
// （盘上不落明文——安全纪律）。
func TestStageDownloader_StoresCiphertextNotPlaintext(t *testing.T) {
	t.Parallel()
	fs, _, content := newCacheEnv(t, -1)
	stageDir := t.TempDir()
	fs.stageDL = newStageDownloader(stageDir, 64<<20, fs)

	rc, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, 4096)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	if _, rerr := io.ReadAll(rc); rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	_ = rc.Close()

	// 整文件预取是**后台**下载：轮询等待 staging 落盘完成（跳过 .tmp 中间态）。
	var entries []os.DirEntry
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ = os.ReadDir(stageDir)
		ready := 0
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".tmp") {
				ready++
			}
		}
		if ready > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(entries) == 0 {
		t.Fatal("staging 目录应有块密文文件")
	}
	plainSample := content[:4096]
	for _, ent := range entries {
		if strings.HasSuffix(ent.Name(), ".tmp") {
			continue // 下载中间态
		}
		raw, rerr := os.ReadFile(filepath.Join(stageDir, ent.Name()))
		if rerr != nil {
			t.Fatalf("读 staging 文件: %v", rerr)
		}
		if bytes.Contains(raw, plainSample) {
			t.Fatalf("staging 文件 %s 含明文样本 ⇒ 明文落盘违反安全纪律", ent.Name())
		}
	}
}

// TestStageDownloader_TamperedFallsBackToRemote：staging 就绪后被篡改 ⇒ 解密失败 →
// 回源段读（fail-closed：不返回错误内容，也不因缓存掩盖损坏）。
func TestStageDownloader_TamperedFallsBackToRemote(t *testing.T) {
	t.Parallel()
	fs, inner, content := newCacheEnv(t, -1)
	stageDir := t.TempDir()
	fs.stageDL = newStageDownloader(stageDir, 64<<20, fs)

	readWindow := func(off int64, size int64) []byte {
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

	// 首次读触发后台整块下载。
	if got := readWindow(0, 4096); !bytes.Equal(got, content[:4096]) {
		t.Fatal("首读内容错误")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && inner.wholeReads.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // rename 完成

	// 篡改 staging 文件（改中间一个字节）。
	entries, _ := os.ReadDir(stageDir)
	if len(entries) == 0 {
		t.Fatal("staging 应有文件")
	}
	p := filepath.Join(stageDir, entries[0].Name())
	raw, rerr := os.ReadFile(p)
	if rerr != nil {
		t.Fatalf("读 staging: %v", rerr)
	}
	raw[len(raw)/2] ^= 0xFF
	if werr := os.WriteFile(p, raw, 0o600); werr != nil {
		t.Fatalf("写回篡改内容: %v", werr)
	}

	// 再读：staging 解密必须失败 → 回源得正确明文（不返回损坏内容）。
	got := readWindow(0, 4096)
	if !bytes.Equal(got, content[:4096]) {
		t.Fatal("篡改后回源内容错误（应 fail-closed 回源得正确明文）")
	}
}

// TestStageDownloader_DisabledZeroRegression：未装配 stageDL ⇒ 读路径零回归
// （无 staging 文件、行为与装配前一致）。
func TestStageDownloader_DisabledZeroRegression(t *testing.T) {
	t.Parallel()
	fs, inner, content := newCacheEnv(t, -1)
	if fs.stageDL != nil {
		t.Fatal("未配置 Sp anCacheDir 时 stageDL 应为 nil")
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
		t.Fatal("未装配 staging 时应走既有远端路径（零回归）")
	}
}

// _ 编译期断言：shardseal/syncpkg 在本测试中被间接使用（保持 import 稳定）。
var (
	_ = shardseal.SaltLen
	_ syncpkg.FS
)
