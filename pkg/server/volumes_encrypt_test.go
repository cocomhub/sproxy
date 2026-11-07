// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_encrypt_test.go 验证加密卷装配（roadmap P2 at-rest 加密残余）：
//  1. extra.encrypt=true + key 文件 → 装配成功，上传/下载透明加解密。
//  2. extra.encrypt=true 无 key → fail-closed 装配错误。
//  3. 未配 encrypt（缺省）→ 明文零回归。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestVolumesEncrypt_RoundTrip 加密卷上传/下载透明加解密。
func TestVolumesEncrypt_RoundTrip(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "vol.key")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	volRoot := filepath.Join(t.TempDir(), "vol")
	_ = os.MkdirAll(volRoot, 0o755)
	baseURL, _, cleanup := newTestServerCreds(t, func(cfg *Config) {
		cfg.Volumes = []VolumeConfig{{
			Name: "enc",
			Type: "local",
			Root: volRoot,
			Extra: map[string]any{
				"encrypt":          true,
				"encrypt_key_file": keyFile,
			},
		}}
		cfg.Volumes[0].Name = "enc"
	})
	defer cleanup()

	if st := uploadFileSigned(t, baseURL, "sec.txt", []byte("top secret")); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	// 下载读回明文。
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/download?filename=sec.txt", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download = %d", resp.StatusCode)
	}
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		// 回归守卫（P0）：加密卷下载若被误接明文流校验，Content-Length=密文长而 body=明文，
		// 读取在这里以 unexpected EOF 报错。
		t.Fatalf("读取下载响应失败（加密卷不应接密文 meta 校验）: %v (body=%q, ContentLength=%d)", rerr, body, resp.ContentLength)
	}
	if string(body) != "top secret" {
		t.Fatalf("下载明文 = %q", body)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != int64(len(body)) {
		t.Fatalf("ContentLength=%d 与明文 body 长度 %d 不符", resp.ContentLength, len(body))
	}
	// 密文落盘（user 桶内文件非明文）。
	var raw []byte
	_ = filepath.WalkDir(volRoot, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(path) == "sec.txt" {
			raw, _ = os.ReadFile(path)
		}
		return nil
	})
	if raw == nil {
		t.Fatalf("未找到加密卷文件（walk %s）", volRoot)
	}
	if bytes.Contains(raw, []byte("top secret")) {
		t.Fatalf("密文不应含明文")
	}
	// 显式请求密文（?ciphertext=1）：返回存储原样字节（== 磁盘上的密文），ContentLength 正确。
	req2, _ := http.NewRequest(http.MethodGet, baseURL+"/download?filename=sec.txt&ciphertext=1", nil)
	signRequest(req2, testAccessKey, testAccessSecret)
	resp2, err2 := http.DefaultClient.Do(req2)
	if err2 != nil {
		t.Fatal(err2)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("ciphertext download = %d", resp2.StatusCode)
	}
	enc, eerr := io.ReadAll(resp2.Body)
	if eerr != nil {
		t.Fatalf("读取密文响应失败: %v", eerr)
	}
	if !bytes.Equal(enc, raw) {
		t.Fatalf("ciphertext=1 应返回磁盘原样密文（len got=%d want=%d）", len(enc), len(raw))
	}
	if resp2.ContentLength >= 0 && resp2.ContentLength != int64(len(enc)) {
		t.Fatalf("密文 ContentLength=%d body=%d", resp2.ContentLength, len(enc))
	}
}

// TestVolumesEncrypt_MissingKey 无 key → fail-closed。
func TestVolumesEncrypt_MissingKey(t *testing.T) {
	t.Parallel()
	volRoot := filepath.Join(t.TempDir(), "vol")
	_ = os.MkdirAll(volRoot, 0o755)
	cfg := Default()
	cfg.Volumes = []VolumeConfig{{
		Name: "enc", Type: "local", Root: volRoot,
		Extra: map[string]any{"encrypt": true},
	}}
	if _, err := assembleVolumes(cfg, nil); err == nil {
		t.Fatalf("缺 key 应装配失败")
	}
}

// TestVolumesEncrypt_ChunkedDownload 加密卷分块下载（审查批次12 P1 修复）：
// 修复前普通 Open 读密文 + Seek 失败——现 OpenDecrypted + Discard offset 读明文。
func TestVolumesEncrypt_ChunkedDownload(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "vol.key")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	volRoot := filepath.Join(t.TempDir(), "vol")
	_ = os.MkdirAll(volRoot, 0o755)
	baseURL, _, cleanup := newTestServerCreds(t, func(cfg *Config) {
		cfg.Volumes = []VolumeConfig{{
			Name: "enc",
			Type: "local",
			Root: volRoot,
			Extra: map[string]any{
				"encrypt":          true,
				"encrypt_key_file": keyFile,
			},
		}}
		cfg.Volumes[0].Name = "enc"
	})
	defer cleanup()

	payload := bytes.Repeat([]byte("enc-chunk-dl!"), 4096) // 64 KiB 多块
	if st := uploadFileSigned(t, baseURL, "big.bin", payload); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	// 分块下载重组（4 KiB 块）。
	var assembled []byte
	for offset := 0; offset < len(payload); offset += 4096 {
		length := min(4096, len(payload)-offset)
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/download/chunk?filename=big.bin&offset=%d&length=%d", baseURL, offset, length), nil)
		signRequest(req, testAccessKey, testAccessSecret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("chunk %d = %d body=%s", offset, resp.StatusCode, body[:min(len(body), 100)])
		}
		assembled = append(assembled, body...)
	}
	if !bytes.Equal(assembled, payload) {
		t.Fatalf("重组明文不匹配: got %d bytes want %d", len(assembled), len(payload))
	}
}

