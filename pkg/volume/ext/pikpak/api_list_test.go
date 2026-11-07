// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAPI_List_Pagination EXT-6 回归：/drive/v1/files 必须按 next_page_token 循环取全量，
// 否则 >500 条目目录的后续文件对 Stat/Get/List 变成「不存在」。
func TestAPI_List_Pagination(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/drive/v1/files" {
			http.NotFound(w, r)
			return
		}
		calls++
		switch r.URL.Query().Get("page_token") {
		case "":
			_, _ = w.Write([]byte(`{"files":[{"id":"1","name":"a"}],"next_page_token":"tok2"}`))
		case "tok2":
			_, _ = w.Write([]byte(`{"files":[{"id":"2","name":"b"}]}`))
		default:
			_, _ = w.Write([]byte(`{"files":[]}`))
		}
	}))
	defer srv.Close()

	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: "t"}, nil)
	files, err := api.List(context.Background(), "root")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != 2 || files[0].ID != "1" || files[1].ID != "2" {
		t.Fatalf("分页应取全量, got %+v", files)
	}
	if calls != 2 {
		t.Fatalf("请求次数 = %d, want 2（第一页 + 第二页）", calls)
	}
}
