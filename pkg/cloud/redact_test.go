// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import "testing"

// TestRedactURLForLog 锁定日志脱敏口径（2026-11-26）：分享 token / 签名 query 不得进日志。
func TestRedactURLForLog(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		// 分享链接（第二段起视为凭据）
		{"https://mypikpak.com/s/EXAMPLEID001", "https://mypikpak.com/s/***"},
		{"https://keepshare.org/s/ABC123/token-xyz", "https://keepshare.org/s/***"},
		// 单段且像文件名：保留，排障有用
		{"http://127.0.0.1:18192/src4k.bin", "http://127.0.0.1:18192/src4k.bin"},
		// 单段无扩展名（多为分享 token）：掩码
		{"https://keepshare.org/AbCdEf1234567890", "https://keepshare.org/***"},
		// query/fragment 一律丢弃
		{"https://cdn.example.com/file.bin?sig=secret#frag", "https://cdn.example.com/file.bin?***"},
		{"https://mypikpak.com/s/ID?token=abc", "https://mypikpak.com/s/***?***"},
		// 无路径 / 根路径
		{"https://example.com", "https://example.com"},
		{"https://example.com/", "https://example.com"},
		// 非法输入：宁可不记
		{"://bad", "***"},
		{"", ""},
	}
	for _, c := range cases {
		if got := redactURLForLog(c.in); got != c.want {
			t.Errorf("redactURLForLog(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