// TestVolumesEncrypt_ChunkedCiphertext 显式请求密文分块（?ciphertext=1）：offset/length
// 按密文坐标，返回存储原样字节。
func TestVolumesEncrypt_ChunkedCiphertext(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "vol.key")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	volRoot := filepath.Join(t.TempDir(), "vol")
	_ = os.MkdirAll(volRoot, 0o755)
	baseURL, _, cleanup := newTestServerCreds(t, func(cfg *Config) {
		cfg.Volumes = []VolumeConfig{{
			Name: "enc", Type: "local", Root: volRoot,
			Extra: map[string]any{"encrypt": true, "encrypt_key_file": keyFile},
		}}
		cfg.Volumes[0].Name = "enc"
	})
	defer cleanup()

	payload := bytes.Repeat([]byte("enc-chunk-ct!"), 1024)
	if st := uploadFileSigned(t, baseURL, "ct.bin", payload); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	var raw []byte
	_ = filepath.WalkDir(volRoot, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) == "ct.bin" {
			raw, _ = os.ReadFile(p)
		}
		return nil
	})
	if len(raw) == 0 {
		t.Fatal("未找到加密文件")
	}
	// 密文分块：offset=0..N，返回 raw 前缀。
	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/download/chunk?filename=ct.bin&offset=0&length=%d&ciphertext=1", baseURL, 1024), nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ciphertext chunk = %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, raw[:1024]) {
		t.Fatalf("ciphertext chunk 应与磁盘密文前缀一致: got %d bytes, want %d", len(got), 1024)
	}
	if bytes.Contains(got, []byte("enc-chunk-ct!")) {
		t.Fatal("密文分块不应含明文")
	}
}

// encTestSetup 建加密卷 + 上传 payload，返回 baseURL 与清理函数（第 5 轮对抗评审回归用）。
func encTestSetup(t *testing.T, name string, payload []byte) (string, func()) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "vol.key")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	volRoot := filepath.Join(t.TempDir(), "vol")
	_ = os.MkdirAll(volRoot, 0o755)
	baseURL, _, cleanup := newTestServerCreds(t, func(cfg *Config) {
		cfg.Volumes = []VolumeConfig{{
			Name: "enc", Type: "local", Root: volRoot,
			Extra: map[string]any{"encrypt": true, "encrypt_key_file": keyFile},
		}}
		cfg.Volumes[0].Name = "enc"
	})
	if st := uploadFileSigned(t, baseURL, name, payload); st != 200 {
		cleanup()
		t.Fatalf("upload = %d", st)
	}
	return baseURL, cleanup
}

// TestVolumesEncrypt_ChunkedDownloadDefaultLength P1 回归（第 5 轮对抗评审，实测复现）：
// 客户端**不传 length**（默认档 4MiB > 明文长）时，`Content-Length` 必须等于实际下发
// 明文长度。修复前 `chunkReadParams` 取 `root.Stat`（**密文**长）截断 length →
// `200 Content-Length=53300` 而实际仅 53248 字节 → 客户端 `io.ReadAll` 报 unexpected EOF。
func TestVolumesEncrypt_ChunkedDownloadDefaultLength(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("enc-chunk-dl!"), 4096) // 64 KiB 多块
	baseURL, cleanup := encTestSetup(t, "deflen.bin", payload)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, baseURL+"/download/chunk?filename=deflen.bin&offset=0", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("io.ReadAll = %v（Content-Length=%s 实际 %d）", rerr, resp.Header.Get("Content-Length"), len(body))
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk = %d", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprintf("%d", len(body)) {
		t.Fatalf("Content-Length=%s 与实际下发 %d 不符", cl, len(body))
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("重组明文不匹配 got=%d want=%d", len(body), len(payload))
	}
	if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)) {
		t.Fatalf("Content-Range=%q 应为明文坐标", cr)
	}
}

// TestVolumesEncrypt_DownloadRangeNonZero P1 回归：加密卷 `/download` 带非零起点 Range
// 必须 206 且下发正确区间（修复前 `encReadCloser.Seek(N, SeekStart)` 报错 → ServeContent 416）。
func TestVolumesEncrypt_DownloadRangeNonZero(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("enc-range-dl!"), 2048)
	baseURL, cleanup := encTestSetup(t, "range.bin", payload)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, baseURL+"/download?filename=range.bin", nil)
	req.Header.Set("Range", "bytes=100-199")
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range 请求应 206，得到 %d（body=%q）", resp.StatusCode, body[:min(len(body), 80)])
	}
	if !bytes.Equal(body, payload[100:200]) {
		t.Fatalf("Range 区间内容不符: got %d bytes", len(body))
	}
	if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 100-199/%d", len(payload)) {
		t.Fatalf("Content-Range=%q", cr)
	}
}

// TestVolumesEncrypt_StatReportsPlaintextSize P1 回归：`/api/files/stat` 的 X-File-Size
// 必须是明文长度（WebUI 据此推 totalChunks 并判完成）。
func TestVolumesEncrypt_StatReportsPlaintextSize(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("enc-stat!"), 1000)
	baseURL, cleanup := encTestSetup(t, "stat.bin", payload)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodHead, baseURL+"/api/files/stat?filename=stat.bin", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stat = %d", resp.StatusCode)
	}
	if sz := resp.Header.Get("X-File-Size"); sz != fmt.Sprintf("%d", len(payload)) {
		t.Fatalf("X-File-Size=%s 应为明文长度 %d", sz, len(payload))
	}
}
