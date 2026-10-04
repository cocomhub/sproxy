// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"errors"
	"testing"
)

// fakeIndexer 是测试用 KeyframeIndexer：返回固定的关键帧偏移序列。
type fakeIndexer struct{ frames []int64 }

func (f *fakeIndexer) KeyframeOffsets(_ KeyframeRequest) ([]int64, error) {
	return f.frames, nil
}

// TestResolveBlockletMode_UnregisteredReturnsFixed：未注册任何 keyframe 提供者时，
// ResolveBlockletMode 回落内置 fixed（blockletReg 的 builtin）。
func TestResolveBlockletMode_UnregisteredReturnsFixed(t *testing.T) {
	t.Parallel()
	p, err := ResolveBlockletMode("video/mp4")
	if err != nil {
		t.Fatalf("未注册时 ResolveBlockletMode: %v", err)
	}
	if p.Mode != "fixed" {
		t.Errorf("未注册时模式=%q，应为内置 fixed", p.Mode)
	}
}

// TestRegisterBlockletMode_ResolveHit：注册一个 video/mp4 提供者后，同 Kind 解析命中该
// 提供者；不同 Kind 回落 fixed；Unregister 后再解析回落 fixed。
func TestRegisterBlockletMode_ResolveHit(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表（其它用例并行只读），隔离避免数据竞争。
	defer blockletReg.Clear()

	idx := &fakeIndexer{frames: []int64{0, 1000, 2400}}
	RegisterBlockletMode(BlockletModeProvider{
		Mode: "video-keyframe", Kind: "video/mp4", Manager: "go-mp4", Indexer: idx,
	}, 1)
	t.Cleanup(func() { UnregisterBlockletMode("video-keyframe", "video/mp4") })

	p, err := ResolveBlockletMode("video/mp4")
	if err != nil {
		t.Fatalf("ResolveBlockletMode(video/mp4): %v", err)
	}
	if p.Mode != "video-keyframe" || p.Kind != "video/mp4" {
		t.Errorf("命中模式=%+v，应为 {video-keyframe video/mp4}", p)
	}
	if p.Indexer == nil {
		t.Error("命中提供者应携带 Indexer")
	}

	// 不同 Kind 未命中 → 回落 fixed。
	other, err := ResolveBlockletMode("video/webm")
	if err != nil {
		t.Fatalf("ResolveBlockletMode(video/webm): %v", err)
	}
	if other.Mode != "fixed" {
		t.Errorf("未命中 Kind 模式=%q，应为 fixed", other.Mode)
	}

	// Unregister 后再解析回落 fixed。
	UnregisterBlockletMode("video-keyframe", "video/mp4")
	after, err := ResolveBlockletMode("video/mp4")
	if err != nil {
		t.Fatalf("Unregister 后 ResolveBlockletMode: %v", err)
	}
	if after.Mode != "fixed" {
		t.Errorf("Unregister 后模式=%q，应为 fixed", after.Mode)
	}
}

// TestResolveBlockletMode_Conflict：同 Kind 两个异名提供者 → ErrPlannerConflict
// （secretdata 绑定失败，不静默二择一）。
func TestResolveBlockletMode_Conflict(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表，隔离避免数据竞争。
	defer blockletReg.Clear()

	RegisterBlockletMode(BlockletModeProvider{
		Mode: "video-keyframe", Kind: "video/mp4", Manager: "go-mp4", Indexer: &fakeIndexer{},
	}, 1)
	RegisterBlockletMode(BlockletModeProvider{
		Mode: "video-keyframe", Kind: "video/mp4", Manager: "ffprobe", Indexer: &fakeIndexer{},
	}, 2)
	t.Cleanup(func() {
		UnregisterBlockletMode("video-keyframe", "video/mp4")
		UnregisterBlockletMode("video-keyframe", "video/mp4")
	})

	_, err := ResolveBlockletMode("video/mp4")
	if !errors.Is(err, ErrPlannerConflict) {
		t.Fatalf("同 Kind 两提供者应 ErrPlannerConflict，got: %v", err)
	}
}

// TestRegisterBlockletMode_DuplicateOverrides：同 Kind+Mode+Manager 同名注册 → 后注册
// 覆盖前注册（pkg/plugin 既有语义，非冲突）。
func TestRegisterBlockletMode_DuplicateOverrides(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表，隔离避免数据竞争。
	defer blockletReg.Clear()

	RegisterBlockletMode(BlockletModeProvider{
		Mode: "video-keyframe", Kind: "video/mp4", Manager: "go-mp4", Indexer: &fakeIndexer{frames: []int64{0}},
	}, 1)
	RegisterBlockletMode(BlockletModeProvider{
		Mode: "video-keyframe", Kind: "video/mp4", Manager: "go-mp4", Indexer: &fakeIndexer{frames: []int64{0, 500}},
	}, 1)
	t.Cleanup(func() { UnregisterBlockletMode("video-keyframe", "video/mp4") })

	p, err := ResolveBlockletMode("video/mp4")
	if err != nil {
		t.Fatalf("ResolveBlockletMode: %v", err)
	}
	got, gerr := p.Indexer.KeyframeOffsets(KeyframeRequest{Reader: bytes.NewReader(nil), Size: 0})
	if gerr != nil {
		t.Fatalf("KeyframeOffsets: %v", gerr)
	}
	if len(got) != 2 {
		t.Errorf("同名覆盖后 Indexer 帧数=%d，应为 2（后注册覆盖）", len(got))
	}
}

// TestResolveBlockletMode_WildcardPrefixHit（评审 I-2 补）：注册通配 Kind="video"
// （ffprobe 生产装配路径）后，**容器族前缀匹配第二遍**命中 video/mkv、video/ts 等——
// 覆盖 ResolveBlockletMode 第二遍循环的组装行为（非 KindPrefixMatch 单测）。
func TestResolveBlockletMode_WildcardPrefixHit(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表，隔离避免数据竞争。
	defer blockletReg.Clear()

	idx := &fakeIndexer{frames: []int64{0, 1000}}
	RegisterBlockletMode(BlockletModeProvider{
		Mode: "video-keyframe", Kind: "video", Manager: "ffprobe", Indexer: idx,
	}, 1)
	t.Cleanup(func() { UnregisterBlockletMode("video-keyframe", "video") })

	// 任意 video/* 容器族经第二遍通配命中（生产 ffprobe 主路径）。
	for _, kind := range []string{"video/mp4", "video/mkv", "video/ts", "video/avi"} {
		p, err := ResolveBlockletMode(kind)
		if err != nil {
			t.Fatalf("ResolveBlockletMode(%s): %v", kind, err)
		}
		if p.Mode != "video-keyframe" || p.Kind != "video" || p.Manager != "ffprobe" {
			t.Errorf("ResolveBlockletMode(%s)=%+v，应为通配命中 {video-keyframe video ffprobe}", kind, p)
		}
	}
	// 非 video 前缀不命中 → 回落 fixed。
	other, err := ResolveBlockletMode("image/png")
	if err != nil {
		t.Fatalf("ResolveBlockletMode(image/png): %v", err)
	}
	if other.Mode != "fixed" {
		t.Errorf("非 video 前缀模式=%q，应为 fixed", other.Mode)
	}
}
