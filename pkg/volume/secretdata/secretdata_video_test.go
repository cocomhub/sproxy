// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// registerVideoKeyframe 注册 video 类型的关键帧提供者并返回清理函数。
// 串行：写共享 blocklet 注册表（其它用例并行只读），隔离避免数据竞争。
func registerVideoKeyframe(t *testing.T) func() {
	t.Helper()
	shardseal.RegisterBlockletMode(shardseal.BlockletModeProvider{
		Mode: "video-keyframe", Kind: "video", Manager: "test-fake",
		Indexer: &videoKeyframeIndexer{},
	}, 1)
	return func() { shardseal.UnregisterBlockletMode("video-keyframe", "video") }
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
