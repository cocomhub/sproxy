// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestResolveShareTTL_Units 锁 TTL 解析口径：空=24h、Go duration、`d`/`w` 后缀、数字天数、上限夹紧、非法拒绝。
//
// 为什么需要（实测缺陷 2026-11-25）：Web UI「有效期」下拉传的是**天数数字**（1/7/30），
// 而旧实现只接受 Go duration 字符串 ⇒ `{"ttl":7}` 直接 400「请求体解析失败」，
// v2 的「创建分享」功能实际**从未成功过**。
func TestResolveShareTTL_Units(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 24 * time.Hour, true},
		{"24h", 24 * time.Hour, true},
		{"90m", 90 * time.Minute, true},
		{"7d", 7 * 24 * time.Hour, true},  // 常见人类写法
		{"2w", 14 * 24 * time.Hour, true}, // 周
		{"7", 7 * 24 * time.Hour, true},   // 纯数字 = 天（UI 下拉语义）
		{"1", 24 * time.Hour, true},
		{"60d", 30 * 24 * time.Hour, true}, // 夹紧到上限
		{"0", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"7x", 0, false},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		got, ok := resolveShareTTL(rec, tc.in)
		if ok != tc.ok {
			t.Fatalf("ttl=%q ok=%v want %v (body=%s)", tc.in, ok, tc.ok, rec.Body.String())
		}
		if ok && got != tc.want {
			t.Fatalf("ttl=%q = %v want %v", tc.in, got, tc.want)
		}
	}
}
