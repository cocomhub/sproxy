// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// range_seek_window_test.go —— RangeSeeker 的**窗口按底层本地性自适应**（2026-10-21 缺陷修复）。
//
// 背景：固定 4MiB 窗口对**网盘/外部卷**过大——`OpenRangeRead` 对窗口是「惰性流式」的
// （2026-10-21 起），但窗口仍是**预取上限**：服务第一个字节只需第一块，可一旦客户端顺序
// 播放，窗口内每一块都要被拉到。窗口越大 ⇒ seek 后重开窗口的读放大越大（拖进度条时多读
// 无用数据）；窗口越小 ⇒ 顺序播放的重开次数越多（每次一次底层 RangeRead）。
//
// 判据：本地卷（`LocalVolume.IsLocalVolume()==true`，page cache + 零网络开销）用大窗口
// （4MiB，顺序读优）；外部/网盘卷用小窗口（256KiB，seek/首帧优、读放大低）。

import (
	"bytes"
	"context"
	"io"
	"testing"
)

// rangeSpan 记录一次 OpenRangeRead 的区间（本文件私有，避免与其它测试文件的类型重名）。
type rangeSpan struct{ offset, size int64 }

// localWindowFS 报告自身为**本地卷**（`IsLocalVolume()==true`），底层用 LocalFS 提供数据。
type localWindowFS struct {
	*LocalFS
	spans []rangeSpan
}

func (f *localWindowFS) IsLocalVolume() bool { return true }

func (f *localWindowFS) OpenRangeRead(ctx context.Context, relPath string, offset, size int64) (io.ReadCloser, error) {
	f.spans = append(f.spans, rangeSpan{offset, size})
	return f.LocalFS.OpenRangeRead(ctx, relPath, offset, size)
}

// spanProbeFS 记录区间并**谎报外部卷**（IsLocalVolume()==false）——构造时传入内层 LocalFS
// 供真实数据；对外语义 = 外部/网盘卷（窗口应取小值）。
type spanProbeFS struct {
	*LocalFS
	spans []rangeSpan
}

func (f *spanProbeFS) IsLocalVolume() bool { return false }

func (f *spanProbeFS) OpenRangeRead(ctx context.Context, relPath string, offset, size int64) (io.ReadCloser, error) {
	f.spans = append(f.spans, rangeSpan{offset, size})
	return f.LocalFS.OpenRangeRead(ctx, relPath, offset, size)
}

// TestRangeSeeker_WindowAdaptsToLocality：本地卷窗口 > 外部卷窗口，且两者都完整读回。
// 断言的是「窗口选取」这一决策（用单次 OpenRangeRead 的 size 观测），而非硬编码数值。
func TestRangeSeeker_WindowAdaptsToLocality(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("abcdefghij"), 2_000_000) // 20MB（> 任一窗口，使窗口有区分）
	size := int64(len(content))

	// 外部卷（IsLocalVolume()==false 的包装；底层 LocalFS 供数据）——用小窗口。
	extRoot := t.TempDir()
	extInner := NewLocalFS(extRoot, nil)
	if werr := extInner.WriteFile(context.Background(), LogicalKey{}, "f.bin", bytes.NewReader(content), size, 0); werr != nil {
		t.Fatalf("WriteFile ext: %v", werr)
	}
	extProbe := &spanProbeFS{LocalFS: extInner}
	extRS, err := NewRangeSeeker(context.Background(), extProbe, "f.bin", size)
	if err != nil {
		t.Fatalf("NewRangeSeeker(ext): %v", err)
	}
	defer extRS.Close()
	if _, serr := extRS.Seek(0, io.SeekStart); serr != nil {
		t.Fatalf("Seek: %v", serr)
	}
	buf := make([]byte, 1)
	if _, rerr := extRS.Read(buf); rerr != nil {
		t.Fatalf("Read(ext): %v", rerr)
	}
	if len(extProbe.spans) == 0 {
		t.Fatal("外部卷应发生 OpenRangeRead")
	}
	extWindow := extProbe.spans[0].size

	// 本地卷（IsLocalVolume()==true）——用大窗口。
	loc := &localWindowFS{LocalFS: NewLocalFS(t.TempDir(), nil)}
	if werr := loc.WriteFile(context.Background(), LogicalKey{}, "f.bin", bytes.NewReader(content), size, 0); werr != nil {
		t.Fatalf("WriteFile local: %v", werr)
	}
	locRS, err := NewRangeSeeker(context.Background(), loc, "f.bin", size)
	if err != nil {
		t.Fatalf("NewRangeSeeker(local): %v", err)
	}
	defer locRS.Close()
	if _, serr := locRS.Seek(0, io.SeekStart); serr != nil {
		t.Fatalf("Seek: %v", serr)
	}
	if _, rerr := locRS.Read(buf); rerr != nil {
		t.Fatalf("Read(local): %v", rerr)
	}
	if len(loc.spans) == 0 {
		t.Fatal("本地卷应发生 OpenRangeRead")
	}
	locWindow := loc.spans[0].size

	if locWindow <= extWindow {
		t.Fatalf("本地卷窗口(%d) 应大于外部卷窗口(%d)——本地零网络开销宜大读，外部宜小读",
			locWindow, extWindow)
	}
}
