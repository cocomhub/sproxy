// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// registerVideoKeyframe 注册 MP4 容器族关键帧提供者并返回清理函数。
// 串行：写共享 blocklet 注册表（其它用例并行只读），隔离避免数据竞争。
func registerVideoKeyframe(t *testing.T) func() {
	t.Helper()
	shardseal.RegisterBlockletMode(shardseal.BlockletModeProvider{
		Mode: "video-keyframe", Kind: "video/mp4", Manager: "test-fake",
		Indexer: &videoKeyframeIndexer{},
	}, 1)
	return func() { shardseal.UnregisterBlockletMode("video-keyframe", "video/mp4") }
}

// videoKeyframeIndexer 是测试用假关键帧索引（不解析真实视频）。
type videoKeyframeIndexer struct{}

func (v *videoKeyframeIndexer) KeyframeOffsets(_ io.ReaderAt, _ int64) ([]int64, error) {
	return []int64{0}, nil
}

// TestWriteFile_VideoSelectsKeyframeMode：注册 video 关键帧提供者后，写 .mp4 →
// meta.BlockletMode == video-keyframe（自动选型命中）。
func TestWriteFile_VideoSelectsKeyframeMode(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表。
	cleanup := registerVideoKeyframe(t)
	defer cleanup()

	fs := newFS(t)
	ctx := context.Background()
	content := data(1000)
	if err := fs.WriteFile(ctx, "clip.mp4", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile(clip.mp4): %v", err)
	}
	e := fs.index["clip.mp4"]
	if e == nil || e.meta == nil {
		t.Fatal("写后索引应含 clip.mp4 的 meta")
	}
	if e.meta.Block.BlockletMode != "video-keyframe" {
		t.Errorf("clip.mp4 BlockletMode=%q，应为 video-keyframe（自动选型）", e.meta.Block.BlockletMode)
	}
}

// TestWriteFile_NonVideoKeepsFixed：写 .txt → BlockletMode 保持默认 fixed（全部未命中
// 回落默认分块策略）。
func TestWriteFile_NonVideoKeepsFixed(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表。
	cleanup := registerVideoKeyframe(t)
	defer cleanup()

	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "note.txt", bytes.NewReader(data(500)), 500, 0); err != nil {
		t.Fatalf("WriteFile(note.txt): %v", err)
	}
	e := fs.index["note.txt"]
	if e == nil || e.meta == nil {
		t.Fatal("写后索引应含 note.txt 的 meta")
	}
	if e.meta.Block.BlockletMode != "fixed" {
		t.Errorf("note.txt BlockletMode=%q，应为默认 fixed（非视频回落）", e.meta.Block.BlockletMode)
	}
}

// TestWriteFile_NoProviderKeepsFixed：未注册任何 video 提供者时，写 .mp4 也回落默认
// fixed（全部未命中 → 默认分块策略，不报错）。
func TestWriteFile_NoProviderKeepsFixed(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "clip.mp4", bytes.NewReader(data(600)), 600, 0); err != nil {
		t.Fatalf("WriteFile(clip.mp4 未注册): %v", err)
	}
	e := fs.index["clip.mp4"]
	if e == nil || e.meta == nil {
		t.Fatal("写后索引应含 clip.mp4 的 meta")
	}
	if e.meta.Block.BlockletMode != "fixed" {
		t.Errorf("未注册时 BlockletMode=%q，应为默认 fixed", e.meta.Block.BlockletMode)
	}
}

// TestWriteFile_NonMP4ContainerKeepsFixed（I2 回归）：.mkv/.webm/.avi 是无解析器的容器族
// （MediaKindOf 返回空）——即使注册了 video/mp4 提供者，也**不**命中（不「宣称支持实为
// 必败降级」），回落默认 fixed。
func TestWriteFile_NonMP4ContainerKeepsFixed(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表。
	cleanup := registerVideoKeyframe(t)
	defer cleanup()

	fs := newFS(t)
	ctx := context.Background()
	for _, name := range []string{"clip.mkv", "clip.webm", "clip.avi"} {
		if err := fs.WriteFile(ctx, name, bytes.NewReader(data(500)), 500, 0); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
		e := fs.index[name]
		if e == nil || e.meta == nil {
			t.Fatalf("写后索引应含 %s 的 meta", name)
		}
		if e.meta.Block.BlockletMode != "fixed" {
			t.Errorf("%s BlockletMode=%q，应为默认 fixed（非 MP4 容器族无解析器回落）", name, e.meta.Block.BlockletMode)
		}
	}
}

// TestWriteFile_ExplicitBlockletModeFixed（评审 I-3 配置开关）：显式配置 blocklet_mode=fixed
// 时，即使视频文件也**不**自动选型 keyframe（运维可显式关闭关键帧分块）。
func TestWriteFile_ExplicitBlockletModeFixed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128, BlockletMode: "fixed"},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "clip.mp4", bytes.NewReader(data(500)), 500, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := fs.index["clip.mp4"]
	if e.meta.Block.BlockletMode != "fixed" {
		t.Errorf("显式 blocklet_mode=fixed 应保持 fixed（不自动选型），got %q", e.meta.Block.BlockletMode)
	}
}

// TestWriteFile_ExplicitBlockletModeKeyframe（配置开关）：显式 blocklet_mode=video-keyframe
// 强制关键帧分块（即使文件非视频也生效）。
func TestWriteFile_ExplicitBlockletModeKeyframe(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表。
	cleanup := registerVideoKeyframe(t)
	defer cleanup()

	root := t.TempDir()
	fs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128, BlockletMode: "video-keyframe"},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "note.txt", bytes.NewReader(data(500)), 500, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := fs.index["note.txt"]
	if e.meta.Block.BlockletMode != "video-keyframe" {
		t.Errorf("显式 video-keyframe 应强制生效，got %q", e.meta.Block.BlockletMode)
	}
}
