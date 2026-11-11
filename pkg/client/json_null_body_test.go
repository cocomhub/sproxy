// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDoJSON_NullBodyIsError 锁定：**200 + JSON `null` 必须报错**。
//
// 回归背景（2026-11-11）：`json.Decoder.Decode` 对字面量 `null` 返回 nil error 且对
// struct 目标是 no-op ⇒ `respBody` 保持零值 ⇒ 调用方看到「成功但字段全空」。
// 实测导致 sdserver 的 `/api/resolve` 全量零值（1619 失败 / 31 成功），
// 真实故障被掩盖成下游的「分享内无合格视频」。
func TestDoJSON_NullBodyIsError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"json_null", "null"},
		{"empty", ""},
		{"whitespace", "  \n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewFileClient(srv.URL, WithInsecureTLS())
			var out ResolveInfo
			err := c.DoJSON(context.Background(), http.MethodGet, "/api/resolve", nil, &out)
			if err == nil {
				t.Fatalf("200 + %q 应报错（否则零值会被当成功），got nil err", tc.body)
			}
			if !strings.Contains(err.Error(), "null") && !strings.Contains(err.Error(), "空") {
				t.Errorf("错误信息应点明空体/null，got %v", err)
			}
		})
	}
}

// TestDoJSON_ValidObjectStillDecodes 保证修复没有误伤正常路径。
func TestDoJSON_ValidObjectStillDecodes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://example.com/s/abc","kind":"pikpak_share","files":[{"name":"a.mp4","size":1}]}`))
	}))
	defer srv.Close()
	c := NewFileClient(srv.URL, WithInsecureTLS())
	var out ResolveInfo
	if err := c.DoJSON(context.Background(), http.MethodGet, "/api/resolve", nil, &out); err != nil {
		t.Fatalf("正常 JSON 对象应解码成功: %v", err)
	}
	if out.Kind != "pikpak_share" || len(out.Files) != 1 {
		t.Fatalf("解码结果不符: kind=%q files=%d", out.Kind, len(out.Files))
	}
}
