// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// tags_api_test.go 是文件标签系统（roadmap 11.10-④）的装配层 HTTP 测试：
//
//   - POST /api/tags（查询参数与 JSON body 批量形态）经认证签名驱动；
//   - GET /api/files/search?tag= 过滤；
//   - 无凭据（no-auth）环境：POST /api/tags 走 AllowInsecureLoopback 兜底放行。
//
// 全部走 httptest（127.0.0.1 回环）；client 用 testHTTPClient(t)（独立连接池）。

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
)

// TestTagsAPI_QueryAndBody 经完整路由（签名认证）验证打标 + 按标签搜索。
func TestTagsAPI_QueryAndBody(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutesCreds(t, nil)

	// 上传文件（签名）。
	body := []byte("tags api body")
	st := uploadFileSigned(t, url, "tagged.txt", body)
	if st != http.StatusOK {
		t.Fatalf("setup upload status = %d, want 200", st)
	}

	// 查询参数形态：POST /api/tags?filename=tagged.txt&tags=alpha,beta。
	req2, _ := http.NewRequest(http.MethodPost, url+"/api/tags?filename=tagged.txt&tags=alpha,beta", nil)
	signRequest(req2, testAccessKey, testAccessSecret)
	resp, err := testHTTPClient(t).Do(req2)
	if err != nil {
		t.Fatalf("tags query: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/tags (query) status = %d, want 200", resp.StatusCode)
	}
	var out UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Success {
		t.Fatalf("tags 响应应 success, got %+v", out)
	}

	// 按标签搜索（签名 GET）。
	st2, body2 := signedGet(t, url+"/api/files/search?tag=alpha", testAccessKey, testAccessSecret)
	if st2 != http.StatusOK {
		t.Fatalf("GET /api/files/search?tag=alpha status = %d, want 200 (body=%s)", st2, body2)
	}
	var list files.ListResponse
	if err := json.Unmarshal(body2, &list); err != nil {
		t.Fatalf("unmarshal search: %v (body=%s)", err, body2)
	}
	if list.Total != 1 || len(list.Files) != 1 || list.Files[0].Name != "tagged.txt" {
		t.Fatalf("tag 搜索应命中 tagged.txt, got %+v", list)
	}

	// JSON body 批量形态。
	st3, body3 := doSignedJSON(t, http.MethodPost, url+"/api/tags", testAccessKey, testAccessSecret, map[string]any{
		"files": []string{"tagged.txt"},
		"tags":  []string{"gamma"},
	})
	if st3 != http.StatusOK {
		t.Fatalf("POST /api/tags (body) status = %d, want 200 (body=%s)", st3, body3)
	}
	st4, body4 := signedGet(t, url+"/api/files/search?tag=gamma", testAccessKey, testAccessSecret)
	if st4 != http.StatusOK {
		t.Fatalf("GET search?tag=gamma status = %d, want 200 (body=%s)", st4, body4)
	}
	var list2 files.ListResponse
	if err := json.Unmarshal(body4, &list2); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body4)
	}
	if list2.Total != 1 || list2.Files[0].Name != "tagged.txt" {
		t.Fatalf("body 打标后 tag=gamma 应命中, got %+v", list2)
	}

	// 非法标签 → 400。
	st5, _ := doSignedJSON(t, http.MethodPost, url+"/api/tags?filename=tagged.txt&tags=bad%20tag", testAccessKey, testAccessSecret, nil)
	if st5 != http.StatusBadRequest {
		t.Fatalf("非法标签 status = %d, want 400", st5)
	}
}

// TestTagsAPI_NoAuthLoopback 无凭据 + AllowInsecureLoopback：POST /api/tags 兜底放行。
func TestTagsAPI_NoAuthLoopback(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("noauth")
	uploadFile(t, url, "n.txt", body, map[string]string{"X-File-Checksum": sha256hex(body)})

	req, _ := http.NewRequest(http.MethodPost, url+"/api/tags?filename=n.txt&tags=solo", nil)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("tags noauth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no-auth POST /api/tags status = %d, want 200", resp.StatusCode)
	}

	resp2, err := testHTTPClient(t).Get(url + "/api/files/search?tag=solo")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	defer resp2.Body.Close()
	var list files.ListResponse
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Total != 1 || list.Files[0].Name != "n.txt" {
		t.Fatalf("no-auth tag 搜索应命中 n.txt, got %+v", list)
	}
}
