// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// assemblyShareTest 手工装配带持久化的分享服务（复用 RegisterRoutes），返回 URL 与 Handlers。
// storageRoot 由调用方固定（多实例共享同一根目录以模拟重启恢复）。
func assemblyShareTest(t *testing.T, storageRoot string) (string, *Handlers) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = storageRoot
	cfg.LogLevel = "error"
	cfg.Addr = "127.0.0.1:0"

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test-version",
		BuildAt:               "test-buildat",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close() })
	return ts.URL, h
}

// TestShare_Persist_AssemblyRestartRestores 验证**装配级**持久化：真实 RegisterRoutes
// 装配创建分享后落盘；重建装配（模拟重启）从同一 storage_root 恢复，分享 token 仍可访问。
func TestShare_Persist_AssemblyRestartRestores(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := []byte("persisted share content")

	// 第一次装配：上传 + 创建分享，然后关服（Handlers.Close）。
	url, h1 := assemblyShareTest(t, root)
	uploadFile(t, url, "p.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})
	reqBody := `{"filename":"p.txt","ttl":"1h"}`
	resp, err := http.Post(url+"/api/share", contentTypeJSON, strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建分享应 200, got %d", resp.StatusCode)
	}
	var shareResp map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&shareResp); err != nil {
		t.Fatal(err)
	}
	token := shareResp["token"].(string)
	if closeErr := h1.Close(); closeErr != nil {
		t.Fatalf("停服: %v", closeErr)
	}

	// 第二次装配（同一根目录）：分享 token 应恢复并可访问（内容一致）。
	url2, h2 := assemblyShareTest(t, root)
	defer func() { _ = h2.Close() }()
	resp2, err := http.Get(url2 + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("重启后分享应可访问, got %d", resp2.StatusCode)
	}
	data, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(body) {
		t.Fatalf("重启后分享内容不一致: got %q want %q", data, body)
	}
}
