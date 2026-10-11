// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// s3_fs_test.go 是不依赖 MinIO 容器的纯逻辑单测（路径映射/归一函数）。
// 协议级集成测试见 s3_fs_integration_test.go（MinIO 容器，S3_ENDPOINT 可达才实跑）。

import (
	"errors"
	"fmt"
	"testing"

	"github.com/minio/minio-go/v7"
)

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

// TestIsNotFound m8 修复：包装链中的 *minio.ErrorResponse 仍判定为不存在（errors.As
// 沿链查找）——原精确类型断言在 %w 包装后失效（Stat 把「不存在」当失败重试）。
func TestIsNotFound(t *testing.T) {
	t.Parallel()
	notFound := &minio.ErrorResponse{Code: "NoSuchKey"}
	// 直接 *ErrorResponse。
	if !isNotFound(notFound) {
		t.Fatal("NoSuchKey 应判定不存在")
	}
	// Unwrap 包装链（模拟 fmt.Errorf %w 的 errors.As 语义；vet 对 %w 值/指针挑剔，
	// 用自定义 Unwrap 包装器等价表达）。
	if !isNotFound(&wrapErr{err: notFound}) {
		t.Fatal("包装链中的 NoSuchKey 应判定不存在（errors.As 沿链查找）")
	}
	// 双层包装。
	if !isNotFound(&wrapErr{err: &wrapErr{err: notFound}}) {
		t.Fatal("双层包装仍应判定不存在")
	}
	// 非不存在错误 → false。
	if isNotFound(fmt.Errorf("some other error: %w", errors.New("boom"))) {
		t.Fatal("普通错误不应判定不存在")
	}
	if isNotFound(nil) {
		t.Fatal("nil 不应判定不存在")
	}
}

// wrapErr 是最小 Unwrap 错误包装器（测试 errors.As 沿链查找；等价 fmt.Errorf %w 语义）。
type wrapErr struct{ err error }

func (w *wrapErr) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrapErr) Unwrap() error { return w.err }
