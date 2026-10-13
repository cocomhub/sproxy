// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// mkdir_volume_test.go 钉住：**mkdir/rmdir 请求必须携带卷上下文**（`--volume` / CLI 卷 URI /
// 工作目录所在卷）。
//
// 为什么值得单测：这两个方法历史上手拼 URL，漏了 `appendVolumeQuery` ⇒ 服务端按 auto 解析 ⇒
// 「mkdir --volume <加密卷>」静默建到**本地卷**（用户看到「目录已创建」，目标卷上却找不到）。
//
// 变异：去掉 `appendVolumeQuery` → 本用例红。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMkdir_Rmdir_CarryVolumeQuery(t *testing.T) {
	t.Parallel()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.Query().Get("volume")+"|"+r.URL.Query().Get("dirname"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	c.SetVolume("vault")
	ctx := context.Background()
	if err := c.Mkdir(ctx, "sub"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := c.Rmdir(ctx, "sub"); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}
	want := []string{"/mkdir?vault|sub", "/rmdir?vault|sub"}
	if len(got) != len(want) {
		t.Fatalf("请求数=%d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("请求[%d]=%q want %q", i, got[i], want[i])
		}
	}
}
