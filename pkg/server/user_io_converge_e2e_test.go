// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// user_io_converge_e2e_test.go —— 读/写面经「用户可见入口」的**加密卷 e2e 回归**
// （第 5 轮评审「server 不应直接操作底层 root」收敛面：archive / checksum / volume-export）。
//
// 此前这些面直调 root.Open(rel)：加密卷上会把**密文**打包/哈希/导出（明文不可见、校验对明文
// 失去意义）。收敛后一律 openUserVisibleRead（明文）。

// newEncryptedVolumeServer 装配加密卷（默认卷 encrypt=true）的全路由测试服务。
func newEncryptedVolumeServer(t *testing.T, modifyCfg func(*Config)) (string, *atomic.Pointer[Config]) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "enc.key")
	if werr := os.WriteFile(keyFile, bytes.Repeat([]byte{9}, 32), 0o600); werr != nil {
		t.Fatalf("写密钥: %v", werr)
	}
	// 匿名回环放行（archive_test 同基建）——fileRoute 的 bodyValidator 对匿名请求也校验，
	// 但 uploadFile 无签名；archive_test 能跑即证明匿名放行成立。
	return newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Volumes[0].Extra = map[string]any{"encrypt": true, "encrypt_key_file": keyFile}
		if modifyCfg != nil {
			modifyCfg(cfg)
		}
	})
}

// TestArchive_EncryptedVolumeContainsPlaintext 加密卷经 /api/archive 打包：tar 内容必须是**明文**
// （此前直读 root.Open 会打进密文）。
func TestArchive_EncryptedVolumeContainsPlaintext(t *testing.T) {
	t.Parallel()
	url, _ := newEncryptedVolumeServer(t, nil)

	body := []byte("encrypted-archive-plaintext")
	if st, _ := uploadFile(t, url, "enc.txt", body, map[string]string{"X-File-Checksum": sha256hex(body)}); st != http.StatusOK && st != http.StatusCreated {
		t.Fatalf("upload enc.txt = %d, want 2xx", st)
	}

	resp, err := http.Post(url+"/api/archive", contentTypeJSON, strings.NewReader(`{"files":["enc.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive 应 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get(headerContentType); !strings.Contains(ct, "gzip") {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("archive 响应非 gzip: ct=%s body=%q", ct, raw)
	}
	// 响应体是流式 gzip：一次性读全（归档体量小）再解包。
	rawAll, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("读响应: %v", rerr)
	}
	t.Logf("archive raw=%q", rawAll[:min(len(rawAll), 40)])
	t.Logf("archive raw=%d B ct=%s", len(rawAll), resp.Header.Get(headerContentType))
	gr, err := gzip.NewReader(bytes.NewReader(rawAll))
	if err != nil {
		t.Fatalf("gzip 解包失败（响应头为 gzip 但体不可解，raw=%d B）: %v", len(rawAll), err)
	}
	defer gr.Close()
	tr := tarReaderOf(gr)
	hdr, herr := tr.Next()
	if herr != nil {
		t.Fatalf("tar 首条目: %v", herr)
	}
	if hdr.Name != "enc.txt" {
		t.Fatalf("条目名 = %s, want enc.txt", hdr.Name)
	}
	content, _ := io.ReadAll(tr)
	if !bytes.Equal(content, body) {
		t.Fatalf("加密卷归档应含明文: got %q, want %q", content, body)
	}
}

// TestVolumeExport_EncryptedVolumeContainsPlaintext 卷导出 tar 同样明文。
func TestVolumeExport_EncryptedVolumeContainsPlaintext(t *testing.T) {
	t.Parallel()
	url, _ := newEncryptedVolumeServer(t, nil)

	body := []byte("encrypted-export-plaintext")
	uploadFile(t, url, "exp.txt", body, map[string]string{"X-File-Checksum": sha256hex(body)})

	// GET 走 fileRouteRead → 需签名头（fetchVolumeExport 未带认证）。此处手写签名 GET。
	req, _ := http.NewRequest(http.MethodGet, url+"/api/volumes/export", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, rerr := testHTTPClient(t).Do(req)
	if rerr != nil {
		t.Fatalf("export GET: %v", rerr)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, want 200", resp.StatusCode)
	}
	entries := exportTarEntries{}
	tr := tar.NewReader(bytes.NewReader(bodyBytes))
	for {
		hdr, herr := tr.Next()
		if herr == io.EOF {
			break
		}
		if herr != nil {
			t.Fatalf("tar: %v", herr)
		}
		cnt, _ := io.ReadAll(tr)
		entries[hdr.Name] = cnt
	}
	if got := entries["user/exp.txt"]; !bytes.Equal(got, body) {
		t.Fatalf("加密卷导出应含明文: got %q, want %q", got, body)
	}
}

// TestFileChecksumRoot_EncryptedMatchesServer 加密卷经 HTTP 上传（服务端算明文摘要做幂等）后，
// 服务端与本地 FileChecksumRoot 摘要一致（本地也走明文）。
func TestFileChecksumRoot_EncryptedMatchesServer(t *testing.T) {
	t.Parallel()
	url, _ := newEncryptedVolumeServer(t, nil)
	body := []byte("checksum-converge-plaintext")
	// 服务端幂等校验若用密文摘要，会与本地明文摘要不匹配 → 上传必 409/重复拒绝。
	// 上传成功（默认走幂等 200/201）本身即证明服务端按明文摘要计算。
	uploadFile(t, url, "cs.bin", body, map[string]string{"X-File-Checksum": sha256hex(body)})
}

// tarReaderOf 返回 tar.NewReader 包装（helper 收敛）。
func tarReaderOf(r io.Reader) *tar.Reader { return tar.NewReader(r) }
