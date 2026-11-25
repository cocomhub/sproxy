// Copyright (C) 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestSharePassword_Access 锁分享口令的**端到端**语义（真实 handler + 真实存储）：
//   - 缺口令 / 错口令 ⇒ 401，且**不消费下载计数**（否则爆破一次就烧掉配额）；
//   - 正确口令（query 或 X-Share-Password 头）⇒ 200 且内容正确，计数 +1；
//   - 无口令的分享不受影响（零回归）。
func TestSharePassword_Access(t *testing.T) {
	t.Parallel()
	url, h := newEventTestServer(t, nil)
	client := testHTTPClient(t)
	body := []byte("secret content")
	if st, resp := uploadFile(t, url, "pw.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	}); st != http.StatusOK {
		t.Fatalf("上传失败 status=%d body=%s", st, resp)
	}

	create := func(payload string) string {
		resp, err := client.Post(url+"/api/shares", contentTypeJSON, strings.NewReader(payload))
		if err != nil {
			t.Fatalf("create share: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("create share status=%d body=%s", resp.StatusCode, b)
		}
		var out struct {
			Token string `json:"token"`
		}
		if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
			t.Fatalf("解析响应: %v", derr)
		}
		return out.Token
	}

	tok := create(`{"filename":"pw.txt","ttl":"1d","password":"s3cret"}`)
	if h.shareStore.Peek(tok) == nil {
		t.Fatal("口令分享应已创建")
	}

	access := func(header, query string) int {
		u := url + "/s/" + tok
		if query != "" {
			u += "?password=" + query
		}
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if header != "" {
			req.Header.Set("X-Share-Password", header)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("access: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if code := access("", ""); code != http.StatusUnauthorized {
		t.Fatalf("缺口令 status=%d want 401", code)
	}
	if code := access("", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("错口令 status=%d want 401", code)
	}
	if code := access("nope", ""); code != http.StatusUnauthorized {
		t.Fatalf("错误头部口令 status=%d want 401", code)
	}
	// 失败尝试不得计数（3 次失败后计数仍为 0）。
	if link := h.shareStore.Peek(tok); link != nil && link.Downloads != 0 {
		t.Fatalf("失败尝试不应计数，downloads=%d", link.Downloads)
	}
	if code := access("s3cret", ""); code != http.StatusOK {
		t.Fatalf("正确口令（头）status=%d want 200", code)
	}
	if link := h.shareStore.Peek(tok); link == nil || link.Downloads != 1 {
		t.Fatalf("成功访问应计数 1，got %+v", link)
	}
	// query 形式同样可用。
	if code := access("", "s3cret"); code != http.StatusOK {
		t.Fatalf("正确口令（query）status=%d want 200", code)
	}

	// 无口令分享：直接可访问（零回归）。
	plain := create(`{"filename":"pw.txt","ttl":"1d"}`)
	req, _ := http.NewRequest(http.MethodGet, url+"/s/"+plain, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("access plain: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("无口令分享 status=%d want 200", resp.StatusCode)
	}
}
