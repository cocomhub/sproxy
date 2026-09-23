// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// dav_config_test.go 验证 WebDAV 服务端配置（roadmap P1 WebDAV 残余）：
//  1. webdav.enabled=false（默认）→ /dav/ 404（零回归）。
//  2. webdav.enabled=true → /dav/ PROPFIND 200。

import (
	"net/http"
	"testing"
)

// TestDAVDisabled_404 默认关 → /dav/ 404。
func TestDAVDisabled_404(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, nil)
	defer cleanup()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/dav/", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("webdav 默认关应 404, got %d", resp.StatusCode)
	}
}

// TestDAVEnabled_Propfind 开启 → PROPFIND 200。
func TestDAVEnabled_Propfind(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, func(cfg *Config) {
		cfg.WebDAV.Enabled = true
	})
	defer cleanup()
	req, _ := http.NewRequest("PROPFIND", baseURL+"/dav/", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND 应 200/207, got %d", resp.StatusCode)
	}
}
