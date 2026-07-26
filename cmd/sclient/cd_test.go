// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
)

func TestResolveRemotePath(t *testing.T) {
	cases := []struct {
		name       string
		currentDir string
		input      string
		want       string
		wantErr    bool
	}{
		{"empty_input_root", "", "", "", false},
		{"empty_input_with_currentdir", "sub", "", "sub", false},
		{"relative_root", "", "file.txt", "file.txt", false},
		{"relative_with_currentdir", "sub", "file.txt", "sub/file.txt", false},
		{"absolute_skips_currentdir", "sub", "/abs/file.txt", "abs/file.txt", false},
		{"trailing_slash_cleaned", "sub", "file.txt/", "sub/file.txt", false},
		{"dot_no_op", "sub", ".", "sub", false},
		{"reject_parent_top", "", "..", "", true},
		{"reject_parent_relative", "", "../etc/passwd", "", true},
		{"reject_parent_after_currentdir_resolve", "sub", "../../etc", "", true},
		{"parent_cancels_currentdir_one_level", "sub", "../foo", "", true},
		{"internal_parent_cleaned_to_safe", "", "a/../b", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &state.State{CurrentDir: c.currentDir}
			got, err := s.ResolveRemotePath(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q (currentDir=%q), got nil (returned %q)", c.input, c.currentDir, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.input, err)
			}
			if got != c.want {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

func TestResolveRemotePath_ParentRefMessage(t *testing.T) {
	s := &state.State{CurrentDir: ""}
	_, err := s.ResolveRemotePath("../escape.txt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "父级引用") {
		t.Fatalf("error message should mention parent ref: %v", err)
	}
}

// loadCurrentDirCachePath 返回 loadCurrentDir 使用的缓存文件路径（基于 XDG_CACHE_HOME）。
func loadCurrentDirCachePath() string {
	path, err := xdg.CacheFile(filepath.Join("sproxy", "current_dir"))
	if err != nil {
		return ""
	}
	return path
}

// setCacheDirForTest 设置 XDG_CACHE_HOME 环境变量并调用 xdg.Reload()，
// 使 loadCurrentDir 使用隔离的临时目录。
// 返回 cleanup 函数，恢复原始环境变量。
func setCacheDirForTest(t *testing.T) string {
	t.Helper()
	oldCacheHome := os.Getenv("XDG_CACHE_HOME")
	tmpDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmpDir)
	xdg.Reload()
	t.Cleanup(func() {
		os.Setenv("XDG_CACHE_HOME", oldCacheHome)
		xdg.Reload()
	})
	return tmpDir
}

func TestLoadCurrentDir_FileExists(t *testing.T) {
	_ = setCacheDirForTest(t)
	cachePath := loadCurrentDirCachePath()
	if cachePath == "" {
		t.Skip("xdg.CacheFile 返回空路径")
	}

	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("subdir"), 0644); err != nil {
		t.Fatal(err)
	}
	got := loadCurrentDir()
	if got != "subdir" {
		t.Errorf("expected 'subdir', got %q", got)
	}
}

func TestLoadCurrentDir_FileNotExists(t *testing.T) {
	_ = setCacheDirForTest(t)
	got := loadCurrentDir()
	if got != "" {
		t.Errorf("expected empty string for missing file, got %q", got)
	}
}

func TestLoadCurrentDir_FileWithWhitespace(t *testing.T) {
	_ = setCacheDirForTest(t)
	cachePath := loadCurrentDirCachePath()
	if cachePath == "" {
		t.Skip("xdg.CacheFile 返回空路径")
	}

	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("  nested/path  \n"), 0644); err != nil {
		t.Fatal(err)
	}
	got := loadCurrentDir()
	if got != "nested/path" {
		t.Errorf("expected 'nested/path', got %q", got)
	}
}
