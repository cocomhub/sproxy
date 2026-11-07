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

// syncfs_range_realimpl_test.go 直测**真实** `Storage.GetRange`（此前该实现被测试替身
// `fakeStorage.GetRange` 完全顶替 ⇒ 长度契约无覆盖，这正是「size==0 尾开区间」长期存活的原因）。
//
// 变异：恢复 size==0 ⇒ 尾开区间 ⇒ 零长用例红；去掉 Content-Range 钳制判据 ⇒ 钳制用例红。

// newRealRangeStorage 构造真实 Storage（fake adapter 提供 canned 直链指向 httptest 服务）。
func newRealRangeStorage(t *testing.T, dlink string) *Storage {
	t.Helper()
	ad := newFakeStorageAdapter()
	ad.dlink = dlink
	ad.files["/baidu/f.bin"] = "hello"
	return newTestStorage(t, ad)
}

// TestStorageReal_GetRange_ZeroSizeNoRequest：size==0 ⇒ 空区间且**不发下行请求**。
func TestStorageReal_GetRange_ZeroSizeNoRequest(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 10)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.ServeContent(w, r, "f.bin", zeroTime, strings.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	s := newRealRangeStorage(t, srv.URL)

	rc, err := s.GetRange(context.Background(), "f.bin", 10, 0)
	if err != nil {
		t.Fatalf("size==0 应为空区间成功: %v", err)
	}
	defer rc.Close()
	got, rerr := io.ReadAll(rc)
	if rerr != nil || len(got) != 0 {
		t.Fatalf("size==0 应为空区间, got %d 字节 err=%v（尾开区间语义会读回整个尾巴）", len(got), rerr)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("size==0 不得发起下行请求（实际 %d 次）", n)
	}
}

// TestStorageReal_GetRange_ClampedFails：服务端按 RFC 钳制（请求 [90,200) 而文件 100B）⇒ 报错。
func TestStorageReal_GetRange_ClampedFails(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.bin", zeroTime, strings.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	s := newRealRangeStorage(t, srv.URL)

	_, err := s.GetRange(context.Background(), "f.bin", 90, 110)
	if err == nil {
		t.Fatal("被钳制的区间必须报错（本地卷/加密卷对越界亦 fail-closed）")
	}
	if !strings.Contains(err.Error(), "钳制") {
		t.Fatalf("错误应可归因「区间被钳制」: %v", err)
	}
}

// TestStorageReal_GetRange_ExactOk：正常区间读回精确字节，且下发的 Range 头精确。
func TestStorageReal_GetRange_ExactOk(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 10) // 100B
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		http.ServeContent(w, r, "f.bin", zeroTime, strings.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	s := newRealRangeStorage(t, srv.URL)

	rc, err := s.GetRange(context.Background(), "f.bin", 20, 10)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer rc.Close()
	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if string(got) != content[20:30] {
		t.Fatalf("区间读 got %q want %q", got, content[20:30])
	}
	if gotRange != "bytes=20-29" {
		t.Fatalf("Range 头=%q want %q", gotRange, "bytes=20-29")
	}
}

// TestStorageReal_GetRange_InvalidRange：负 offset/size ⇒ 非法区间（不触网）。
func TestStorageReal_GetRange_InvalidRange(t *testing.T) {
	t.Parallel()
	s := newRealRangeStorage(t, "http://127.0.0.1:1/never")
	if _, err := s.GetRange(context.Background(), "f.bin", -1, 4); err == nil {
		t.Fatal("负 offset 必须报错")
	}
	if _, err := s.GetRange(context.Background(), "f.bin", 0, -4); err == nil {
		t.Fatal("负 size 必须报错")
	}
}
