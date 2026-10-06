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
	// 共享 testSeamIndexer 包级注入缝——经 setTestSeamIndexer 持锁替换（生产路径只读+局部回落）
	prev := testSeamIndexer
	setTestSeamIndexer(fakeIndexer{err: ffprobe.ErrFFprobeMissing})
	t.Cleanup(func() { setTestSeamIndexer(prev) })

	path := writeBytes(t, "m.mp4", []byte("whatever"))
	rep, err := VideoChecker{}.Check(context.Background(), path, int64(len("whatever")))
	if err != nil {
		t.Fatalf("缺 ffprobe 应返回 OK=true（放行），而非 error，got %v", err)
	}
	if !rep.OK {
		t.Fatalf("缺 ffprobe 应 OK=true 放行，got Reason=%q", rep.Reason)
	}
}

// TestVideoChecker_OutputLimitPasses M5-I2：ffprobe 输出超上限（ErrFFprobeOutputLimit）
// → OK=true 放行——合法视频帧数异常多属**资源限制 ≠ 文件损坏**，不误判 damaged。
func TestVideoChecker_OutputLimitPasses(t *testing.T) {
	// 共享 testSeamIndexer 包级注入缝（输出超限哨兵）——须串行（同文件 serial budget）。
	prev := testSeamIndexer
	setTestSeamIndexer(fakeIndexer{err: ffprobe.ErrFFprobeOutputLimit})
	t.Cleanup(func() { setTestSeamIndexer(prev) })

	path := writeBytes(t, "big.mp4", []byte("whatever"))
	rep, err := VideoChecker{}.Check(context.Background(), path, int64(len("whatever")))
	if err != nil {
		t.Fatalf("输出超限应 OK=true（放行），而非 error，got %v", err)
	}
	if !rep.OK {
		t.Fatalf("输出超限应 OK=true（资源限制≠损坏），got Reason=%q", rep.Reason)
	}
}

// TestVideoChecker_CtxCancel 取消路径：ctx 取消 → 返回 error（非 OK:false——调用方
// 按「校验执行出错→放行」处理，不当语义异常累计）。
func TestVideoChecker_CtxCancel(t *testing.T) {
	// 共享 testSeamIndexer 包级注入缝——经 setTestSeamIndexer 持锁替换
	prev := testSeamIndexer
	// 阻塞索引器（不返回）——select 等 ctx.Done
	blocking := make(chan struct{})
	setTestSeamIndexer(blockingIndexer{ch: blocking})
	t.Cleanup(func() {
		setTestSeamIndexer(prev)
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

// emptyIndexer 返回空关键帧列表的索引器（模拟容器无视频流——仅音频轨/空流）。
type emptyIndexer struct{}

func (emptyIndexer) KeyframeOffsets(req shardseal.KeyframeRequest) ([]int64, error) {
	return nil, nil
}

// TestVideoChecker_AudioOnlyPasses M3-I1：仅音频轨容器（-select_streams v:0 输出空）→
// OK=true 放行（有内容但非视频 ≠ 损坏，不误判 damaged/重下）。
func TestVideoChecker_AudioOnlyPasses(t *testing.T) {
	// 共享 testSeamIndexer 包级注入缝（空索引器模拟无视频流）——此测试与另外两个
	// 注入缝测试共享包级 seam 变量，须串行（同文件已登记 serial budget）。
	prev := testSeamIndexer
	setTestSeamIndexer(emptyIndexer{})
	t.Cleanup(func() { setTestSeamIndexer(prev) })

	path := writeBytes(t, "audio.m4a", []byte("audio-data"))
	rep, err := VideoChecker{}.Check(context.Background(), path, int64(len("audio-data")))
	if err != nil {
		t.Fatalf("仅音频轨应 OK=true 放行（非 error），got %v", err)
	}
	if !rep.OK {
		t.Fatalf("仅音频轨应 OK=true（有内容但非视频 ≠ 损坏），got Reason=%q", rep.Reason)
	}
}

// blockingIndexer 永不返回的索引器（测试取消路径；ch 关闭可解除）。
type blockingIndexer struct{ ch chan struct{} }

func (b blockingIndexer) KeyframeOffsets(req shardseal.KeyframeRequest) ([]int64, error) {
	<-b.ch
	return nil, nil
}

// TestVideoChecker_EstimateMem 估算：小文件（<2GiB）→ 仅进程 64MiB；大文件（100GiB）
// → 64MiB + JSON 估算（0.5MB/GB×2 上浮：100GiB→50MiB），且 JSON 封顶 256MiB。
func TestVideoChecker_EstimateMem(t *testing.T) {
	// 共享 testSeamIndexer？不——EstimateMem 用 os.Stat 不碰注入缝，可并行
	t.Parallel()
	c := VideoChecker{}
	if est := c.EstimateMem("", 1024*1024); est != 64<<20 {
		t.Fatalf("小文件估算应=64MiB（进程），got %d", est)
	}
	// 100GiB = 107374182400 B → jsonEst = 107374182400/(2<<30)*(1<<20) = 50MiB
	const big = int64(100) * (1 << 30)
	est := c.EstimateMem("", big)
	wantJSON := big / (2 << 30) * (1 << 20)
	if est != 64<<20+wantJSON {
		t.Fatalf("100GiB 估算应=64MiB+50MiB，got %d", est)
	}
	// JSON 封顶：1TB 视频 → jsonEst 应为 256MiB（封顶）
	const tb = int64(1024) * (1 << 30)
	estTB := c.EstimateMem("", tb)
	if estTB != 64<<20+256<<20 {
		t.Fatalf("1TB 视频 JSON 应封顶 256MiB，got %d", estTB)
	}
}
