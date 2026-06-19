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
	result := h.safePath("file.txt")
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
	result := h.safePath("../etc/passwd")
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
	result := h.safePath("dir/file.txt")
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
	result := h.safePath("")
	if result != "" {
		t.Fatalf("expected empty string for empty remotePath, got %q", result)
	}
}
