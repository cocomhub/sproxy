// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// archive_cipher_test.go 验证加密归档端点（roadmap P2 加密归档插件化）：
//  1. POST /api/archive encrypt=true → 响应 .tar.gz.aes（密文）。
//  2. 配置 key_file 后解密 → tar.gz → tar 解出原文件内容。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestArchive_Encrypted 加密归档 → 解密 → tar 解出原文件。
func TestArchive_Encrypted(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "archive.key")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	url, _, _ := newTestServerCreds(t, func(c *Config) {
		c.Archive.KeyFile = keyFile
	})
	cl := &http.Client{Transport: netutil.IsolatedTransport()}

	// 上传文件。
	body := []byte("encrypted archive content")
	st := uploadFileSigned(t, url, "secret.txt", body)
	if st != 200 {
		t.Fatalf("upload = %d", st)
	}

	// 加密归档。
	req, _ := http.NewRequest(http.MethodPost, url+"/api/archive", strings.NewReader(`{"files":["secret.txt"],"encrypt":true}`))
	signBodyRequestEntry(req, testAccessKey, testEntryID(testAccessKey), testAccessSecret, []byte(`{"files":["secret.txt"],"encrypt":true}`))
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	enc, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("archive = %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), ".tar.gz.aes") {
		t.Fatalf("Content-Disposition = %q, want .tar.gz.aes", resp.Header.Get("Content-Disposition"))
	}
	if bytes.Contains(enc, []byte("encrypted archive content")) {
		t.Fatal("密文不应含明文特征")
	}

	// 解密 → tar.gz → tar 解出。
	dr, err := files.NewCipherReader("aes-256-gcm", key, bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("NewCipherReader: %v", err)
	}
	gzr, err := gzip.NewReader(dr)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar.Next: %v", err)
	}
	if hdr.Name != "secret.txt" {
		t.Fatalf("tar 内文件名 = %q", hdr.Name)
	}
	got, _ := io.ReadAll(tr)
	if string(got) != "encrypted archive content" {
		t.Fatalf("解出内容 = %q", got)
	}
}
