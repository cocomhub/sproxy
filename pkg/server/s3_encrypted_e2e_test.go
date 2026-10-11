// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_encrypted_e2e_test.go 钉住评审 P0 修复的**真实 S3 路径**回归：加密卷经 S3 网关 PUT 必须
// 密文落盘、GET 必须返回明文（此前 raw Open/OpenFile ⇒ 读密文/写明文，且改回后无测试会红）。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestS3PutGet_EncryptedVolumeRoundTrip 加密卷：PUT 落密文 → GET 回明文（真实 SigV4 请求）。
// 变异：s3PutObject 改回 root.OpenFile / s3GetObject 改回 root.Open → 相应断言红。
func TestS3PutGet_EncryptedVolumeRoundTrip(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "enc.key")
	if werr := os.WriteFile(keyFile, bytes.Repeat([]byte{7}, 32), 0o600); werr != nil {
		t.Fatal(werr)
	}
	url, cfgPtr, _ := newTestServerCreds(t, func(c *Config) {
		c.Volumes[0].Extra = map[string]any{"encrypt": true, "encrypt_key_file": keyFile}
	})
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	plain := []byte("s3-encrypted-plaintext-payload")
	const key = "dir/enc.bin"
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/"+key, host, plain, now)
	req, _ := http.NewRequest(http.MethodPut, url+"/s3/"+key, bytes.NewReader(plain))
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(plain)))
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("put = %d", resp.StatusCode)
	}

	// ① 磁盘上必须是**密文**（不含明文片段）。
	storageRoot := cfgPtr.Load().StorageRoot
	var raw []byte
	_ = filepath.WalkDir(storageRoot, func(p string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(filepath.ToSlash(p), "/user/"+key) {
			raw, _ = os.ReadFile(p)
		}
		return nil
	})
	if len(raw) == 0 {
		t.Fatal("PUT 后应能在卷内找到对象文件")
	}
	if bytes.Contains(raw, []byte("s3-encrypted-plaintext")) {
		t.Fatal("加密卷 S3 PUT 必须透明加密落盘（明文落盘 = at-rest 被绕过）")
	}

	// ② GET 必须回**明文**（用户可见语义）。
	authGet := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodGet, "/s3/"+key, host, nil, now)
	greq, _ := http.NewRequest(http.MethodGet, url+"/s3/"+key, nil)
	greq.Header.Set("Authorization", authGet)
	greq.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	greq.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(nil)))
	gresp, gerr := cl.Do(greq)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	got, _ := io.ReadAll(gresp.Body)
	_ = gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d", gresp.StatusCode)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("加密卷 S3 GET 应返回明文, got %q", got)
	}
}
