// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxy_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
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

// TestE2E_UploadSubdir507 验证真实二进制 + 真实签名下 bucket_limits 子目录对直接上传封顶
// （分层配额绑定 1/2/3）：owner=200B + bucket_limits{user/videos/hd: 100B}——向子目录上传
// 60B 成功、再传 50B 触发 507（60+50>100 子目录层拦截，租户 200 仍足）。
func TestE2E_UploadSubdir507(t *testing.T) {
	extraYAML := "owner_quotas:\n  " + e2eTestAK + ": 200\n" +
		"bucket_limits:\n  user/videos/hd: 100\n"
	baseURL, _, cleanup := startSPROXYImpl(t, extraYAML)
	defer cleanup()

	// 子目录目标（X-File-Path 指定 user 桶内相对路径）。
	hdKeys := map[string]string{"X-File-Checksum": "", "X-File-Path": "videos/hd/a.mkv"}

	mkBody := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i % 251)
		}
		return b
	}
	bd1 := mkBody(60)
	hdKeys["X-File-Checksum"] = sha256hex(bd1)
	status, body := uploadFile(t, baseURL, "a.mkv", bd1, hdKeys)
	if status != http.StatusOK {
		t.Fatalf("子目录 60B 应 200, got %d: %s", status, body)
	}

	bd2 := mkBody(50)
	hdKeys["X-File-Checksum"] = sha256hex(bd2)
	hdKeys["X-File-Path"] = "videos/hd/b.mkv"
	status, body = uploadFile(t, baseURL, "b.mkv", bd2, hdKeys)
	if status != http.StatusInsufficientStorage {
		t.Fatalf("子目录累计 110>100 应 507, got %d: %s", status, body)
	}
}

// TestE2E_QuotaClientStorageFull 验证（残余项 E）客户端 507 → client.ErrStorageFull 协议
// 一致性：真实二进制 + 真实签名下用 FileClient 上传超 owner 配额，收到 client.ErrStorageFull
// 哨兵错误（errors.Is），而非通用错误。
func TestE2E_QuotaClientStorageFull(t *testing.T) {
	extraYAML := "owner_quotas:\n  " + e2eTestAK + ": 50\n"
	baseURL, _, cleanup := startSPROXYImpl(t, extraYAML)
	defer cleanup()

	fc := testFileClient(baseURL)
	content := bytes.Repeat([]byte("z"), 200) // 200 > 50
	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := fc.Upload(t.Context(), src, "big.bin")
	if err == nil {
		t.Fatal("上传超配额应报错")
	}
	if !errors.Is(err, client.ErrStorageFull) {
		t.Fatalf("507 应映射为 client.ErrStorageFull, got %v", err)
	}
}

// testFileClient 构造带 E2E AK/SK 的 FileClient（复用 TestE2E_CloudDownloadChain 形态）。
func testFileClient(baseURL string) *client.FileClient {
	return client.NewFileClient(baseURL,
		client.WithAccessKey(e2eTestAK, e2eTestSK))
}
