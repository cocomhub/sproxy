// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxy_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestE2E_QuotaCap507 验证（审查 D）真实二进制下 owner_quotas 写路径封顶：
// startSPROXY 配置 owner_quotas（owner=e2eTestAK，上限 100B）→ sclient/HTTP 上传 200 字节
// → 服务端 507（InsufficientStorage，JSON success=false）且磁盘无残留文件（TryReserve 在
// 原子写入前失败，不落盘、不泄漏预留）。
func TestE2E_QuotaCap507(t *testing.T) {
	extraYAML := "owner_quotas:\n  " + e2eTestAK + ": 100\n"
	baseURL, uploadsDir, cleanup := startSPROXYImpl(t, extraYAML)
	defer cleanup()

	content := make([]byte, 200) // 200 > 100 owner 上限
	for i := range content {
		content[i] = byte(i % 251)
	}
	status, body := uploadFile(t, baseURL, "over-quota.bin", content, map[string]string{
		"X-File-Checksum": sha256hex(content),
	})
	if status != http.StatusInsufficientStorage {
		t.Fatalf("上传超 owner_quotas 应 507, got %d: %s", status, body)
	}
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("507 响应解析失败: %v body=%s", err, body)
	}
	if resp.Success {
		t.Fatalf("507 响应 success 应为 false: %s", body)
	}

	// 磁盘无残留：<uploads>/<owner>/user/ 下应无文件（可存在空 user 目录）。
	owner := e2eTestAK
	userDir := filepath.Join(uploadsDir, owner, "user")
	entries, err := os.ReadDir(userDir)
	if err != nil {
		if os.IsNotExist(err) {
			return // user 目录未创建即无残留
		}
		t.Fatalf("读取 %s 失败: %v", userDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			t.Fatalf("507 后磁盘应无残留文件, got %s", e.Name())
		}
	}
}
