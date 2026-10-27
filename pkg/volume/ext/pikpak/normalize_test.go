// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// fakeFollowNormalizer 是测试专用：把 keepshare 源 → 目标映射表（隔离真实 HTTP）。
// 与生产 NormalizeShareSource 逻辑同构：keepshare → 真实分享 URL（保留 spx_target），
// mypikpak 原样返回；无映射 = 失效（错误）。
type fakeFollowNormalizer struct {
	table map[string]string
}

func newFakeFollowNormalizer(tb testing.TB, table map[string]string) *fakeFollowNormalizer {
	tb.Helper()
	return &fakeFollowNormalizer{table: table}
}

func (f *fakeFollowNormalizer) NormalizeShareSource(_ context.Context, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !isKeepshareMirror(raw) {
		return raw, nil
	}
	target := parseShareTarget(raw)
	dst, ok := f.table[stripShareTarget(raw)]
	if !ok || dst == "" {
		return "", fmt.Errorf("fake follow: keepshare 无映射（视为失效）: %s", raw)
	}
	out := dst
	if target != "" {
		out += "?spx_target=" + url.QueryEscape(target)
	}
	return out, nil
}

// TestNormalizeShareSource_KeepShare 红灯测试：keepshare 容器 id 必须归一为真实 mypikpak 分享 id。
func TestNormalizeShareSource_KeepShare(t *testing.T) {
	t.Parallel()
	n := newFakeFollowNormalizer(t, map[string]string{
		"https://keepshare.org/p5uj7smo/magnet:?xt=urn%3Abtih%3Aabc": "https://mypikpak.com/s/VOyUFLrf4RJcADwdFrpb5Cedo2",
	})
	got, err := n.NormalizeShareSource(context.Background(),
		"https://keepshare.org/p5uj7smo/magnet:?xt=urn:btih:abc&spx_target=foo.mp4")
	if err != nil {
		t.Fatalf("NormalizeShareSource: %v", err)
	}
	want := "https://mypikpak.com/s/VOyUFLrf4RJcADwdFrpb5Cedo2?spx_target=foo.mp4"
	if got != want {
		t.Fatalf("NormalizeShareSource = %q, want %q", got, want)
	}
}

// TestNormalizeShareSource_MyPikPak 原样返回（非 keepshare 不 follow）。
func TestNormalizeShareSource_MyPikPak(t *testing.T) {
	t.Parallel()
	n := newFakeFollowNormalizer(t, nil)
	in := "https://mypikpak.com/s/VOyUFLrf4RJcADwdFrpb5Cedo2?spx_target=a.mp4"
	got, err := n.NormalizeShareSource(context.Background(), in)
	if err != nil {
		t.Fatalf("NormalizeShareSource: %v", err)
	}
	if got != in {
		t.Fatalf("NormalizeShareSource = %q, want %q", got, in)
	}
}

// TestNormalizeShareSource_Invalid 失效分享（无映射）→ 错误（fail-closed）。
func TestNormalizeShareSource_Invalid(t *testing.T) {
	t.Parallel()
	n := newFakeFollowNormalizer(t, map[string]string{
		"https://keepshare.org/p5uj7smo/magnet:?xt=urn%3Abtih%3Adead": "",
	})
	if _, err := n.NormalizeShareSource(context.Background(),
		"https://keepshare.org/p5uj7smo/magnet:?xt=urn%3Abtih%3Adead"); err == nil {
		t.Fatal("NormalizeShareSource 应对失效分享报错")
	}
}

// TestShareSourceNormalizer_ReduceDuplicateResolution 收敛佐证：keepshare 容器 id 与真实
// 分享 id 不同（容器 id 不能直接当分享 id 用）；归一器是把它变成可解析形态的唯一路径。
// （parseShareIDWithToken 对 keepshare 仍返回容器 id——/api/resolve 路径自己 follow；但
// 下载路径必须经归一器，避免把容器 id 当分享 id。）
func TestShareSourceNormalizer_ReduceDuplicateResolution(t *testing.T) {
	t.Parallel()
	containerID, _ := parseShareIDWithToken("https://keepshare.org/p5uj7smo/magnet:?xt=urn:btih:abc")
	if containerID == "" {
		t.Fatal("keepshare 容器 id 应可提取（但它是容器 id，不是分享 id）")
	}
	realID, _ := parseShareIDWithToken("https://mypikpak.com/s/VOyUFLrf4RJcADwdFrpb5Cedo2")
	if realID == "" {
		t.Fatal("mypikpak 分享 id 应可提取")
	}
	if containerID == realID {
		t.Fatal("keepshare 容器 id 与真实分享 id 不应相同（否则无需归一）")
	}
}

// TestResolvedShareURL 契约测试：解析结果回填的 URL 必须是可直接定位分享的形态。
// keepshare 镜像（容器 id）→ 真实 mypikpak 分享 URL（sdserver 据此提交 cloud download 转存）。
func TestResolvedShareURL(t *testing.T) {
	t.Parallel()
	cases := []struct{ raw, shareID, want string }{
		{"https://keepshare.org/p5uj7smo/magnet:?xt=urn:btih:abc", "VOyUFLrf4RJcADwdFrpb5Cedo2",
			"https://mypikpak.com/s/VOyUFLrf4RJcADwdFrpb5Cedo2"},
		{"https://keepshare.cc/p5uj7smo/magnet:?xt=urn:btih:abc", "REALID",
			"https://mypikpak.com/s/REALID"},
		{"https://mypikpak.com/s/VOyUFLrf4RJcADwdFrpb5Cedo2", "VOyUFLrf4RJcADwdFrpb5Cedo2",
			"https://mypikpak.com/s/VOyUFLrf4RJcADwdFrpb5Cedo2"}, // 已归一 → 原样
		{"https://example.com/plain.mp4", "x", "https://example.com/plain.mp4"}, // 非分享 → 原样
	}
	for _, c := range cases {
		if got := resolvedShareURL(c.raw, c.shareID); got != c.want {
			t.Errorf("resolvedShareURL(%q, %q) = %q, want %q", c.raw, c.shareID, got, c.want)
		}
	}
}
