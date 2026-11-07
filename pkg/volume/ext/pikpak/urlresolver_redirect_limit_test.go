// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestFollowKeepshareShareID_LongChain 较长重定向链（超过旧的 5 跳上限）必须能解析出分享 id。
// 变异：把 maxKeepshareRedirects 改回 5 ⇒ 本测红（上游 2026-11-01 实测链路变长）。
func TestFollowKeepshareShareID_LongChain(t *testing.T) {
	t.Parallel()
	const hops = 7
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/h"))
		if err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if n < hops {
			http.Redirect(w, r, srv.URL+"/h"+strconv.Itoa(n+1), http.StatusFound)
			return
		}
		w.Header().Set("Location", "https://mypikpak.com/s/VEXAMPLE123")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	id, err := followKeepshareShareID(context.Background(), srv.Client(), srv.URL+"/h1")
	if err != nil {
		t.Fatalf("长链应能解析: %v", err)
	}
	if id != "VEXAMPLE123" {
		t.Fatalf("分享 id 不正确: %q", id)
	}
}

// TestFollowKeepshareShareID_LoopDetected 循环重定向必须快速失败（而非耗尽全部跳数）。
func TestFollowKeepshareShareID_LoopDetected(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/same", http.StatusFound)
	}))
	defer srv.Close()

	_, err := followKeepshareShareID(context.Background(), srv.Client(), srv.URL+"/same")
	if err == nil || !strings.Contains(err.Error(), "循环") {
		t.Fatalf("应报循环重定向, err=%v", err)
	}
}
