// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// syncfs_range_contract_test.go 钉住 baidupcs 的 **Range 长度契约**（2026-10-14 跨层扫雷）：
// 与本地卷/加密卷同语义（size==0 = 空区间；越界不得静默短读）。
//
// 变异：恢复「size==0 ⇒ bytes=offset-」尾开区间 ⇒ 零长用例红（会实际发请求并读回尾巴）；
// 去掉 Content-Range 钳制判据 ⇒ 钳制用例红（静默返回 10 字节）。

// TestStorage_GetRange_ZeroSizeIsEmptyNoRequest：size==0 ⇒ 空区间且**不发任何请求**。
func TestStorage_GetRange_ZeroSizeIsEmptyNoRequest(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 10) // 100B
	fs := newTestStorageFS(t)
	if _, err := fs.s.Put(context.Background(), "dir/f.bin", strings.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.ServeContent(w, r, "f.bin", zeroTime, strings.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	fs.s.(*fakeStorage).dlinkBase = srv.URL

	rc, err := fs.OpenRangeRead(context.Background(), "dir/f.bin", 10, 0)
	if err != nil {
		t.Fatalf("size==0 应成功返回空流（与本地卷/加密卷同语义）: %v", err)
	}
	defer rc.Close()
	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("空区间读: %v", rerr)
	}
	if len(got) != 0 {
		t.Fatalf("size==0 必须为空区间，实际读回 %d 字节（尾开区间语义 = 越出请求区间）", len(got))
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("size==0 不得发起下行请求（实际 %d 次）", n)
	}
}

// TestStorageFS_OpenRangeRead_ClampedByServerFails：请求区间越过文件尾时，下游按 RFC 钳制并
// 回 206（Content-Range 只覆盖到末尾）⇒ 必须 **fail-closed 报错**，而不是静默返回更少的字节。
// （本地卷/加密卷对越界即报错 ⇒ 此处对齐，消除「同一请求随卷类型可成可败」的跨层分歧。）
func TestStorageFS_OpenRangeRead_ClampedByServerFails(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 10) // 100B
	fs, rel := newRangeFS(t, content)
	rc, err := fs.OpenRangeRead(context.Background(), rel, 90, 110) // [90,200) 越过 100B
	if err == nil {
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		t.Fatalf("被钳制的区间必须报错（fail-closed），实际静默返回 %d 字节", len(got))
	}
	if !strings.Contains(err.Error(), "钳制") {
		t.Fatalf("错误应可归因「区间被钳制」: %v", err)
	}
}

// TestStorage_GetRange_NegativeSize：负长度是非法区间（与本地卷同口径）。
func TestStorage_GetRange_NegativeSize(t *testing.T) {
	t.Parallel()
	fs, rel := newRangeFS(t, "hello")
	if _, err := fs.OpenRangeRead(context.Background(), rel, 0, -1); err == nil {
		t.Fatal("负 size 必须报错")
	}
}

// TestParseContentRange：Content-Range 解析（含 total=* 与畸形输入）。
func TestParseContentRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw        string
		start, end int64
		ok         bool
	}{
		{"bytes 0-7/8", 0, 7, true},
		{"bytes 90-99/100", 90, 99, true},
		{"bytes 10-19/*", 10, 19, true},
		{"Bytes 5-9/10", 5, 9, true},
		{"bytes */100", 0, 0, false},
		{"bytes 5-4/10", 0, 0, false},
		{"items 1-2/3", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		start, end, ok := parseContentRange(c.raw)
		if ok != c.ok || (ok && (start != c.start || end != c.end)) {
			t.Fatalf("parseContentRange(%q)=(%d,%d,%v) want (%d,%d,%v)", c.raw, start, end, ok, c.start, c.end, c.ok)
		}
	}
}
