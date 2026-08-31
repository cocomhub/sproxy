// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestSafePath_NormalPath(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{UploadsDir: tmpDir})

	h := &Handlers{cfgPtr: &cfg}
	result := h.safePathForOwner("", "file.txt")
	expected := filepath.Join(tmpDir, "file.txt")
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestSafePath_PathTraversal(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{UploadsDir: tmpDir})

	h := &Handlers{cfgPtr: &cfg}
	result := h.safePathForOwner("", "../etc/passwd")
	if result != "" {
		t.Fatalf("expected empty string for path traversal, got %q", result)
	}
}

func TestSafePath_SubDirectory(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{UploadsDir: tmpDir})

	h := &Handlers{cfgPtr: &cfg}
	result := h.safePathForOwner("", "dir/file.txt")
	expected := filepath.Join(tmpDir, "dir/file.txt")
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestSafePath_EmptyString(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{UploadsDir: tmpDir})

	h := &Handlers{cfgPtr: &cfg}
	result := h.safePathForOwner("", "")
	if result != "" {
		t.Fatalf("expected empty string for empty remotePath, got %q", result)
	}
}

// TestSafePath_OwnerIsolation 验证多租户 owner 隔离：owner 非空时文件落到
// uploadsDir/<owner>/ 子目录；未认证（owner 空）直接用 uploadsDir。
func TestSafePath_OwnerIsolation(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{UploadsDir: tmpDir})

	h := &Handlers{cfgPtr: &cfg}
	owned := h.safePathForOwner("ak-tenant-a", "dir/file.txt")
	expected := filepath.Join(tmpDir, "ak-tenant-a", "dir", "file.txt")
	if owned != expected {
		t.Fatalf("expected owner path %q, got %q", expected, owned)
	}
	// 越界防护仍生效：owner 非空时 ../ 逃逸被拒绝
	if out := h.safePathForOwner("ak-tenant-a", "../etc/passwd"); out != "" {
		t.Fatalf("expected empty for traversal under owner, got %q", out)
	}
}
