// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// share_encrypted_e2e_test.go 钉住评审 P0 修复的**真实分享路径**回归：加密卷文件的分享链接
// 必须返回**明文**（此前 share.go 用 raw `root.Open` ⇒ 下发密文，静默损坏）。改回 raw 后无测试
// 会红——本用例关闭该缺口。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestShare_EncryptedVolumeReturnsPlaintext 加密卷：上传 → 创建分享 → GET /s/<token> 回明文。
// 变异：share.go 改回 `root.Open(link.Rel)` → 明文断言红（会拿到密文）。
func TestShare_EncryptedVolumeReturnsPlaintext(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "enc.key")
	if werr := os.WriteFile(keyFile, bytes.Repeat([]byte{13}, 32), 0o600); werr != nil {
		t.Fatal(werr)
	}
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Volumes[0].Extra = map[string]any{"encrypt": true, "encrypt_key_file": keyFile}
	})
	cl := &http.Client{Transport: netutil.IsolatedTransport()}

	plain := []byte("share-encrypted-plaintext-payload")
	if _, body := uploadFile(t, url, "shared-enc.txt", plain, map[string]string{
		"X-File-Checksum": sha256hex(plain),
	}); len(body) == 0 {
		t.Fatal("上传响应为空")
	}

	// 创建分享链接。
	createResp, cerr := cl.Post(url+"/api/share", contentTypeJSON, strings.NewReader(`{"filename":"shared-enc.txt","ttl":"1h"}`))
	if cerr != nil {
		t.Fatalf("POST /api/share: %v", cerr)
	}
	raw, _ := io.ReadAll(createResp.Body)
	_ = createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		t.Fatalf("创建分享 = %d body=%s", createResp.StatusCode, raw)
	}
	var sr struct {
		Token string `json:"token"`
	}
	if uerr := json.Unmarshal(raw, &sr); uerr != nil || sr.Token == "" {
		t.Fatalf("分享响应无 token: %s", raw)
	}

	// 匿名访问分享：必须是**明文**。
	sresp, serr := cl.Get(url + "/s/" + sr.Token)
	if serr != nil {
		t.Fatalf("GET /s/: %v", serr)
	}
	got, _ := io.ReadAll(sresp.Body)
	_ = sresp.Body.Close()
	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("GET /s/ = %d", sresp.StatusCode)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("加密卷分享应返回明文, got %d bytes %q", len(got), truncateForLog(got))
	}
	// 长度头应为**明文**长度（否则客户端 unexpected EOF）。
	if cl := sresp.Header.Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(plain)) {
		t.Fatalf("Content-Length 应为明文长度 %d, got %s", len(plain), cl)
	}
}

// truncateForLog 截断日志内容（防测试输出泄漏/爆量）。
func truncateForLog(b []byte) string {
	if len(b) > 64 {
		return string(b[:64]) + "…"
	}
	return string(b)
}
