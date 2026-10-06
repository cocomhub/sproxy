// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package video

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/integrity"
)

// writeBytes 写原始字节到临时文件（扩展名由参数指定，供 Matches 判定）。
func writeBytes(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestVideoChecker_Matches：扩展名族匹配（对齐 pkg/integrity §77 规则）。
func TestVideoChecker_Matches(t *testing.T) {
	t.Parallel()
	c := VideoChecker{}
	for _, name := range []string{"a.mp4", "b.mkv", "c.webm", "d.mov", "e.ts", "f.avi", "g.flv", "h.wmv", "u.MP4"} {
		if !c.Matches(name) {
			t.Errorf("Matches(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"x.txt", "y.tar", "z.png"} {
		if c.Matches(name) {
			t.Errorf("Matches(%q) = true, want false", name)
		}
	}
}

// TestVideoChecker_Kind：类型标识为注册键。
func TestVideoChecker_Kind(t *testing.T) {
	t.Parallel()
	if (VideoChecker{}).Kind() != "video/*" {
		t.Fatal("Kind 应为 video/*")
	}
}

// TestVideoChecker_Corrupt：非视频字节（即使 .mp4 扩展名）→ OK=false（不回 error）。
func TestVideoChecker_Corrupt(t *testing.T) {
	t.Parallel()
	path := writeBytes(t, "corrupt.mp4", []byte("not-a-video"))
	rep, err := VideoChecker{}.Check(context.Background(), path, int64(len("not-a-video")))
	if err != nil {
		t.Fatalf("损坏视频应返回 OK=false 语义而非 error，got %v", err)
	}
	if rep.OK {
		t.Fatal("损坏视频应失败")
	}
}

// TestVideoChecker_Nonexistent：路径不存在 → error（校验执行错误，非语义判定）。
func TestVideoChecker_Nonexistent(t *testing.T) {
	t.Parallel()
	_, err := VideoChecker{}.Check(context.Background(), filepath.Join(t.TempDir(), "missing.mp4"), 0)
	if err == nil {
		t.Fatal("不存在的路径应返回 error")
	}
}

// TestVideoChecker_ZeroSize：空文件 → ffprobe 解析失败 → OK=false。
func TestVideoChecker_ZeroSize(t *testing.T) {
	t.Parallel()
	path := writeBytes(t, "empty.mp4", nil)
	rep, err := VideoChecker{}.Check(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("空文件应返回 OK=false 语义而非 error，got %v", err)
	}
	if rep.OK {
		t.Fatal("空文件应失败")
	}
}

// TestVideoChecker_RegistryLookup：空白导入注册后，Lookup 按扩展名分发命中 video/*。
func TestVideoChecker_RegistryLookup(t *testing.T) {
	t.Parallel()
	c := integrity.Lookup("movie.mp4")
	if c == nil {
		t.Fatal("Lookup(movie.mp4) 应命中已注册校验器")
	}
	if got := c.Kind(); got != "video/*" {
		t.Fatalf("Lookup Kind = %q, want video/*", got)
	}
}
