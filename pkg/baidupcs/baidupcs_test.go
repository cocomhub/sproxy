// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"strings"
	"testing"
)

func TestNewClient_RequiresBDUSS(t *testing.T) {
	t.Parallel()
	// 空 BDUSS → 错误。
	if _, err := NewClient(ClientConfig{BDUSS: ""}); err == nil {
		t.Fatal("空 BDUSS 应报错")
	}
}

func TestNewClient_OK(t *testing.T) {
	t.Parallel()
	// 正常 BDUSS → 客户端可用。
	c, err := NewClient(ClientConfig{BDUSS: "test-bduss", AppID: 266719})
	if err != nil {
		t.Fatalf("NewClient 失败: %v", err)
	}
	if c == nil {
		t.Fatal("NewClient 返回 nil")
	}
	if c.PCS() == nil {
		t.Fatal("PCS() 返回 nil")
	}
}

func TestSanitizeRemotePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "/a/b/", want: "/a/b"},
		{in: "a/b", want: "/a/b"},
		{in: "/", want: "/"},
		{in: "", want: "/"},
		{in: "../etc", wantErr: true},
		{in: "/a/../b", wantErr: true},
		{in: `a\b`, wantErr: true}, // 反斜杠拒绝（非正斜杠分隔）
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := sanitizeRemotePath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("sanitizeRemotePath(%q) 应报错, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sanitizeRemotePath(%q) 失败: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("sanitizeRemotePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeRemotePath_RejectsAbsWindows(t *testing.T) {
	t.Parallel()
	// Windows 盘符形状（如 "C:/x"）必须拒绝（跨平台一致性）。
	if _, err := sanitizeRemotePath("C:/x"); err == nil {
		t.Fatal("盘符形状路径应报错")
	}
	if strings.Contains("C:/x", "/") == false {
		t.Fatal("测试前提错误")
	}
}
