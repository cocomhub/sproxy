// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

// TestDownload_RejectsInternalDirPrefix（审查 #4 收敛 + I-1 多租户跨目录读取）：
// 普通下载/stat（无 kind）访问服务端内部目录首段必须被拒——否则任何持有有效 AK 的
// 租户可经 GET /download?filename=.__cloud__/<taskID>/<file> 跨租户读取他人云端文件。
// ValidateFilePath 全局放行 .__（避免破坏 sync push），拦截点收敛到 resolveDownloadPath。
func TestDownload_RejectsInternalDirPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{UploadsDir: tmpDir})
	h := &Handlers{cfgPtr: &cfg, logger: testLogger()}

	// 制造一个内部目录跨租户文件（模拟他人云端下载产物）
	internalDir := filepath.Join(tmpDir, ".__cloud__", "task123")
	if err := os.MkdirAll(internalDir, 0755); err != nil {
		t.Fatal(err)
	}
	secret := "cross-tenant-secret"
	if err := os.WriteFile(filepath.Join(internalDir, "file.zip"), []byte(secret), 0644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		".__cloud__/task123/file.zip",
		".__downloads__/x.txt",
		".__versions__/a/b.txt",
		".__chunked__/s/00000.chunk",
		".__cloud_archives__/x.tar.gz",
		".__sync__/y.json",
	} {
		req := httptest.NewRequest("GET", "/download?filename="+url.QueryEscape(name), nil)
		req = req.WithContext(withActor(req.Context(), "ak-tenant-a"))
		_, _, err := h.resolveDownloadPath(req)
		if err == nil {
			t.Errorf("下载内部目录 %q 应被拒绝", name)
			continue
		}
		var de *downloadPathError
		if !errors.As(err, &de) || de.status != http.StatusBadRequest {
			t.Errorf("内部目录 %q 应返回 400, got %v", name, err)
		}
	}

	// 普通下载不受影响
	normal := filepath.Join(tmpDir, "normal.txt")
	if err := os.WriteFile(normal, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/download?filename=normal.txt", nil)
	req = req.WithContext(withActor(req.Context(), ""))
	if _, _, err := h.resolveDownloadPath(req); err != nil {
		t.Errorf("普通下载应允许: %v", err)
	}
	// 深层含 .__ 的普通文件仍允许
	req2 := httptest.NewRequest("GET", "/download?filename=dir/foo.__bar.txt", nil)
	req2 = req2.WithContext(withActor(req2.Context(), ""))
	if _, _, err := h.resolveDownloadPath(req2); err != nil {
		t.Errorf("深层 .__ 普通文件应允许: %v", err)
	}
}
