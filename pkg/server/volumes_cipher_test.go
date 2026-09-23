// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_cipher_test.go 验证加密卷算法选型（roadmap P2 加密归档插件化残余：
// volumes[].extra.cipher）：
//  1. extra.cipher="aes-256-gcm" → 装配成功（默认算法显式写）。
//  2. extra.cipher="unknown" → fail-closed 装配错误。

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestVolumesCipher_ExplicitDefault 显式 aes-256-gcm 装配成功。
func TestVolumesCipher_ExplicitDefault(t *testing.T) {
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
				"cipher":           "aes-256-gcm",
			},
		}}
	})
	defer cleanup()
	if st := uploadFileSigned(t, baseURL, "s.txt", []byte("x")); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	// 下载读回明文。
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/download?filename=s.txt", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download = %d", resp.StatusCode)
	}
}

// TestVolumesCipher_UnknownAlgo 未知算法 fail-closed（装配即拒绝）。
func TestVolumesCipher_UnknownAlgo(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "vol.key")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	volRoot := filepath.Join(t.TempDir(), "vol")
	_ = os.MkdirAll(volRoot, 0o755)
	// 直接调装配（assembleVolumes）验证 fail-closed，不依赖 HTTP 层。
	if _, err := assembleVolumesForTest(t, volRoot, keyFile, "unknown-cipher"); err == nil {
		t.Fatalf("未知 cipher 应装配失败")
	}
}

// assembleVolumesForTest 直接装配单加密卷（返回 registry.Set）。
func assembleVolumesForTest(t *testing.T, volRoot, keyFile, cipher string) (any, error) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(t.TempDir(), "root")
	cfg.Volumes = []VolumeConfig{{
		Name: "enc",
		Type: "local",
		Root: volRoot,
		Extra: map[string]any{
			"encrypt":          true,
			"encrypt_key_file": keyFile,
			"cipher":           cipher,
		},
	}}
	// assembleVolumes 是包内函数（本测试在 server 包内）。
	set, err := assembleVolumes(cfg, nil)
	return set, err
}
