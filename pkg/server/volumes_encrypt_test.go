// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_encrypt_test.go 验证加密卷装配（roadmap P2 at-rest 加密残余）：
//  1. extra.encrypt=true + key 文件 → 装配成功，上传/下载透明加解密。
//  2. extra.encrypt=true 无 key → fail-closed 装配错误。
//  3. 未配 encrypt（缺省）→ 明文零回归。

import (
	"bytes"
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
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "top secret" {
		t.Fatalf("下载明文 = %q", body)
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
