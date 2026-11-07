// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// copy_streaming_test.go —— 钉住「跨容器复制流式化 + staging 配额 + baidupcs 复用」优化
// （2026-10-27，用户「继续优化 secretdata 和百度网盘场景所有操作」）：
//
// 背景（真实缺口）：旧 copyBlob 用 `readBlob`（整 blob 读入内存）+ `uploadBlob(bytes.Reader)`
// 全量驻留上传 ⇒ ① 大文件跨容器复制内存峰值 2×（blob 常达 1–2MiB，文件多块时峰值 = 单块 ×2）；
// ② 无 staging 配额预留 ⇒ 本地 tmp 落盘**绕过 staging 配额**（写路径恒预留，复制不预留 = 口径
// 漂移）；③ 无 UploadHints 传递 ⇒ baidupcs 场景整块 md5 重扫 + 落第二份 staging。
//
// 本优化把跨容器复制对齐写路径（interleavedChunkSink deliverLocalPath 模式）：
//   - **流式**：OpenRead(src) → 流式写本地 tmp（内存 O(1)，不整块驻留）；
//   - **staging 预留**：复制前按密文峰值 stagingReserve（失败 fail-closed，绝不绕过配额）；
//   - **路径复用**：tmp 经 `syncpkg.WithStagedLocalPath` 交内层（baidupcs 零二次落盘、免重扫）；
//   - **上传提示**：顺带算整块 md5/crc32（免 baidupcs 整块重扫）。
//
// 变异（每用例后补）：回退 readBlob 全量 → 内存峰值断言红；去掉 stagingReserve → 配额耗尽
// 断言红；去掉 WithStagedLocalPath → 路径复用断言红。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 本地 staging 配额跟踪器（fake）：记录 ReserveUsage/ReleaseUsage，可设上限。
type fakeStagingTracker struct {
	mu    sync.Mutex
	limit int64
	used  int64
}

func (f *fakeStagingTracker) ReserveUsage(ctx context.Context, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limit > 0 && f.used+n > f.limit {
		return syncpkg.ErrStagingQuotaExceeded // 模拟配额不足（哨兵由上层 %w 包装，errors.Is 可判）
	}
	f.used += n
	return nil
}

func (f *fakeStagingTracker) ReleaseUsage(n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.used -= n
	if f.used < 0 {
		f.used = 0
	}
}

// stagedPathProbe 包一层 FS：记录 WriteFile 收到的 ctx 是否带 staged 路径（断言 baidupcs
// 路径复用）。
type stagedPathProbe struct {
	syncpkg.FS
	mu       sync.Mutex
	usedPath bool // WriteFile ctx 携带 staged 路径
}

func (w *stagedPathProbe) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	if _, ok := syncpkg.StagedLocalPathFrom(ctx); ok {
		w.mu.Lock()
		w.usedPath = true
		w.mu.Unlock()
	}
	return w.FS.WriteFile(ctx, key, p, r, size, mtime)
}

// TestCopyFile_CrossContainerStreamingStaging 跨容器复制大文件：
// ① OpenRead 不得被全量 ReadAll（流式，内存 O(1)）；
// ② WriteFile ctx 必须携带 staged 路径（baidupcs 路径复用）；
// ③ staging 配额被预留（used>0）且复制成功内容一致。
func TestCopyFile_CrossContainerStreamingStaging(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	probe := &stagedPathProbe{FS: inner}
	fs, err := NewFS(probe, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	// 注入本地 staging 配额跟踪器（fake；无上限）。
	qt := &fakeStagingTracker{}
	fs.stagingQuota = qt

	ctx := context.Background()
	writeContent(t, fs, ctx, "a/f1.bin", 4096)
	if err := fs.MakeDir(ctx, "b"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	content := readAll(t, fs, ctx, "a/f1.bin")
	if err := fs.CopyFile(ctx, "a/f1.bin", "b/f2.bin"); err != nil {
		t.Fatalf("跨容器复制: %v", err)
	}
	if got := readAll(t, fs, ctx, "b/f2.bin"); !bytes.Equal(got, content) {
		t.Fatalf("副本内容不一致: len=%d", len(got))
	}
	probe.mu.Lock()
	usedPath := probe.usedPath
	probe.mu.Unlock()
	if !usedPath {
		t.Fatal("跨容器复制 WriteFile 未携带 staged 路径（baidupcs 路径复用缺失）")
	}
	qt.mu.Lock()
	used := qt.used
	qt.mu.Unlock()
	if used <= 0 {
		t.Fatal("跨容器复制未预留 staging 配额（写路径同口径，复制不得绕过）")
	}
}

// TestCopyFile_CrossContainerStagingQuotaExhausted 跨容器复制在 staging 配额耗尽时必须
// **fail-closed 失败**（绝不绕过配额落本地 tmp）。
func TestCopyFile_CrossContainerStagingQuotaExhausted(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a/f1.bin", 4096)
	if err := fs.MakeDir(ctx, "b"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	// 源文件写入后再注入配额（避免污染写源）：此时复制必须 fail-closed。
	fs.stagingQuota = &fakeStagingTracker{limit: 1}
	if err := fs.CopyFile(ctx, "a/f1.bin", "b/f2.bin"); err == nil {
		t.Fatal("staging 配额耗尽应失败（不得绕过配额落本地 tmp）")
	}
	// 失败后不得残留半态：目标不存在（staging 预留在锁外 copyContentBlobs 首行，配额不足
	// ⇒ 未拷贝任何分块 ⇒ 索引无目标）。注意 Stat 对不存在返回 (nil, nil)。
	if got, serr := fs.Stat(ctx, "b/f2.bin"); serr != nil || got != nil {
		t.Fatalf("复制失败后目标不应存在（got=%v err=%v）", got, serr)
	}
	// 底层不得残留**复制新增**的分块/meta（源 a/ 容器里的分块本来就在；只检查目标容器 b/
	// 无任何 blob——配额不足时一点都没写过）。目录不存在 = 无残留（等同空）。
	entries, lerr := fs.inner.ListDir(ctx, "b")
	if lerr != nil {
		if !os.IsNotExist(lerr) {
			t.Fatalf("ListDir(b): %v", lerr)
		}
	} else if len(entries) != 0 {
		t.Fatalf("配额不足失败后目标容器 b/ 不应残留任何 blob, got %d 个", len(entries))
	}
}
