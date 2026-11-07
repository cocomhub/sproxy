// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
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

// ---- 卷感知工作目录（用户口径 2026-10-13）----

// runCdAndCapture 执行一次 cd（隔离 XDG 缓存），返回 stdout 与执行后的 state。
func runCdAndCapture(t *testing.T, st *state.State, args ...string) string {
	t.Helper()
	var out strings.Builder
	cmd := NewCmdCd(st, cli.IOStreams{Out: &out, ErrOut: io.Discard})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cd %v: %v", args, err)
	}
	return out.String()
}

func TestCd_VolumeURI_SwitchesVolumeAndDir(t *testing.T) {
	_ = setCacheDirForTest(t)
	st := &state.State{}
	runCdAndCapture(t, st, "vault:/docs/top")
	if st.CurrentVolume != "vault" || st.CurrentDir != "docs/top" {
		t.Fatalf("cd vault:/docs/top => vol=%q dir=%q", st.CurrentVolume, st.CurrentDir)
	}
	if got := loadCurrentVolume(); got != "vault" {
		t.Fatalf("卷应持久化：loadCurrentVolume()=%q", got)
	}
	if got := loadCurrentDir(); got != "docs/top" {
		t.Fatalf("目录应持久化：loadCurrentDir()=%q", got)
	}
	if got := st.Location(); got != "vault:/docs/top" {
		t.Fatalf("Location()=%q", got)
	}
	// `A:` / `A:/` = 卷根（保留卷、目录归零）。
	runCdAndCapture(t, st, "vault:")
	if st.CurrentVolume != "vault" || st.CurrentDir != "" {
		t.Fatalf("cd vault: => vol=%q dir=%q", st.CurrentVolume, st.CurrentDir)
	}
	// 相对路径保持当前卷。
	runCdAndCapture(t, st, "sub")
	if st.CurrentVolume != "vault" || st.CurrentDir != "sub" {
		t.Fatalf("cd sub => vol=%q dir=%q", st.CurrentVolume, st.CurrentDir)
	}
	// `..` 保持当前卷。
	runCdAndCapture(t, st, "..")
	if st.CurrentVolume != "vault" || st.CurrentDir != "" {
		t.Fatalf("cd .. => vol=%q dir=%q", st.CurrentVolume, st.CurrentDir)
	}
}

func TestCd_LeadingSlashReturnsToLocalVolume(t *testing.T) {
	_ = setCacheDirForTest(t)
	st := &state.State{CurrentVolume: "vault", CurrentDir: "docs"}
	saveCurrentVolumeValue("vault")
	runCdAndCapture(t, st, "/other")
	if st.CurrentVolume != "" || st.CurrentDir != "other" {
		t.Fatalf("cd /other 应回到本地卷：vol=%q dir=%q", st.CurrentVolume, st.CurrentDir)
	}
	if got := loadCurrentVolume(); got != "" {
		t.Fatalf("本地卷应持久化为空：%q", got)
	}
	if got := st.Location(); got != "/other" {
		t.Fatalf("Location()=%q", got)
	}
	// `cd /` = 本地卷根。
	runCdAndCapture(t, st, "/")
	if st.CurrentVolume != "" || st.CurrentDir != "" || st.Location() != "/" {
		t.Fatalf("cd / => vol=%q dir=%q loc=%q", st.CurrentVolume, st.CurrentDir, st.Location())
	}
}

func TestPwd_ShowsVolumeQualifiedPath(t *testing.T) {
	_ = setCacheDirForTest(t)
	var out strings.Builder
	st := &state.State{CurrentVolume: "vault", CurrentDir: "docs"}
	cmd := NewCmdPwd(st, cli.IOStreams{Out: &out, ErrOut: io.Discard})
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("pwd: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "vault:/docs" {
		t.Fatalf("pwd 输出=%q want vault:/docs", got)
	}
}
