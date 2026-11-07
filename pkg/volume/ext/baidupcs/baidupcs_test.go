// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"strings"
	"testing"
)

func TestNewClient_RequiresBDUSS(t *testing.T) {
	t.Parallel()
	_, err := NewClient("", "stoken")
	if err == nil {
		t.Fatal("空 BDUSS 应返回错误")
	}
	if !strings.Contains(err.Error(), "bduss") {
		t.Fatalf("错误信息应含 bduss，got %q", err.Error())
	}
}

func TestNewClient_AcceptsBDUSS(t *testing.T) {
	t.Parallel()
	c, err := NewClient("dummy-bduss", "stoken")
	if err != nil {
		t.Fatalf("合法 BDUSS 不应报错: %v", err)
	}
	if c == nil {
		t.Fatal("client 不应为 nil")
	}
}

func TestSanitizeRemotePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"/a/b/", "/a/b"},
		{"/a/b", "/a/b"},
		{"", "/"},
		{"/", "/"},
		{"a/b", "/a/b"},
	}
	for _, tc := range cases {
		got, err := sanitizeRemotePath(tc.in)
		if err != nil {
			t.Fatalf("sanitizeRemotePath(%q) 不应报错: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("sanitizeRemotePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeRemotePath_RejectsDotDot(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"../etc", "/a/../b", "a/../../c"} {
		if _, err := sanitizeRemotePath(in); err == nil {
			t.Fatalf("sanitizeRemotePath(%q) 应拒绝 ..", in)
		}
	}
}
