// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestVolumeMutationObserver_InstalledBeforeTenants 钉住「观察者必须在租户创建之前装好」：
//
// 背景（本轮实测）：`RegisterRoutes` 在 `initStorageLayout` 里**先建 anonymous 租户**，而
// files.Service 是懒装配的——若观察者等到那时才装到卷根上，已创建的租户子根**不会继承**
// 观察者（`OpenTenant` 只在创建时继承）⇒ 该租户的所有写都不发事件 ⇒ 卷状态缓存永不失效
// （UI 表现为 rename/delete 后列表仍显示旧内容，直到 TTL）。
//
// 修法：装配早期装**转发器**（`installLocalVolumeObservers`），files.Service 就绪后再把真正
// 的处理函数存入原子指针（`wireLocalVolumeMutations`）。
//
// 变异：把 `installLocalVolumeObservers()` 从 `initStorageLayout` 之前移走（只在
// fileService() 里装）⇒ 本用例红（rename 后仍显示旧名）。
func TestVolumeMutationObserver_InstalledBeforeTenants(t *testing.T) {
	t.Parallel()
	urlBase, _ := newTestServerWithAllRoutes(t, nil)

	body := "hello"
	sum := sha256.Sum256([]byte(body))
	cs := hex.EncodeToString(sum[:])

	// 上传（multipart，字段 file；header 带 checksum）
	var buf strings.Builder
	buf.WriteString("--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\n\r\n")
	buf.WriteString(body)
	buf.WriteString("\r\n--B--\r\n")
	req, _ := http.NewRequest(http.MethodPost, urlBase+"/upload", strings.NewReader(buf.String()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=B")
	req.Header.Set("X-File-Checksum", cs)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	_ = resp.Body.Close()

	listNames := func() []string {
		r, _ := http.NewRequest(http.MethodGet, urlBase+"/api/files?owner=anonymous", nil)
		rr, lerr := testHTTPClient(t).Do(r)
		if lerr != nil {
			t.Fatalf("list: %v", lerr)
		}
		defer rr.Body.Close()
		raw, _ := io.ReadAll(rr.Body)
		var parsed struct {
			Files []struct {
				Name string `json:"name"`
			} `json:"files"`
		}
		_ = json.Unmarshal(raw, &parsed)
		out := make([]string, 0, len(parsed.Files))
		for _, f := range parsed.Files {
			out = append(out, f.Name)
		}
		return out
	}
	t.Logf("before=%v", listNames())

	r, _ := http.NewRequest(http.MethodPost, urlBase+"/rename?from=a.txt&to=b.txt&owner=anonymous", nil)
	r.Header.Set("X-File-Checksum", cs)
	rr, rerr := testHTTPClient(t).Do(r)
	if rerr != nil {
		t.Fatalf("rename: %v", rerr)
	}
	_ = rr.Body.Close()

	after := listNames()
	t.Logf("after=%v", after)
	for _, n := range after {
		if n == "a.txt" {
			t.Fatalf("rename 后仍显示旧名（缓存未失效）: %v", after)
		}
	}
	if len(after) != 1 || after[0] != "b.txt" {
		t.Fatalf("rename 后应只见 b.txt，got %v", after)
	}

	// 删除同样必须立即反映（Root.Remove 事件 → 缓存失效）。
	dr, _ := http.NewRequest(http.MethodPost, urlBase+"/delete?filename=b.txt&owner=anonymous", nil)
	dr.Header.Set("X-File-Checksum", cs)
	dresp, derr := testHTTPClient(t).Do(dr)
	if derr != nil {
		t.Fatalf("delete: %v", derr)
	}
	_ = dresp.Body.Close()
	if got := listNames(); len(got) != 0 {
		t.Fatalf("删除后列表应为空（缓存未失效），got %v", got)
	}
	_ = url.Values{}
}

// 追加断言：删除同样必须立即反映（事件 → 缓存失效）。

// delete 也必须立即反映（Root.Remove 事件 → 缓存失效）。
