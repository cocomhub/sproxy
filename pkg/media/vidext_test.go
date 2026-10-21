// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package media

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// TestIsVideoName：常见视频扩展名全量命中（大小写不敏感）。
func TestIsVideoName(t *testing.T) {
	t.Parallel()
	exts := []string{
		".mp4", ".MOV", ".m4v", ".mkv", ".WebM", ".ts", ".m2ts", ".mts",
		".avi", ".divx", ".wmv", ".asf", ".rmvb", ".rm", ".flv", ".f4v",
		".mpg", ".mpeg", ".vob", ".mod", ".dat", ".ogv", ".3gp", ".3g2",
	}
	for _, ext := range exts {
		name := "clip" + ext
		if !IsVideoName(name) {
			t.Errorf("IsVideoName(%q) 应为 true", name)
		}
	}
	// 大小写混合（Windows/网盘上传常见）
	for _, name := range []string{"Movie.WMV", "视频.RMVB", "CLIP.MP4", "x.Avi"} {
		if !IsVideoName(name) {
			t.Errorf("IsVideoName(%q) 应为 true（大小写不敏感）", name)
		}
	}
}

// TestIsVideoName_NonVideo：非视频/无扩展名/目录不应命中。
func TestIsVideoName_NonVideo(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a.txt", "a.png", "a.mp3", "noext", "a.", "dir/"} {
		if IsVideoName(name) {
			t.Errorf("IsVideoName(%q) 应为 false", name)
		}
	}
}

// TestIsPlayableVideoName：浏览器可播子集 vs 全量集边界（B-M4 口径）。
func TestIsPlayableVideoName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a.mp4", "a.M4V", "a.webm", "a.mkv", "a.mov", "a.ts"} {
		if !IsPlayableVideoName(name) {
			t.Errorf("IsPlayableVideoName(%q) 应为 true（浏览器可播）", name)
		}
	}
	// 可校验但浏览器不原生播 → 播放入口不给，但仍是视频
	for _, name := range []string{"a.rmvb", "a.wmv", "a.flv", "a.avi", "a.mpg", "a.ogv", "a.3gp"} {
		if IsPlayableVideoName(name) {
			t.Errorf("IsPlayableVideoName(%q) 应为 false（浏览器不原生播放）", name)
		}
		if !IsVideoName(name) {
			t.Errorf("IsVideoName(%q) 应为 true（仍是视频，可 ffprobe 校验）", name)
		}
	}
}

// TestMediaKindOf_Consistency：全仓视频扩展名单一事实源对齐——本表每个扩展名在
// shardseal.MediaKindOf 都能得到非空容器族（选型不落空）；MediaKindOf 的容器族
// 覆盖点也都在本表（差集为空 → 无漂移）。两边语义不同（bool vs 容器族），但**覆盖
// 集合必须一致**——否则新增扩展名只改一处时另一处静默漏掉。
func TestMediaKindOf_Consistency(t *testing.T) {
	t.Parallel()
	for ext := range videoExt {
		if shardseal.MediaKindOf("f"+ext) == "" {
			t.Errorf("videoExt %q 在 shardseal.MediaKindOf 无容器族（选型会落空→fixed）", ext)
		}
	}
	// 反向：MediaKindOf 返回的每个容器族至少被本表一个扩展名覆盖。
	seen := map[string]bool{}
	for ext := range videoExt {
		seen[shardseal.MediaKindOf("f"+ext)] = true
	}
	for _, kind := range []string{"video/mp4", "video/mkv", "video/ts", "video/avi", "video/asf",
		"video/rm", "video/flv", "video/mpeg", "video/ogg", "video/3gpp"} {
		if !seen[kind] {
			t.Errorf("容器族 %q 无扩展名落入（videoExt 表缺失）", kind)
		}
	}
}

// TestExtOf_Edge：无扩展名/尾点/大小写边界（extOf 直接单测）。
func TestExtOf_Edge(t *testing.T) {
	t.Parallel()
	if extOf("", videoExt) {
		t.Error("空名不应命中")
	}
	if extOf("dir/", videoExt) {
		t.Error("尾斜杠不应命中")
	}
	if extOf("a.MP4", videoExt) != true {
		t.Error("a.MP4 应命中（ToLower 归一）")
	}
	if !strings.EqualFold("a.mp4", "a.MP4") {
		t.Error("EqualFold 自检")
	}
}
