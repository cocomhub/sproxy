// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFileClient_UploadStatus_OK 验证正常响应的全部字段（含服务端阶段进度）
// 被完整解析，且 upload_id / filename 经 query 正确传递（含 URL 转义）。
func TestFileClient_UploadStatus_OK(t *testing.T) {
	t.Parallel()
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":        true,
			"upload_id":      "sess-1",
			"received_count": 3,
			"total_chunks":   10,
			"missing_chunks": []int{4, 5},
			"completed":      false,
			"file_checksum":  "abc123",
			"filename":       "dir/a b.mp4",
			"message":        "会话3/10分块已接收",
			"stage":          "encrypt",
			"encrypt_done":   1024,
			"encrypt_total":  4096,
			"remote_done":    0,
			"remote_total":   0,
		})
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	c.logger = testLogger()

	st, err := c.UploadStatus(context.Background(), "sess-1", "dir/a b.mp4")
	if err != nil {
		t.Fatalf("UploadStatus: %v", err)
	}
	if st == nil {
		t.Fatal("UploadStatus 返回 nil 结果")
	}
	if !st.Success || st.UploadID != "sess-1" {
		t.Errorf("基础字段不符: %+v", st)
	}
	if st.ReceivedCount != 3 || st.TotalChunks != 10 || st.Completed {
		t.Errorf("计数/完成标志不符: %+v", st)
	}
	if len(st.MissingChunks) != 2 || st.MissingChunks[0] != 4 || st.MissingChunks[1] != 5 {
		t.Errorf("MissingChunks 不符: %v", st.MissingChunks)
	}
	if st.FileChecksum != "abc123" || st.Filename != "dir/a b.mp4" {
		t.Errorf("文件字段不符: %+v", st)
	}
	// 服务端阶段进度（T4）必须原样透出，供进度页展示「加密/远端卷上传」两阶段。
	if st.Stage != "encrypt" || st.EncryptDone != 1024 || st.EncryptTotal != 4096 {
		t.Errorf("加密阶段字段不符: %+v", st)
	}
	if st.RemoteDone != 0 || st.RemoteTotal != 0 {
		t.Errorf("远端阶段字段不符: %+v", st)
	}
	if !strings.Contains(gotQuery, "upload_id=sess-1") {
		t.Errorf("query 缺少 upload_id: %q", gotQuery)
	}
	// filename 必须转义后传递，服务端解出原文。
	if !strings.Contains(gotQuery, "a+b.mp4") && !strings.Contains(gotQuery, "a%20b.mp4") {
		t.Errorf("query 中 filename 未按预期转义: %q", gotQuery)
	}
}

// TestFileClient_UploadStatus_NotFound 验证「未找到会话」（HTTP 404）返回明确错误
// 且不静默返回零值结果。
func TestFileClient_UploadStatus_NotFound(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"message": "未找到文件或上传会话",
		})
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	c.logger = testLogger()

	st, err := c.UploadStatus(t.Context(), "missing", "")
	if err == nil {
		t.Fatalf("404 应返回错误, got result=%+v", st)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 应为 ErrNotFound, got %v", err)
	}
	if st != nil {
		t.Fatalf("出错时结果应为 nil, got %+v", st)
	}
}

// TestFileClient_UploadStatus_StageAbsent 验证旧服务端不返回阶段字段时零值不 panic。
func TestFileClient_UploadStatus_StageAbsent(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"upload_id":"s1","total_chunks":2}`))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	c.logger = testLogger()

	st, err := c.UploadStatus(t.Context(), "s1", "")
	if err != nil {
		t.Fatalf("UploadStatus: %v", err)
	}
	if st.Stage != "" || st.EncryptDone != 0 || st.EncryptTotal != 0 || st.RemoteDone != 0 || st.RemoteTotal != 0 {
		t.Errorf("缺省阶段字段应为零值: %+v", st)
	}
}

// TestFileClient_UploadStatus_OversizedBody 验证超过 1MiB 上限的响应体被截断后
// 解析失败（返回错误而非 panic、更不返回半成品）。
func TestFileClient_UploadStatus_OversizedBody(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"`))
		_, _ = w.Write([]byte(strings.Repeat("A", 2<<20)))
		_, _ = w.Write([]byte(`"}`))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	c.logger = testLogger()

	st, err := c.UploadStatus(t.Context(), "", "big")
	if err == nil {
		t.Fatalf("超限响应应解析失败, got %+v", st)
	}
}
