// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// share_ro_test.go 验证分享权限细化（roadmap P2 分享权限细化）：
//  1. 创建分享 readonly 参数 → 响应带 readonly 标志。
//  2. 访问 /s/{token} 响应带 X-Share-ReadOnly 头（分享只读语义可见）。
//  3. readonly 分享仍可正常下载内容（只读 = 禁止写面，读不受影响）。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestShare_ReadOnlyFlag 创建 readonly 分享 → 响应标志 + 访问头。
func TestShare_ReadOnlyFlag(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}

	// 上传文件（multipart + 带凭据签名——复用 uploadFileSigned）。
	st := uploadFileSigned(t, url, "ro.txt", []byte("share readonly content"))
	if st != 200 {
		t.Fatalf("upload = %d", st)
	}

	// 创建 readonly 分享。
	req2, _ := http.NewRequest(http.MethodPost, url+"/api/share", strings.NewReader(`{"filename":"ro.txt","readonly":true}`))
	signBodyRequestEntry(req2, testAccessKey, testEntryID(testAccessKey), testAccessSecret, []byte(`{"filename":"ro.txt","readonly":true}`))
	resp2, serr := cl.Do(req2)
	if serr != nil {
		t.Fatalf("share: %v", serr)
	}
	var created struct {
		Success  bool   `json:"success"`
		Token    string `json:"token"`
		ReadOnly bool   `json:"readonly"`
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("share = %d body=%q", resp2.StatusCode, body2)
	}
	if err := json.Unmarshal(body2, &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !created.Success || created.Token == "" {
		t.Fatalf("创建失败: %+v", created)
	}
	if !created.ReadOnly {
		t.Fatalf("readonly 应 true, got %+v", created)
	}

	// 访问分享 → X-Share-ReadOnly 头。
	req3, _ := http.NewRequest(http.MethodGet, url+"/s/"+created.Token, nil)
	resp3, err := cl.Do(req3)
	if err != nil {
		t.Fatalf("access: %v", err)
	}
	body3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("access = %d", resp3.StatusCode)
	}
	if string(body3) != "share readonly content" {
		t.Fatalf("内容 = %q", body3)
	}
	if resp3.Header.Get("X-Share-ReadOnly") != "true" {
		t.Fatalf("X-Share-ReadOnly = %q, want true", resp3.Header.Get("X-Share-ReadOnly"))
	}
}
