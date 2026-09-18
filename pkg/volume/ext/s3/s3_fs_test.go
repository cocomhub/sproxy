// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// s3_fs_test.go 是不依赖 MinIO 容器的纯逻辑单测（路径映射/归一函数）。
// 协议级集成测试见 s3_fs_integration_test.go（MinIO 容器，S3_ENDPOINT 可达才实跑）。

import "testing"

// TestNormalizePrefix 验证卷根前缀归一（去首尾斜杠；空 = 桶根）。
func TestNormalizePrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/", ""},
		{"myprefix", "myprefix"},
		{"/myprefix/", "myprefix"},
		{"a/b/", "a/b"},
	}
	for _, tc := range cases {
		if got := normalizePrefix(tc.in); got != tc.want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestKeyFor 验证相对路径 → 对象键映射（prefix 拼接）。
func TestKeyFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prefix string
		rel    string
		want   string
	}{
		{"根无前缀根路径", "", "", ""},
		{"根无前缀文件", "", "a/b.txt", "a/b.txt"},
		{"有前缀文件", "vol", "a/b.txt", "vol/a/b.txt"},
		{"有前缀根", "vol", "", "vol"},
		{"前缀尾斜杠归一", "vol/", "x.txt", "vol/x.txt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &S3FS{prefix: normalizePrefix(tc.prefix)}
			if got := f.keyFor(tc.rel); got != tc.want {
				t.Errorf("keyFor(%q) with prefix %q = %q, want %q", tc.rel, tc.prefix, got, tc.want)
			}
		})
	}
}

// TestJoinRel 验证目录相对路径拼接。
func TestJoinRel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		dir, name, want string
	}{
		{"", "a.txt", "a.txt"},
		{"/", "a.txt", "a.txt"},
		{"dir", "a.txt", "dir/a.txt"},
		{"dir/", "a.txt", "dir/a.txt"},
		{"a/b", "c.txt", "a/b/c.txt"},
	}
	for _, tc := range cases {
		if got := joinRel(tc.dir, tc.name); got != tc.want {
			t.Errorf("joinRel(%q, %q) = %q, want %q", tc.dir, tc.name, got, tc.want)
		}
	}
}
