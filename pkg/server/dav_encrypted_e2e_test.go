// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// dav_encrypted_e2e_test.go 钉住评审 P0 修复的**真实 WebDAV 路径**回归：加密卷经 DAV PUT 必须
// 密文落盘、GET 必须返回明文（此前 `sync.NewLocalFS(userAbs)` 纯 os I/O：读密文、写明文，且
// 同时绕过封装封印——改回后无测试会红）。

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestWebDAV_EncryptedVolumeRoundTrip 加密卷 DAV：PUT 落密文 → GET 回明文。
// 变异：dav.go 的 FS 改回 `sync.NewLocalFS(userAbs)` → 两条断言红。
func TestWebDAV_EncryptedVolumeRoundTrip(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "enc.key")
	if werr := os.WriteFile(keyFile, bytes.Repeat([]byte{11}, 32), 0o600); werr != nil {
		t.Fatal(werr)
	}
	url, cfgPtr, _ := newTestServerCreds(t, func(cfg *Config) {
		cfg.WebDAV.Enabled = true
		cfg.Volumes[0].Extra = map[string]any{"encrypt": true, "encrypt_key_file": keyFile}
	})
	cl := &http.Client{Transport: netutil.IsolatedTransport()}

	const plain = "dav-encrypted-plaintext-payload"
	resp := webDAVRequest(t, cl, url, http.MethodPut, "/dav/enc/a.bin", plain)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("DAV PUT = %d", resp.StatusCode)
	}

	// ① 磁盘上必须是密文。
	storageRoot := cfgPtr.Load().StorageRoot
	var raw []byte
	_ = filepath.WalkDir(storageRoot, func(p string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(filepath.ToSlash(p), "/user/enc/a.bin") {
			raw, _ = os.ReadFile(p)
		}
		return nil
	})
	if len(raw) == 0 {
		t.Fatal("DAV PUT 后应能在卷内找到文件")
	}
	if bytes.Contains(raw, []byte("dav-encrypted-plaintext")) {
		t.Fatal("加密卷 DAV PUT 必须透明加密落盘（明文落盘 = at-rest 被绕过）")
	}

	// ② GET 必须回明文。
	gresp := webDAVRequest(t, cl, url, http.MethodGet, "/dav/enc/a.bin", "")
	got, _ := io.ReadAll(gresp.Body)
	_ = gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("DAV GET = %d", gresp.StatusCode)
	}
	if string(got) != plain {
		t.Fatalf("加密卷 DAV GET 应返回明文, got %q", got)
	}
}
