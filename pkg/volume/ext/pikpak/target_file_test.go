// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"testing"
)

// filesAB 是分卷分享的测试样本（A/B 两个视频 + 一张图 + 广告小视频）。
func filesAB() []FileMeta {
	return []FileMeta{
		{ID: "id-a", Name: "EXAMPLE-001A.mp4", Kind: "drive#file", Size: 4_370_580_470, MimeType: "video/mp4"},
		{ID: "id-b", Name: "EXAMPLE-001B.mp4", Kind: "drive#file", Size: 4_070_000_000, MimeType: "video/mp4"},
		{ID: "id-jpg", Name: "EXAMPLE-001.jpg", Kind: "drive#file", Size: 200_000, MimeType: "image/jpeg"},
		{ID: "id-ad", Name: "manko.fun.mp4", Kind: "drive#file", Size: 11_000_000, MimeType: "video/mp4"},
	}
}

// TestPickVideoByTarget_ExactName 验证按文件名精确选中分卷（A 不被 B/广告/图片抢走）。
// 这是「分卷全套」的核心：同一分享内多次提交，每次指定不同 targetFile。
func TestPickVideoByTarget_ExactName(t *testing.T) {
	t.Parallel()
	files := filesAB()

	got := pickVideoByTarget(files, "EXAMPLE-001A.mp4")
	if got == nil || got.ID != "id-a" {
		t.Fatalf("target A = %+v, want id-a", got)
	}
	got = pickVideoByTarget(files, "EXAMPLE-001B.mp4")
	if got == nil || got.ID != "id-b" {
		t.Fatalf("target B = %+v, want id-b", got)
	}
}

// TestPickVideoByTarget_EmptyTargetFallsBackToLargest 验证 targetFile 为空时保持既有
// 「挑最大视频」行为（零回归：所有既有调用方不传 target）。
func TestPickVideoByTarget_EmptyTargetFallsBackToLargest(t *testing.T) {
	t.Parallel()
	got := pickVideoByTarget(filesAB(), "")
	if got == nil || got.ID != "id-a" {
		t.Fatalf("largest = %+v, want id-a (4.37GB)", got)
	}
}

// TestPickVideoByTarget_NilWhenNotFound 验证指定文件名不存在时返回 nil（不静默改选
// 其它文件——否则分卷会重复下载最大那个，导致内容错乱）。
func TestPickVideoByTarget_NilWhenNotFound(t *testing.T) {
	t.Parallel()
	if got := pickVideoByTarget(filesAB(), "EXAMPLE-001C.mp4"); got != nil {
		t.Fatalf("missing target = %+v, want nil", got)
	}
}

// TestPickVideoByTarget_CaseInsensitiveAndExtOptional 验证匹配忽略大小写、允许省略扩展名
// （分享内命名大小写混乱是常态；调用方可能传 code 名而非完整文件名）。
func TestPickVideoByTarget_CaseInsensitiveAndExtOptional(t *testing.T) {
	t.Parallel()
	files := filesAB()
	for _, target := range []string{"example-001a.mp4", "EXAMPLE-001A", "example-001a"} {
		if got := pickVideoByTarget(files, target); got == nil || got.ID != "id-a" {
			t.Fatalf("target %q = %+v, want id-a", target, got)
		}
	}
}

// TestParseShareTarget 验证从分享 URL 提取 spx_target 查询参数（调用方以 URL 内参数
// 表达「要分享内哪个文件」，无需新增 API 字段；同 share 不同 target ⇒ URL 不同，
// 云端任务去重不会误吸收）。
func TestParseShareTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
	}{
		{"https://mypikpak.com/s/EXAMPLE-001?spx_target=EXAMPLE-001A.mp4", "EXAMPLE-001A.mp4"},
		{"https://mypikpak.com/s/EXAMPLE-001?spx_target=EXAMPLE-001A%2Emp4", "EXAMPLE-001A.mp4"},
		{"https://mypikpak.com/s/EXAMPLE-001?x=1&spx_target=foo.avi&y=2", "foo.avi"},
		{"https://mypikpak.com/s/EXAMPLE-001", ""},
		{"https://keepshare.org/x/magnet:?xt=urn:btih:abc&spx_target=m.mp4", "m.mp4"},
	}
	for _, c := range cases {
		if got := parseShareTarget(c.raw); got != c.want {
			t.Fatalf("parseShareTarget(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestPickShareFileByTarget 验证匿名分享路径（[]ShareFile）同样支持按名选文件。
func TestPickShareFileByTarget(t *testing.T) {
	t.Parallel()
	files := []ShareFile{
		{ID: "a", Name: "EXAMPLE-001A.mp4", Kind: driveKindFile, Size: 100},
		{ID: "b", Name: "EXAMPLE-001B.mp4", Kind: driveKindFile, Size: 200},
	}
	if got := pickShareFileByTarget(files, "EXAMPLE-001B.mp4"); got == nil || got.ID != "b" {
		t.Fatalf("target B = %+v", got)
	}
	if got := pickShareFileByTarget(files, ""); got == nil || got.ID != "b" {
		t.Fatalf("largest = %+v, want b", got)
	}
	if got := pickShareFileByTarget(files, "nope.mp4"); got != nil {
		t.Fatalf("missing = %+v, want nil", got)
	}
}
