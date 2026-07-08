// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateURLHost_RejectsPrivateIPs(t *testing.T) {
	tests := []struct {
		url     string
		wantErr bool
	}{
		// 环回地址：应拒绝
		{"http://127.0.0.1:8080/file", true},
		{"http://[::1]:8080/file", true},
		{"http://localhost:8080/file", true},
		// 私有地址：应拒绝
		{"http://10.0.0.1/file", true},
		{"http://172.16.0.1/file", true},
		{"http://192.168.1.1/file", true},
		// 链路本地地址：应拒绝
		{"http://169.254.1.1/file", true},
		{"http://[fe80::1]:8080/file", true},
		// 未指定地址：应拒绝
		{"http://0.0.0.0:8080/file", true},
		{"http://[::]:8080/file", true},
		// CGNAT 地址：应拒绝
		{"http://100.64.0.1/file", true},
		{"http://100.127.255.254/file", true},
		// 广播地址：应拒绝
		{"http://255.255.255.255/file", true},
		// 保留地址：应拒绝
		{"http://240.0.0.1/file", true},
		// 公网地址：应允许
		{"https://example.com/file.zip", false},
		{"http://93.184.216.34/file", false},
		// 非 HTTP scheme：应拒绝
		{"ftp://example.com/file", true},
		// 空 host：应拒绝
		{"http:///file", true},
		// 无效 URL：应拒绝
		{"not-a-url", true},
	}
	for _, tt := range tests {
		err := ValidateURLHost(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateURLHost(%q) error=%v, wantErr=%v", tt.url, err, tt.wantErr)
		}
	}
}

func TestValidateURLHost_IPv6Loopback(t *testing.T) {
	err := ValidateURLHost("http://[::1]:80/path")
	if err == nil {
		t.Error("expected error for IPv6 loopback")
	}
}

func TestValidateURLHost_IPv4MappedIPv6(t *testing.T) {
	err := ValidateURLHost("http://[::ffff:127.0.0.1]:80/path")
	if err == nil {
		t.Error("expected error for IPv4-mapped IPv6 loopback")
	}
}

func TestValidateURLHost_PublicHostname(t *testing.T) {
	err := ValidateURLHost("https://example.com/file.zip")
	if err != nil {
		t.Logf("ValidateURLHost for example.com: %v (may need network)", err)
	}
}

func TestValidateURLHost_InvalidURL(t *testing.T) {
	err := ValidateURLHost("")
	if err == nil {
		t.Error("expected error for empty URL")
	}
	err = ValidateURLHost("://")
	if err == nil {
		t.Error("expected error for malformed URL")
	}
}

func TestValidateURLHost_ReservedRanges(t *testing.T) {
	// 0.0.0.0/8 范围内的地址
	err := ValidateURLHost("http://0.1.2.3/file")
	if err == nil {
		t.Error("expected error for 0.0.0.0/8 address")
	}
	// 广播地址
	err = ValidateURLHost("http://255.255.255.255:80/file")
	if err == nil {
		t.Error("expected error for broadcast address")
	}
	// CGNAT
	err = ValidateURLHost("http://100.64.0.1:80/file")
	if err == nil {
		t.Error("expected error for CGNAT address")
	}
	// 198.18.0.0/15 benchmark
	err = ValidateURLHost("http://198.18.0.1:80/file")
	if err == nil {
		t.Error("expected error for benchmark address")
	}
	// 240.0.0.0/4 reserved
	err = ValidateURLHost("http://240.0.0.1:80/file")
	if err == nil {
		t.Error("expected error for 240.0.0.0/4 reserved address")
	}
}

func TestSafeCheckRedirect_TooManyRedirects(t *testing.T) {
	fn := safeCheckRedirect()
	req := httptest.NewRequest("GET", "http://example.com", nil)
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = httptest.NewRequest("GET", "http://example.com", nil)
	}
	err := fn(req, via)
	if err == nil {
		t.Error("expected error after 10 redirects")
	}
}

func TestSafeCheckRedirect_ExternalURLValidates(t *testing.T) {
	fn := safeCheckRedirect()
	req := httptest.NewRequest("GET", "http://127.0.0.1:8080/evil", nil)
	err := fn(req, nil)
	if err == nil {
		t.Error("expected error for loopback redirect")
	}
}

func TestSafeCheckRedirect_InternalPathOK(t *testing.T) {
	fn := safeCheckRedirect()
	req := httptest.NewRequest("GET", "/local/path", nil)
	err := fn(req, nil)
	if err != nil {
		t.Errorf("expected no error for internal path, got: %v", err)
	}
}
