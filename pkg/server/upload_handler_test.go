// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestResolveFilePath_PathTraversal(t *testing.T) {
	t.Parallel()

	// 先直接测试 ValidateFilePath 的行为
	if _, err := ValidateFilePath("../etc/passwd"); err != nil {
		t.Logf("ValidateFilePath('../etc/passwd') = error: %v", err)
	} else {
		t.Log("ValidateFilePath('../etc/passwd') = success (unexpected)")
	}

	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("test content")
	// 使用 X-File-Path header 显式传递路径穿越文件名
	status, respBody := uploadFile(t, url, "safe.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
		"X-File-Path":     "../etc/passwd",
	})
	if status != http.StatusBadRequest {
		t.Errorf("expected 400 for path traversal, got %d: %s", status, respBody)
	}
	var resp UploadResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Success {
		t.Error("expected success=false for invalid path")
	}
}

func TestResolveFilePath_AbsolutePath(t *testing.T) {
	t.Parallel()

	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("test content")
	status, respBody := uploadFile(t, url, "safe.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
		"X-File-Path":     "/etc/passwd",
	})
	if status != http.StatusBadRequest {
		t.Errorf("expected 400 for absolute path, got %d: %s", status, respBody)
	}
}

// TestAtomicRename_SuccessPath 通过正常文件上传间接测试 atomicRename 的成功路径。
// upload -> writeFileAtomically -> atomicRename(src, dst)，覆盖快速路径（os.Rename 直接成功）。
func TestAtomicRename_SuccessPath(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("test atomic rename success")
	cs := sha256hex(body)
	status, respBody := uploadFile(t, url, "atomic-test.txt", body, map[string]string{
		"X-File-Checksum": cs,
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, respBody)
	}
}
