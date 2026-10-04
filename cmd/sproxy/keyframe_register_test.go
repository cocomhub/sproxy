// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/media/ffprobe"
)

// TestKeyframeProviderFor_FFprobePreferred：ffprobe 可用 → 通配 Kind="video" 的 ffprobe
// 提供者（任意 video/* 容器族命中，行为一致全走 ffprobe）。
func TestKeyframeProviderFor_FFprobePreferred(t *testing.T) {
	p := keyframeProviderFor(true)
	if p.Mode != "video-keyframe" || p.Kind != "video" || p.Manager != "ffprobe" {
		t.Fatalf("ffprobe 选型=%+v，应为 {video-keyframe video ffprobe}", p)
	}
	if p.Indexer == nil {
		t.Error("ffprobe 提供者应携带 Indexer")
	}
	// 通配 Kind=video 命中任意 video/* 容器族。
	for _, kind := range []string{"video/mp4", "video/mkv", "video/ts", "video/avi", "video"} {
		if !shardseal.KindPrefixMatch(p.Kind, kind) {
			t.Errorf("KindPrefixMatch(%q,%q) 应为 true", p.Kind, kind)
		}
	}
}

// TestKeyframeProviderFor_GoMP4Fallback：无 ffprobe → 精确 Kind="video/mp4" 的 go-mp4
// 提供者（仅 MP4 命中，其它容器族回落 fixed）。
func TestKeyframeProviderFor_GoMP4Fallback(t *testing.T) {
	p := keyframeProviderFor(false)
	if p.Mode != "video-keyframe" || p.Kind != "video/mp4" || p.Manager != "go-mp4" {
		t.Fatalf("go-mp4 选型=%+v，应为 {video-keyframe video/mp4 go-mp4}", p)
	}
	if p.Indexer == nil {
		t.Error("go-mp4 提供者应携带 Indexer")
	}
	// 用真实 go-mp4 Indexer 解析非 MP4 → 报错（不 panic）。
	if _, ierr := p.Indexer.KeyframeOffsets(shardseal.KeyframeRequest{Reader: bytes.NewReader([]byte("not mp4")), Size: 7}); ierr == nil {
		t.Error("非 MP4 输入 go-mp4 解析应报错")
	}
}

// TestRegisterKeyframeBackend_Unregister：装配注册 + 解绑（动态绑定/解绑语义验证）。
func TestRegisterKeyframeBackend_Unregister(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表。
	registerKeyframeProvider(keyframeProviderFor(false))
	t.Cleanup(func() { shardseal.UnregisterBlockletMode("video-keyframe", "video/mp4") })

	// 命中 go-mp4 精确提供者。
	reg, err := shardseal.ResolveBlockletMode("video/mp4")
	if err != nil {
		t.Fatalf("ResolveBlockletMode(video/mp4): %v", err)
	}
	if reg.Manager != "go-mp4" {
		t.Errorf("装配注册 Manager=%q，应为 go-mp4", reg.Manager)
	}
}

// TestRegisterKeyframeBackend_DualRegistration（真实文件实测决策，评审汇总）：双注册共存
// ——go-mp4 精确（**装配层注入 Fallback=[ffprobe]**，评审 C1 回归）+ ffprobe 通配。
// MP4 命中 go-mp4（先精确），MKV/TS/AVI 命中 ffprobe（通配）。
func TestRegisterKeyframeBackend_DualRegistration(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表。
	gp := keyframeProviderFor(false)
	gp.Fallback = []shardseal.KeyframeIndexer{ffprobe.Indexer{}}
	registerKeyframeProvider(gp)
	registerKeyframeProvider(keyframeProviderFor(true))
	t.Cleanup(func() {
		shardseal.UnregisterBlockletMode("video-keyframe", "video/mp4")
		shardseal.UnregisterBlockletMode("video-keyframe", "video")
	})

	// MP4 → go-mp4（精确优先，先于通配）。
	mp4p, err := shardseal.ResolveBlockletMode("video/mp4")
	if err != nil {
		t.Fatalf("ResolveBlockletMode(video/mp4): %v", err)
	}
	if mp4p.Manager != "go-mp4" {
		t.Errorf("MP4 应命中 go-mp4（精确优先），got %q", mp4p.Manager)
	}
	// **C1 回归断言：装配注入后 go-mp4 提供者 Fallback 非空**（此前死代码：Fallback 注入
	// 放 hasFFprobe==true 分支恒不可达，四轮评审命中 Critical）。
	if len(mp4p.Fallback) == 0 {
		t.Error("装配后 go-mp4 提供者 Fallback 应非空（[ffprobe] 兜底伪装容器）")
	}
	// 非 MP4 容器 → ffprobe（通配命中）。
	for _, kind := range []string{"video/mkv", "video/ts", "video/avi"} {
		p, err := shardseal.ResolveBlockletMode(kind)
		if err != nil {
			t.Fatalf("ResolveBlockletMode(%s): %v", kind, err)
		}
		if p.Manager != "ffprobe" {
			t.Errorf("%s 应命中 ffprobe（通配），got %q", kind, p.Manager)
		}
	}
}

// TestKindPrefixMatch：通配 Kind 前缀匹配语义（video ↔ video/mp4 等）。
func TestKindPrefixMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		provider string
		request  string
		want     bool
	}{
		{"video", "video/mp4", true},
		{"video", "video/mkv", true},
		{"video", "video", true},
		{"video", "image/png", false},
		{"video/mp4", "video/mp4", true},
		{"video/mp4", "video/mkv", false},
		{"", "video/mp4", false},
	}
	for _, c := range cases {
		if got := shardseal.KindPrefixMatch(c.provider, c.request); got != c.want {
			t.Errorf("KindPrefixMatch(%q,%q)=%v，应为 %v", c.provider, c.request, got, c.want)
		}
	}
}
