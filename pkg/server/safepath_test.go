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
	"testing"
)

// TestDownload_RejectsInternalDirPrefix（审查 #4 收敛 + I-1 多租户跨目录读取）：
// 普通下载/stat（无 kind）访问服务端内部目录首段必须被拒——否则任何持有有效 AK 的
// 租户可经 GET /download?filename=.__cloud__/<taskID>/<file> 跨租户读取他人云端文件。
// 迁移到 Tenant API 后拦截点收敛到 Tenant.UserRel：逐段 ValidSegmentName 拒绝 .__
// 内部前缀（首段或深层）；功能桶名作为用户路径首段合法（user/ 桶内物理隔离），
// 不在此列（见 TestDownload_FeatureBucketFirstSegment）。
func TestDownload_RejectsInternalDirPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	h := newAssemblyTestHandlers(t, tmpDir)

	// 制造一个内部目录跨租户文件（模拟他人云端下载产物）——即使文件存在，普通下载也应拒绝
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
		_, err := h.resolveDownloadPath(req)
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
	if dp, err := h.resolveDownloadPath(req); err != nil || dp == nil {
		t.Errorf("普通下载应允许: %v", err)
	}
	// 深层含 .__ 的普通文件仍允许
	req2 := httptest.NewRequest("GET", "/download?filename=dir/foo.__bar.txt", nil)
	req2 = req2.WithContext(withActor(req2.Context(), ""))
	if dp, err := h.resolveDownloadPath(req2); err != nil || dp == nil {
		t.Errorf("深层 .__ 普通文件应允许: %v", err)
	}
}
