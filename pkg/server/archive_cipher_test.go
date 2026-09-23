// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// archive_cipher_test.go 验证归档加密算法选型（roadmap P2 残余：双层加密/选型）：
//  1. cipher 空 → 默认 aes-256-gcm（归档成功）。
//  2. cipher 未注册 → 请求失败（fail-closed）。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestArchiveCipher_DefaultAlgo 默认 aes-256-gcm。
func TestArchiveCipher_DefaultAlgo(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "aes.key")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	baseURL, _, cleanup := newTestServerCreds(t, func(cfg *Config) {
		cfg.Archive.KeyFile = keyFile
	})
	defer cleanup()
	if st := uploadFileSigned(t, baseURL, "a.txt", []byte("hello")); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	// 创建加密归档（默认 cipher）。
	body := `{"files":["a.txt"],"encrypt":true}`
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/archive", bytes.NewReader([]byte(body)))
	signBodyRequestEntry(req, testAccessKey, testEntryID(testAccessKey), testAccessSecret, []byte(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive encrypt = %d", resp.StatusCode)
	}
}

// TestArchiveCipher_UnknownAlgo 未注册算法 → fail-closed。
func TestArchiveCipher_UnknownAlgo(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "aes.key")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	baseURL, _, cleanup := newTestServerCreds(t, func(cfg *Config) {
		cfg.Archive.KeyFile = keyFile
	})
	defer cleanup()
	if st := uploadFileSigned(t, baseURL, "b.txt", []byte("x")); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	body := `{"files":["b.txt"],"encrypt":true,"cipher":"unknown-cipher"}`
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/archive", bytes.NewReader([]byte(body)))
	signBodyRequestEntry(req, testAccessKey, testEntryID(testAccessKey), testAccessSecret, []byte(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("未注册 cipher 应失败, got %d", resp.StatusCode)
	}
	// 解析响应（fail-closed 消息）。
	var sr struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sr)
	if sr.Success {
		t.Fatalf("未注册 cipher 应 success=false")
	}
}
