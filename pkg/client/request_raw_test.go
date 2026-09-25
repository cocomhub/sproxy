// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestRaw_ExportedDoRequest 验证 RequestRaw（导出版 doRequest）直接可用：
// 携带自定义头直连 /raw 端点并返回原始响应（MCP 工具层复用签名/隧道管线的接缝）。
func TestRequestRaw_ExportedDoRequest(t *testing.T) {
	t.Parallel()

	var gotMethod, gotHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/raw" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotMethod = r.Method
		gotHeader = r.Header.Get("X-Test")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("raw-ok"))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL, WithAccessKey("ak-test", "sk-test"), WithAccessKeyID("sk-1"))
	headers := make(http.Header)
	headers.Set("X-Test", "abc")
	resp, err := c.RequestRaw(t.Context(), http.MethodGet, "/raw", nil, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "raw-ok" {
		t.Errorf("body = %q, want raw-ok", body)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotHeader != "abc" {
		t.Errorf("X-Test = %q, want abc", gotHeader)
	}
}

// TestRequestRaw_SendsBody 验证 RequestRaw 可发送请求体（write_file 直传 multipart 的接缝）。
func TestRequestRaw_SendsBody(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "hello" {
			http.Error(w, "unexpected body", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL, WithAccessKey("ak-test", "sk-test"), WithAccessKeyID("sk-1"))
	resp, err := c.RequestRaw(t.Context(), http.MethodPost, "/echo", strings.NewReader("hello"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestRequestRaw_ServerError 验证非 2xx 响应透传（错误处理由调用方按状态码判定，
// 与 doRequest 行为一致）。
func TestRequestRaw_ServerError(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"success":false,"message":"boom"}`, http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL, WithAccessKey("ak-test", "sk-test"), WithAccessKeyID("sk-1"))
	resp, err := c.RequestRaw(t.Context(), http.MethodGet, "/missing", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
