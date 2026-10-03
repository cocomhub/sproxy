// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// TestRegisterKeyframeBackend：装配注册 video-keyframe 提供者（Kind=video），
// ResolveBlockletMode("video") 命中唯一提供者（Manager=go-mp4）；重复装配幂等。
func TestRegisterKeyframeBackend(t *testing.T) {
	// 串行：写共享 blocklet 注册表，隔离避免数据竞争。
	registerKeyframeBackend()
	defer shardseal.UnregisterBlockletMode("video-keyframe", "video")

	// 再调一次（sync.Once 幂等，不 panic、不覆盖）。
	registerKeyframeBackend()

	reg, err := shardseal.ResolveBlockletMode("video")
	if err != nil {
		t.Fatalf("ResolveBlockletMode(video): %v", err)
	}
	if reg.Mode != "video-keyframe" || reg.Kind != "video" || reg.Manager != "go-mp4" {
		t.Errorf("装配注册=%+v，应为 {video-keyframe video go-mp4}", reg)
	}
	if reg.Indexer == nil {
		t.Error("装配注册应携带 Indexer（go-mp4 解析器）")
	}
	// 用真实 Indexer 解析非 MP4 → 报错（不 panic）。
	if _, ierr := reg.Indexer.KeyframeOffsets(bytes.NewReader([]byte("not mp4")), 7); ierr == nil {
		t.Error("非 MP4 输入解析应报错")
	}
}
