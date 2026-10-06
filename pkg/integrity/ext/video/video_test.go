// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package video

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/integrity"
	"github.com/cocomhub/sproxy/pkg/media/ffprobe"
)

// writeBytes 写原始字节到临时文件（扩展名由参数指定，供 Matches 判定）。
func writeBytes(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestVideoChecker_Matches：扩展名族匹配（对齐 pkg/integrity §77 规则）。
func TestVideoChecker_Matches(t *testing.T) {
	t.Parallel()
	c := VideoChecker{}
	for _, name := range []string{"a.mp4", "b.mkv", "c.webm", "d.mov", "e.ts", "f.avi", "g.flv", "h.wmv", "u.MP4"} {
		if !c.Matches(name) {
			t.Errorf("Matches(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"x.txt", "y.tar", "z.png"} {
		if c.Matches(name) {
			t.Errorf("Matches(%q) = true, want false", name)
		}
	}
}

// TestVideoChecker_Kind：类型标识为注册键。
func TestVideoChecker_Kind(t *testing.T) {
	t.Parallel()
	if (VideoChecker{}).Kind() != "video/*" {
		t.Fatal("Kind 应为 video/*")
	}
}

// TestVideoChecker_Corrupt：非视频字节（即使 .mp4 扩展名）→ OK=false（不回 error）。
func TestVideoChecker_Corrupt(t *testing.T) {
	t.Parallel()
	path := writeBytes(t, "corrupt.mp4", []byte("not-a-video"))
	rep, err := VideoChecker{}.Check(context.Background(), path, int64(len("not-a-video")))
	if err != nil {
		t.Fatalf("损坏视频应返回 OK=false 语义而非 error，got %v", err)
	}
	if rep.OK {
		t.Fatal("损坏视频应失败")
	}
}

// TestVideoChecker_Nonexistent：路径不存在 → error（校验执行错误，非语义判定）。
func TestVideoChecker_Nonexistent(t *testing.T) {
	t.Parallel()
	_, err := VideoChecker{}.Check(context.Background(), filepath.Join(t.TempDir(), "missing.mp4"), 0)
	if err == nil {
		t.Fatal("不存在的路径应返回 error")
	}
}

// TestVideoChecker_ZeroSize：空文件 → ffprobe 解析失败 → OK=false。
func TestVideoChecker_ZeroSize(t *testing.T) {
	t.Parallel()
	path := writeBytes(t, "empty.mp4", nil)
	rep, err := VideoChecker{}.Check(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("空文件应返回 OK=false 语义而非 error，got %v", err)
	}
	if rep.OK {
		t.Fatal("空文件应失败")
	}
}

// TestVideoChecker_RegistryLookup：空白导入注册后，Lookup 按扩展名分发命中 video/*。
func TestVideoChecker_RegistryLookup(t *testing.T) {
	t.Parallel()
	c := integrity.Lookup("movie.mp4")
	if c == nil {
		t.Fatal("Lookup(movie.mp4) 应命中已注册校验器")
	}
	if got := c.Kind(); got != "video/*" {
		t.Fatalf("Lookup Kind = %q, want video/*", got)
	}
}

// fakeIndexer 是 KeyframeOffsets 的测试实现：err 固定返回（模拟缺 ffprobe/解析失败）。
type fakeIndexer struct{ err error }

func (f fakeIndexer) KeyframeOffsets(req shardseal.KeyframeRequest) ([]int64, error) {
	return nil, f.err
}

// TestVideoChecker_MissingFFprobe R5-I1：缺 ffprobe（ErrFFprobeMissing）→ OK=true 放行
// （无校验器 ≠ 损坏，无 ffmpeg 部署不误判 damaged）。
func TestVideoChecker_MissingFFprobe(t *testing.T) {
	// 共享 testSeamIndexer 包级注入缝——不可并行（其他测试并发写）
	prev := testSeamIndexer
	testSeamIndexer = fakeIndexer{err: ffprobe.ErrFFprobeMissing}
	t.Cleanup(func() { testSeamIndexer = prev })

	path := writeBytes(t, "m.mp4", []byte("whatever"))
	rep, err := VideoChecker{}.Check(context.Background(), path, int64(len("whatever")))
	if err != nil {
		t.Fatalf("缺 ffprobe 应返回 OK=true（放行），而非 error，got %v", err)
	}
	if !rep.OK {
		t.Fatalf("缺 ffprobe 应 OK=true 放行，got Reason=%q", rep.Reason)
	}
}

// TestVideoChecker_CtxCancel 取消路径：ctx 取消 → 返回 error（非 OK:false——调用方
// 按「校验执行出错→放行」处理，不当语义异常累计）。
func TestVideoChecker_CtxCancel(t *testing.T) {
	// 共享 testSeamIndexer 包级注入缝——不可并行
	prev := testSeamIndexer
	// 阻塞索引器（不返回）——select 等 ctx.Done
	blocking := make(chan struct{})
	testSeamIndexer = blockingIndexer{ch: blocking}
	t.Cleanup(func() {
		testSeamIndexer = prev
		close(blocking)
	})

	path := writeBytes(t, "c.mp4", []byte("data"))
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消
	_, err := VideoChecker{}.Check(cancelCtx, path, 0)
	if err == nil {
		t.Fatal("ctx 取消应返回 error（中止哨兵），而非 nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消错误应包含 context.Canceled，got %v", err)
	}
}

// blockingIndexer 永不返回的索引器（测试取消路径；ch 关闭可解除）。
type blockingIndexer struct{ ch chan struct{} }

func (b blockingIndexer) KeyframeOffsets(req shardseal.KeyframeRequest) ([]int64, error) {
	<-b.ch
	return nil, nil
}
