// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// dlink_reuse_test.go —— 同一 key 的多次 Range GET **不得**重复签发直链（真实用户实测缺陷）。
//
// 背景（2026-10-21，WSL + 百度网盘真机）：
//   - 真机实测 `LocateDownload`（直链签发）**每次约 0.3s**（网盘元数据 API + 签名）；
//   - secretdata 读一个文件需要「salt 段 + N 个 blocklet 段」= 1+N 次 Range GET
//     （见 pkg/volume/secretdata/erasure.go 的 readChunkRangeBySegments）；
//   - 旧实现 `Storage.GetRange` **每次都调 `DirectURL` → `LocateDownload`** ⇒ N+1 次签发，
//     单文件随机读凭空多付 (N+1)×0.3s（实测 242KB MP4 Range 请求 47s）。
//
// 修复：Storage 内按 key 缓存 dlink（短 TTL，默认 60s，远小于百度 dlink 的 8h 有效期），
// 命中即复用 —— 同一文件的多次段读只签发一次。dlink 失效由 CDN 返回非 206 暴露，
// 此时**失效缓存并重签一次**（自愈，不把失败暴露给调用方）。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dlinkCountingAdapter 包装 fakeStorageAdapter：统计 DirectLink（= 直链签发）调用次数。
// 经 StorageConfig.Adapter（结构体字段）注入 —— 不是包级可变 seam。
type dlinkCountingAdapter struct {
	*fakeStorageAdapter
	signs atomic.Int64
}

func (c *dlinkCountingAdapter) DirectLink(ctx context.Context, remotePath string) (string, bool, error) {
	c.signs.Add(1)
	return c.fakeStorageAdapter.DirectLink(ctx, remotePath)
}

// newCountingRangeEnv 建「可计数直链签发」的 Storage + 内容服务器（直链打到 httptest）。
func newCountingRangeEnv(t *testing.T, content string) (*Storage, *dlinkCountingAdapter, string) {
	t.Helper()
	srv := newRangeTestServer(t, content, nil)
	ad := &dlinkCountingAdapter{fakeStorageAdapter: newFakeStorageAdapter()}
	ad.dlink = srv.URL + "/f.bin"
	st, err := NewStorage(StorageConfig{
		Root:    "/",
		TempDir: t.TempDir(),
		Adapter: ad,
		Logger:  testLogger(),
	})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if _, perr := st.Put(context.Background(), "dir/f.bin", strings.NewReader(content)); perr != nil {
		t.Fatalf("Put: %v", perr)
	}
	return st, ad, "dir/f.bin"
}

// readRange 读 key 的 [offset, offset+size) 并返回内容（失败即 Fatal）。
func readRange(t *testing.T, st *Storage, key string, offset, size int64) string {
	t.Helper()
	rc, err := st.GetRange(context.Background(), key, offset, size)
	if err != nil {
		t.Fatalf("GetRange(%d,%d): %v", offset, size, err)
	}
	defer rc.Close()
	buf, rerr := io.ReadAll(io.LimitReader(rc, size))
	if rerr != nil {
		t.Fatalf("读区间: %v", rerr)
	}
	if int64(len(buf)) != size {
		t.Fatalf("区间长度=%d，应为 %d", len(buf), size)
	}
	return string(buf)
}

// TestGetRange_ReusesSingleDlinkSigning：同一 key 连续多次 Range GET **只签发一次直链**
// （旧实现每次重签 ⇒ 网盘场景每段多付 ~0.3s，实测量级差异）。
func TestGetRange_ReusesSingleDlinkSigning(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("0123456789", 100) // 1000B
	st, ad, key := newCountingRangeEnv(t, content)

	for i := 0; i < 3; i++ {
		got := readRange(t, st, key, int64(i*100), 100)
		if got != content[i*100:(i+1)*100] {
			t.Fatalf("第 %d 段内容错误: %q", i, got)
		}
	}
	if n := ad.signs.Load(); n != 1 {
		t.Fatalf("直链签发次数=%d，应为 1（同一 key 的段读必须复用 dlink；旧实现每段一次 ⇒ 网盘每段多付 ~0.3s）", n)
	}
}

// TestGetRange_ConcurrentSegmentsShareDlink：并发段读（RangeSeeker 多窗口 / 并发请求）
// 同样只签发一次（缓存须并发安全）。
func TestGetRange_ConcurrentSegmentsShareDlink(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 100)
	st, ad, key := newCountingRangeEnv(t, content)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = readRange(t, st, key, int64(i*50), 50)
		}(i)
	}
	wg.Wait()
	if n := ad.signs.Load(); n != 1 {
		t.Fatalf("并发段读直链签发次数=%d，应为 1（dlink 缓存须并发安全且复用）", n)
	}
}

// TestGetRange_ExpiredDlinkResignsOnce：dlink 失效（CDN 非 206）⇒ 失效缓存并重签一次后
// 成功（自愈；不得把过期直链的失败直接暴露给调用方）。
func TestGetRange_ExpiredDlinkResignsOnce(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("xyz", 200)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 2 {
			// 第二次请求（复用缓存的 dlink）模拟「签名已失效」：403 而非 206。
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		http.ServeContent(w, r, "f.bin", time.Time{}, strings.NewReader(content))
	}))
	t.Cleanup(srv.Close)

	ad := &dlinkCountingAdapter{fakeStorageAdapter: newFakeStorageAdapter()}
	ad.dlink = srv.URL + "/f.bin"
	st, err := NewStorage(StorageConfig{Root: "/", TempDir: t.TempDir(), Adapter: ad, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if _, perr := st.Put(context.Background(), "dir/f.bin", strings.NewReader(content)); perr != nil {
		t.Fatalf("Put: %v", perr)
	}

	// 第一次段读：签直链（hit 1 → 206 OK）并缓存；第二次段读复用缓存直链（hit 2 → 403
	// 模拟签名失效）⇒ 实现应失效缓存、重签（sign #2）并重试（hit 3 → 206 OK）。
	if got := readRange(t, st, "dir/f.bin", 0, 30); got != content[:30] {
		t.Fatalf("首段内容错误: %q", got)
	}
	got := readRange(t, st, "dir/f.bin", 100, 30)
	if got != content[100:130] {
		t.Fatalf("失效重签后内容错误: %q", got)
	}
	if n := ad.signs.Load(); n != 2 {
		t.Fatalf("直链签发次数=%d，应为 2（首次 + 失效后重签一次）", n)
	}
}

// TestGetRange_DifferentKeysSignSeparately：不同 key 各自签发（缓存按 key 隔离，
// 不得串用别的文件的 dlink —— 串用会读到错误文件）。
func TestGetRange_DifferentKeysSignSeparately(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("A", 100)
	st, ad, _ := newCountingRangeEnv(t, content)
	ctx := context.Background()
	if _, perr := st.Put(ctx, "dir/other.bin", strings.NewReader(content)); perr != nil {
		t.Fatalf("Put other: %v", perr)
	}
	_ = readRange(t, st, "dir/f.bin", 0, 10)
	_ = readRange(t, st, "dir/other.bin", 0, 10)
	if n := ad.signs.Load(); n != 2 {
		t.Fatalf("不同 key 直链签发次数=%d，应为 2（缓存按 key 隔离）", n)
	}
}
