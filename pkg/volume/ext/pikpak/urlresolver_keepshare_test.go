// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"strings"
	"testing"
)

// TestURLResolver_Supports_KeepShare 回归：keepshare.org/<id>/magnet:... 镜像 URL
// 必须被 Supports 识别（当前只认 /s/<id> 路径 → 恒回落 generic，resolve 拿不到 hash）。
// 判据与 parseShareIDWithToken 一致（keepshare 主机 parts[0] 即分享 id）。
func TestURLResolver_Supports_KeepShare(t *testing.T) {
	t.Parallel()
	r := NewURLResolver(nil, nil)
	cases := []struct {
		url  string
		want bool
	}{
		{"https://mypikpak.com/s/abc", true},
		{"https://mypikpak.com/s/abc/tok123", true},
		{"https://keepshare.org/k1/magnet:?xt=urn:btih:abc", true},
		{"https://keepshare.cc/k2/magnet:?xt=urn:btih:abc", true},
		{"https://keepshare.org/k3/magnet:?xt=urn:btih:abc&dn=xxx", true},
		{"https://example.com/s/abc", false},
		{"https://keepshare.org", false},
	}
	for _, c := range cases {
		if got := r.Supports(c.url); got != c.want {
			t.Errorf("Supports(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// TestURLResolver_Resolve_KeepShareShareID 验证 keepshare URL 解析出的 share id
// 与直接 parseShareID 一致（keepshare parts[0]，无需 302 跟随）。
func TestURLResolver_Resolve_KeepShareShareID(t *testing.T) {
	t.Parallel()
	raw := "https://keepshare.org/k1/magnet:?xt=urn:btih:abc&dn=EXAMPLE-254"
	id, err := parseShareID(raw)
	if err != nil {
		t.Fatalf("parseShareID(keepshare) 失败: %v", err)
	}
	if id != "k1" {
		t.Fatalf("share id = %q, want k1", id)
	}
	// stripShareTarget 不破坏 keepshare 路径
	stripped := stripShareTarget(raw)
	if !strings.Contains(stripped, "keepshare.org/k1") {
		t.Fatalf("stripShareTarget 破坏了 URL: %q", stripped)
	}
}
