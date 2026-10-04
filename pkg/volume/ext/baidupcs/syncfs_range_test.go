// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newRangeTestServer 起 httptest.Server 供文件内容（Range 语义由 net/http ServeContent 承担），
// 并把 base 注入 fakeStorage.dlinkBase（模拟百度 CDN 直链）。rangeHeader 记录最近请求的
// Range 头（断言实际下发的 Range 格式——评审 Minor 测试盲区）。
func newRangeTestServer(t *testing.T, content string, rangeHeader *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rangeHeader != nil {
			*rangeHeader = r.Header.Get("Range")
		}
		http.ServeContent(w, r, "f.bin", zeroTime, strings.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	return srv
}

var zeroTime = time.Time{}

// newRangeFS 建带直链会话的 StorageFS（httptest 驱动 GetRange）。
func newRangeFS(t *testing.T, content string) (*StorageFS, string) {
	t.Helper()
	fs := newTestStorageFS(t)
	if _, err := fs.s.Put(context.Background(), "dir/f.bin", strings.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	srv := newRangeTestServer(t, content, nil)
	fs.s.(*fakeStorage).dlinkBase = srv.URL
	return fs, "dir/f.bin"
}

// TestStorageFS_OpenRangeRead_MiddleSegment：Range 读中间一段 == 原内容对应明文段。
func TestStorageFS_OpenRangeRead_MiddleSegment(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 10) // 100B
	fs, rel := newRangeFS(t, content)
	rc, err := fs.OpenRangeRead(context.Background(), rel, 20, 10)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	defer rc.Close()
	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if string(got) != content[20:30] {
		t.Fatalf("区间读 got %q want %q", got, content[20:30])
	}
}

// TestStorageFS_OpenRangeRead_FullTail：Range 读 [40, size) 尾部段。
func TestStorageFS_OpenRangeRead_FullTail(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 10)
	fs, rel := newRangeFS(t, content)
	rc, err := fs.OpenRangeRead(context.Background(), rel, 40, int64(len(content)-40))
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	defer rc.Close()
	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if string(got) != content[40:] {
		t.Fatalf("尾部段 got %q want %q", got, content[40:])
	}
}

// TestStorageFS_OpenRangeRead_NoSession：无直链会话（dlinkBase 空）→ DirectURL ok=false、
// OpenRangeRead 报错（模拟 binary-only，服务端转发场景退整流 200）。
func TestStorageFS_OpenRangeRead_NoSession(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	if _, err := fs.s.Put(context.Background(), "f.bin", strings.NewReader("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// DirectURL ok=false（无会话）。
	if _, ok, err := fs.DirectURL(context.Background(), "f.bin"); ok || err != nil {
		t.Fatalf("无会话 DirectURL 应 (_,false,nil)，got ok=%v err=%v", ok, err)
	}
	// OpenRangeRead 报错（RangeSeeker 构造失败 → 调用方整流）。
	if _, err := fs.OpenRangeRead(context.Background(), "f.bin", 0, 3); err == nil {
		t.Fatal("无会话 OpenRangeRead 应报错")
	}
}

// TestStorageFS_DirectURL：有会话 → DirectURL 返回 httptest 直链（ok=true）。
func TestStorageFS_DirectURL(t *testing.T) {
	t.Parallel()
	content := "hello baidu"
	fs, rel := newRangeFS(t, content)
	u, ok, err := fs.DirectURL(context.Background(), rel)
	if err != nil || !ok || u == "" {
		t.Fatalf("DirectURL=(%q,%v,%v)，应成功", u, ok, err)
	}
	if !strings.HasPrefix(u, "http") {
		t.Fatalf("直链应以 http 开头: %q", u)
	}
}

// TestStorage_DirectURL_ViaFakeAdapter：Storage.DirectURL 经 fakeStorageAdapter.DirectLink
// （libraryAdapter 直链能力同构）返回 canned dlink；无 directLinkProvider 的 adapter → ok=false。
func TestStorage_DirectURL_ViaFakeAdapter(t *testing.T) {
	t.Parallel()
	// fakeStorageAdapter 实现 directLinkProvider（canned dlink）。
	ad := newFakeStorageAdapter()
	ad.dlink = "https://d.pcs.baidu.com/file?sign=test"
	// DirectLink 检查 remote 存在：root=/baidu → remotePath("f.bin")=/baidu/f.bin。
	ad.files["/baidu/f.bin"] = "hello"
	s := newTestStorage(t, ad)
	u, ok, err := s.DirectURL(context.Background(), "f.bin")
	if err != nil || !ok || u != "https://d.pcs.baidu.com/file?sign=test" {
		t.Fatalf("DirectURL=(%q,%v,%v)，应为 canned dlink", u, ok, err)
	}
}

// TestStorageFS_OpenRangeRead_SendsExactRangeHeader（评审 Minor 补）：OpenRangeRead
// 实际下发的 Range 头格式正确（bytes=off-(off+size-1)）——此前只断言读回内容，
// 未验证下游收到的 Range 区间。
func TestStorageFS_OpenRangeRead_SendsExactRangeHeader(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 10) // 100B
	fs := newTestStorageFS(t)
	if _, err := fs.s.Put(context.Background(), "dir/f.bin", strings.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var gotRange string
	srv := newRangeTestServer(t, content, &gotRange)
	fs.s.(*fakeStorage).dlinkBase = srv.URL

	rc, err := fs.OpenRangeRead(context.Background(), "dir/f.bin", 20, 10)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	defer rc.Close()
	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if string(got) != content[20:30] {
		t.Fatalf("区间读 got %q want %q", got, content[20:30])
	}
	if want := "bytes=20-29"; gotRange != want {
		t.Fatalf("Range 头=%q want %q（实际下发的 Range 区间必须精确）", gotRange, want)
	}
}

// TestStorageFS_OpenRangeRead_Server416（评审 Minor 补）：服务器对越界 Range 返回
// 416 而非 206 → GetRange fail-closed 报错（不静默返回非目标内容）。
func TestStorageFS_OpenRangeRead_Server416(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 10) // 100B
	fs := newTestStorageFS(t)
	if _, err := fs.s.Put(context.Background(), "dir/f.bin", strings.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// 服务器对任何 Range 返回 416（模拟 CDN 拒绝越界区间）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
	}))
	t.Cleanup(srv.Close)
	fs.s.(*fakeStorage).dlinkBase = srv.URL

	_, err := fs.OpenRangeRead(context.Background(), "dir/f.bin", 90, 100) // 越界
	if err == nil {
		t.Fatal("服务器 416 应 fail-closed 报错（不静默返回非目标内容）")
	}
}

// TestStorageFS_OpenRangeRead_Server200（评审 Minor 补）：服务器忽略 Range 返回 200
// （非 206）→ GetRange fail-closed 报错——绝不允许把整文件当区间静默返回。
func TestStorageFS_OpenRangeRead_Server200(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 10)
	fs := newTestStorageFS(t)
	if _, err := fs.s.Put(context.Background(), "dir/f.bin", strings.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 忽略 Range，返回 200 整文件。
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(srv.Close)
	fs.s.(*fakeStorage).dlinkBase = srv.URL

	if _, err := fs.OpenRangeRead(context.Background(), "dir/f.bin", 20, 10); err == nil {
		t.Fatal("服务器忽略 Range 返回 200 应 fail-closed 报错（期望 206）")
	}
}
