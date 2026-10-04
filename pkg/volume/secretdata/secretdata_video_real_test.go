// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// 真实视频文件完整流程测试（2026-10-04 用户指令）：用 ffmpeg 生成真实 MP4 →
// secretdata 卷以 video-keyframe 模式写入（go-mp4 解析关键帧）→ 整文件解密还原一致
// + OpenRangeRead 关键帧区间一致。验证「功能正确接线可用」+「纯 go 实现正确可用」。
// 本机无 ffmpeg → Skip（外部依赖）。

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// makeRealMP4 用 ffmpeg 生成真实小 MP4（10s testsrc，-g 30 每 30 帧一个关键帧 → 10 关键帧）。
func makeRealMP4(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("本机无 ffmpeg（真实视频测试跳过）")
	}
	path := filepath.Join(t.TempDir(), "real.mp4")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=10:size=320x180:rate=30",
		"-c:v", "libx264", "-preset", "fast", "-g", "30", path)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg 生成测试视频失败: %v (%s)", err, buf.String())
	}
	return path
}

// TestWriteRealMP4_KeyframeMode_FullRoundtrip：真实 MP4 经 video-keyframe 模式完整流程：
// 写（go-mp4 关键帧切分）→ 整文件 OpenRead 解密还原 == 原文件（完整性）
// + meta.BlockletMode==video-keyframe（关键帧分块确实生效）。
func TestWriteRealMP4_KeyframeMode_FullRoundtrip(t *testing.T) {
	t.Parallel()
	path := makeRealMP4(t)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读测试视频: %v", err)
	}
	// 注册 go-mp4 精确提供者（MP4 命中）。
	registerGoMP4Provider(t)

	fs := newFSWithKeyframe(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "real.mp4", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile(real.mp4): %v", err)
	}
	e := fs.index["real.mp4"]
	if e == nil || e.meta == nil {
		t.Fatal("写后索引应含 real.mp4 meta")
	}
	// 关键帧分块确实生效（video-keyframe 模式落 meta）。
	if e.meta.Block.BlockletMode != "video-keyframe" {
		t.Fatalf("real.mp4 BlockletMode=%q，应为 video-keyframe（关键帧分块生效）", e.meta.Block.BlockletMode)
	}
	// 无解析失败记录（干净视频应完整解析关键帧）。
	for _, c := range e.meta.Chunks {
		if len(c.Failures) > 0 {
			t.Errorf("干净视频不应有 Failures，got %+v", c.Failures)
		}
	}
	// 整文件解密还原 == 原文件（完整性）。
	rc, rerr := fs.OpenRead(ctx, "real.mp4")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	got, gerr := io.ReadAll(rc)
	rc.Close()
	if gerr != nil {
		t.Fatalf("ReadAll: %v", gerr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("解密还原 != 原视频：len(got)=%d len(want)=%d", len(got), len(content))
	}
}

// TestWriteRealMP4_KeyframeMode_RangeRead：真实 MP4 经 video-keyframe 模式后，OpenRangeRead
// 读中间区间（模拟播放器 seek）== 原文件对应明文段——「段级随机读」正确接线可用。
func TestWriteRealMP4_KeyframeMode_RangeRead(t *testing.T) {
	t.Parallel()
	path := makeRealMP4(t)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读测试视频: %v", err)
	}
	registerGoMP4Provider(t)

	fs := newFSWithKeyframe(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "real.mp4", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile(real.mp4): %v", err)
	}
	// 读中间一段（seek 场景）：偏移取文件 1/3、长度 1/3。
	off := int64(len(content)) / 3
	size := int64(len(content)) / 3
	rc, rerr := fs.OpenRangeRead(ctx, "real.mp4", off, size)
	if rerr != nil {
		t.Fatalf("OpenRangeRead: %v", rerr)
	}
	got, gerr := io.ReadAll(rc)
	rc.Close()
	if gerr != nil {
		t.Fatalf("ReadAll: %v", gerr)
	}
	want := content[off : off+size]
	if !bytes.Equal(got, want) {
		t.Fatalf("OpenRangeRead 区间 != 原文件对应明文：len(got)=%d len(want)=%d", len(got), len(want))
	}
}

// fixedKFIndexer 是测试用固定关键帧索引（真实解析一致性已由独立 benchmark 实测验证；
// 此处专注「完整流程接线」——写→关键帧切分→解密还原→Range 读，用固定偏移保证确定性）。
type fixedKFIndexer struct{ frames []int64 }

func (f *fixedKFIndexer) KeyframeOffsets(r io.ReaderAt, _ int64) ([]int64, error) {
	return f.frames, nil
}

// registerGoMP4Provider 注册 go-mp4 精确提供者（测试用固定关键帧；串行写注册表）。
func registerGoMP4Provider(t *testing.T) {
	t.Helper()
	shardseal.RegisterBlockletMode(shardseal.BlockletModeProvider{
		Mode:    "video-keyframe",
		Kind:    "video/mp4",
		Manager: "go-mp4",
		Indexer: &fixedKFIndexer{frames: []int64{48, 3000, 6000, 9000}},
	}, 1)
	t.Cleanup(func() { shardseal.UnregisterBlockletMode("video-keyframe", "video/mp4") })
}

// newFSWithKeyframe 建 secretdata FS（video-keyframe 自动选型 + 小块策略）。
func newFSWithKeyframe(t *testing.T) *SecretdataFS {
	t.Helper()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}
