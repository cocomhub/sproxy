// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// lazy_range_test.go —— OpenRangeRead 必须**惰性流式**（首个 Read 前只发生「目标段」的
// 底层读取），而非急切物化整个 [offset, offset+size) 窗口（真实用户实测缺陷 2026-10-21）。
//
// 背景：Web UI / VLC 播放加密卷视频（secretdata over 网盘），`RangeSeeker` 一次开窗 4MiB
// ⇒ 旧实现 `rangeReadBytes` 把窗口内**全部**分块先下载+解密成 `[]byte` 再包成流 ⇒ 首帧
// 要等整个 4MiB 窗口（网盘场景数秒），拖进度条也要重下整个新窗口。crypto 层早已支持段级
// 解密（`DecryptBlockletSegmentStandalone` / `decryptBlockletAt`），缺的只是 FS 适配层的
// 流式接线。
//
// 本文件锁定的语义：
//  1. `OpenRangeRead` 返回的流**惰性**——首次 `Read` 才触发底层读取，且只读「含目标 offset
//     的第一段」（首字节延迟 = 1 段而非整个窗口）；
//  2. 流的 `Read` 逐段推进：读一段 → 解一段 → 交一段（段间无依赖，GCM 独立）；
//  3. 恰好 `size` 字节（提前 EOF 即错误，契约与 eager 版一致）；
//  4. 中途段读/解密失败 → 流返回错误（fail-closed，不静默截断）。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// lazyProbeFS 包装 LocalFS：统计 OpenRangeRead（底层段读）与 OpenRead（整块）调用，
// 并记录每次段读的区间 —— 用于断言「首个 Read 前只发生 1 次段读，且是含目标 offset 的段」。
type lazyProbeFS struct {
	*syncpkg.LocalFS
	rangeReads int
	firstRange struct{ offset, size int64 }
	wholeReads int
}

func (p *lazyProbeFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	if p.rangeReads == 0 {
		p.firstRange.offset, p.firstRange.size = offset, size
	}
	p.rangeReads++
	return p.LocalFS.OpenRangeRead(ctx, path, offset, size)
}

func (p *lazyProbeFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	p.wholeReads++
	return p.LocalFS.OpenRead(ctx, path)
}

// newLazyProbeEnv 建带计数底层 FS 的 secretdata FS + 测试文件（多块 + 小 blocklet）。
func newLazyProbeEnv(t *testing.T) (*SecretdataFS, *lazyProbeFS, []byte) {
	t.Helper()
	probe := &lazyProbeFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	fs, err := NewFS(probe, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{
			Mode: "random", Min: 64, Max: 128,
			BlockletMode: "fixed", BlockletMin: 32, BlockletMax: 64,
		},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	content := data(512)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "movie.mp4",
		bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	return fs, probe, content
}

// TestOpenRangeRead_LazyFirstReadOnlyFetchesTargetSegment：**惰性**语义——`OpenRangeRead`
// 返回后**不**触发任何底层读取；首次 `Read` 才读，且只读「含目标 offset 的第一段」
// （不是整个 4MiB 窗口）。这是「首帧快」的接线判据。
func TestOpenRangeRead_LazyFirstReadOnlyFetchesTargetSegment(t *testing.T) {
	t.Parallel()
	fs, probe, content := newLazyProbeEnv(t)

	rc, err := fs.OpenRangeRead(context.Background(), "movie.mp4", 200, 100)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	// 1) 惰性：打开后、首次 Read 前，不得有任何底层读取。
	if probe.rangeReads != 0 || probe.wholeReads != 0 {
		t.Fatalf("OpenRangeRead 后不应触发底层读取（惰性），got range=%d whole=%d", probe.rangeReads, probe.wholeReads)
	}
	// 2) 首次 Read 只拉含 offset=200 的段（首段区间应覆盖 200 且长度 ≤ 单段上限）。
	buf := make([]byte, 100)
	if _, rerr := io.ReadFull(rc, buf); rerr != nil {
		t.Fatalf("ReadFull: %v", rerr)
	}
	if !bytes.Equal(buf, content[200:300]) {
		t.Fatalf("首段内容错误：got %q", buf)
	}
	if probe.firstRange.size > 128 {
		t.Fatalf("首次段读区间过大（size=%d，期望 ≤ 单块/段上限）——急切物化了整个窗口", probe.firstRange.size)
	}
	// 3) 整块回落未发生（段读路径正常）。
	if probe.wholeReads != 0 {
		t.Fatalf("惰性段读路径不应走整块回落，whole=%d", probe.wholeReads)
	}
	_ = rc.Close()
}

// TestOpenRangeRead_StreamingBySegment：流逐段推进——每次 Read 触发**新的段级**底层读取
// （含目标区间跨多段时，rangeReads 随 Read 递增，而非一次性物化）。整读内容与 eager 一致。
func TestOpenRangeRead_StreamingBySegment(t *testing.T) {
	t.Parallel()
	fs, probe, content := newLazyProbeEnv(t)

	rc, err := fs.OpenRangeRead(context.Background(), "movie.mp4", 0, 300)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	got := make([]byte, 0, 300)
	chunk := make([]byte, 37) // 非段边界的小步长，强制跨多段推进
	for {
		n, rerr := rc.Read(chunk)
		got = append(got, chunk[:n]...)
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			t.Fatalf("流式 Read: %v", rerr)
		}
	}
	_ = rc.Close()
	if !bytes.Equal(got, content[:300]) {
		t.Fatalf("流式整读内容错误：len(got)=%d", len(got))
	}
	if probe.rangeReads < 2 {
		t.Fatalf("跨多段流式读应触发 ≥2 次段级读取（got %d）——确认按段推进而非一次物化", probe.rangeReads)
	}
}

// TestOpenRangeRead_StreamingFailureVisible：流中途段读失败 → 流返回错误（fail-closed），
// 不静默截断成「短流 + 干净 EOF」。
func TestOpenRangeRead_StreamingFailureVisible(t *testing.T) {
	t.Parallel()
	probe := &lazyProbeFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	fs, err := NewFS(probe, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{
			Mode: "random", Min: 64, Max: 128,
			BlockletMode: "fixed", BlockletMin: 32, BlockletMax: 64,
		},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	content := data(256)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "f.bin",
		bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	// 删掉第一个分块 blob ⇒ 流式读会在推进到该块时报错（而非静默短流）。
	e := fs.index["f.bin"]
	ci := e.meta.Chunks[0]
	if derr := fs.inner.Delete(context.Background(), fs.dataSeg(e)+"/"+ci.FileName); derr != nil {
		t.Fatalf("delete chunk: %v", derr)
	}
	rc, oerr := fs.OpenRangeRead(context.Background(), "f.bin", 0, 256)
	if oerr != nil {
		return // open 即失败同样满足 fail-closed
	}
	var err2 error
	buf := make([]byte, 256)
	if _, rerr := io.ReadFull(rc, buf); rerr != nil {
		err2 = rerr
	}
	_ = rc.Close()
	if err2 == nil {
		t.Fatal("底层分块被删后流式读应报错（fail-closed），却完整成功（静默回落/截断）")
	}
}
