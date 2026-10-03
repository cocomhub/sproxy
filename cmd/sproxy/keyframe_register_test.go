// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// TestRegisterKeyframeBackend：装配注册 video-keyframe 提供者（Kind=video），
// ResolveBlockletMode("video/mp4") 命中唯一提供者（Manager=go-mp4）；重复装配幂等。
func TestRegisterKeyframeBackend(t *testing.T) {
	// sproxy:serial: 写共享 blocklet 注册表（其它用例并行只读），隔离避免数据竞争。
	registerKeyframeBackend()
	defer shardseal.UnregisterBlockletMode("video-keyframe", "video/mp4")

	// 再调一次（sync.Once 幂等，不 panic、不覆盖）。
	registerKeyframeBackend()

	reg, err := shardseal.ResolveBlockletMode("video/mp4")
	if err != nil {
		t.Fatalf("ResolveBlockletMode(video/mp4): %v", err)
	}
	if reg.Mode != "video-keyframe" || reg.Kind != "video/mp4" || reg.Manager != "go-mp4" {
		t.Errorf("装配注册=%+v，应为 {video-keyframe video/mp4 go-mp4}", reg)
	}
	if reg.Indexer == nil {
		t.Error("装配注册应携带 Indexer（go-mp4 解析器）")
	}
	// 用真实 Indexer 解析非 MP4 → 报错（不 panic）。
	if _, ierr := reg.Indexer.KeyframeOffsets(bytes.NewReader([]byte("not mp4")), 7); ierr == nil {
		t.Error("非 MP4 输入解析应报错")
	}
	// 非 MP4 容器族（mkv/webm/avi）不命中注册表 → 回落 fixed（I2 口径）。
	other, oerr := shardseal.ResolveBlockletMode("video/webm")
	if oerr != nil {
		t.Fatalf("ResolveBlockletMode(video/webm): %v", oerr)
	}
	if other.Mode != "fixed" {
		t.Errorf("未注册容器族模式=%q，应为 fixed", other.Mode)
	}
}
