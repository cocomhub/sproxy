// Copyright (C) 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestDeleteUsesUIPrefs 锁**边界**：页面配置**不改变 API 默认语义**（零回归），
// 页面通过显式 `?soft=` 落实用户配置：
//   - 不带 ?soft 的删除仍是**硬删**（历史契约，CLI/API 不受用户页面配置影响）；
//   - 显式 ?soft=true ⇒ 进回收站；
//   - 用户配置 hard 后，页面显式传 soft=false ⇒ 仍是硬删（与配置一致）。
//
// 为什么不做成「服务端按配置改默认」：那会让同一 HTTP 请求的语义随**用户页面设置**漂移
// （CLI/脚本/其它客户端无法预期），属破坏性变更。配置只影响「页面默认勾了什么」。
func TestDeleteUsesUIPrefs(t *testing.T) {
	t.Parallel()
	url, h := newEventTestServer(t, nil)
	client := testHTTPClient(t)

	upload := func(name string) string {
		body := []byte("payload-" + name)
		if st, resp := uploadFile(t, url, name, body, map[string]string{
			"X-File-Checksum": sha256hex(body),
		}); st != http.StatusOK {
			t.Fatalf("upload %s status=%d body=%s", name, st, resp)
		}
		return sha256hex(body)
	}
	setPrefs := func(mode string) {
		req, _ := http.NewRequest(http.MethodPut, url+"/api/ui/prefs",
			strings.NewReader(`{"delete":{"default_mode":"`+mode+`"}}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT prefs: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT prefs status=%d", resp.StatusCode)
		}
	}
	del := func(name, sum, query string) int {
		req, _ := http.NewRequest(http.MethodPost, url+"/delete?filename="+name+query, nil)
		req.Header.Set("X-File-Checksum", sum)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	trashLen := func() int {
		resp, err := client.Get(url + "/api/trash")
		if err != nil {
			t.Fatalf("GET trash: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Entries []json.RawMessage `json:"entries"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return len(out.Entries)
	}

	// ① 不带 soft ⇒ 硬删（API 契约不变），回收站仍为空。
	sum1 := upload("pref1.txt")
	if code := del("pref1.txt", sum1, ""); code != http.StatusOK {
		t.Fatalf("默认删除 status=%d", code)
	}
	if n := trashLen(); n != 0 {
		t.Fatalf("不带 soft 应为硬删，回收站条目 = %d，期望 0", n)
	}

	// ② 显式 ?soft=true ⇒ 进回收站。
	sum2 := upload("pref2.txt")
	if code := del("pref2.txt", sum2, "&soft=true"); code != http.StatusOK {
		t.Fatalf("soft 删除 status=%d", code)
	}
	if n := trashLen(); n != 1 {
		t.Fatalf("显式 soft 后回收站条目 = %d，期望 1", n)
	}

	// ③ 用户配置 hard ⇒ 页面显式传 soft=false，仍是硬删（配置与显式参数一致时不产生回收站条目）。
	setPrefs("hard")
	sum3 := upload("pref3.txt")
	if code := del("pref3.txt", sum3, "&soft=false"); code != http.StatusOK {
		t.Fatalf("soft=false 删除 status=%d", code)
	}
	if n := trashLen(); n != 1 {
		t.Fatalf("soft=false 后回收站条目 = %d，期望仍为 1", n)
	}
	_ = h
}
