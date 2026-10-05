// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHybridDownload_MultiAccountSharding 多账号分片：账号区 chunk 经 Select round-robin
// 分摊到 N 账号，每账号只转存一次（accounted 去重），直链并行下载，文件完整。
//
// 断言核心：
//  1. restore 调用次数 == 账号数（每账号一次转存副本）
//  2. 文件内容与 payload 一致（多账号分片不损坏）
//  3. 单账号路径（pool=nil）零回归（现有测试覆盖）
func TestHybridDownload_MultiAccountSharding(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 8<<20) // 8MB
	for i := range payload {
		payload[i] = byte(i % 97)
	}
	chunkLen := int64(len(payload) / 4) // 4 chunks × 2MB → 分享区 2 + 账号区 2

	var restoreCalls atomic.Int64
	var dlCalls atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-1", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case r.URL.Path == "/share/dl":
			dlCalls.Add(1)
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/share/restore":
			restoreCalls.Add(1)
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
		case r.URL.Path == "/drive/v1/files":
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/files/restored-1" || (len(r.URL.Path) > 20 && r.URL.Path[:21] == "/drive/v1/files/restored"):
			writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srv.URL + "/drive/dl"})
		case r.URL.Path == "/drive/dl":
			dlCalls.Add(1)
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/files:batchDelete":
			writeJSON(w, map[string]any{"task_id": "del-1"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)

	// 2 账号池（quota 足够放 8MB；fake secrets + 临时凭据目录）
	now := time.Now()
	pool, sec := newTestPool(t, 1<<30, &now)
	addAcct(t, pool, "acct-a", `{"access_token":"tok-a"}`, 1<<30)
	addAcct(t, pool, "acct-b", `{"access_token":"tok-b"}`, 1<<30)
	_ = sec

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 4,
		AutoDelete: false, AccountPool: pool,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, dlErr := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); dlErr != nil {
		t.Fatalf("Download: %v", dlErr)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %d want %d bytes", len(got), len(payload))
	}
	// 多账号：账号区 2 chunk 分摊到 2 账号 → restore 恰好 2 次（每账号一次副本）
	if restoreCalls.Load() != 2 {
		t.Fatalf("restore calls = %d, want 2 (each account restores once)", restoreCalls.Load())
	}
	// 关键差异断言：多账号版**每账号记账**（RecordUsage）——变异回单账号路径时
	// 账号池用量全 0（不经过 pool），此处红。
	acctA, aerr := pool.findLocked("acct-a")
	acctB, berr := pool.findLocked("acct-b")
	if aerr == nil && berr == nil {
		if acctA.DailyUsed == 0 || acctB.DailyUsed == 0 {
			t.Fatalf("multi-account usage not recorded: acct-a=%d acct-b=%d (单账号路径不记账 → 变异)", acctA.DailyUsed, acctB.DailyUsed)
		}
	} else {
		t.Fatalf("pool accounts not found: %v %v", aerr, berr)
	}
	// 下载请求 = 分享区 2 + 账号区 2 = 4（Range 下载）
	if dlCalls.Load() < 4 {
		t.Fatalf("dl calls = %d, want >=4 (2 share + 2 account chunks)", dlCalls.Load())
	}
}

// TestHybridDownload_MultiAccountSingleAccountFallback 账号池只有 1 个账号（或配额不足）时
// 退化为单账号行为：仍能完成下载。
func TestHybridDownload_MultiAccountSingleAccountFallback(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 31)
	}
	chunkLen := int64(len(payload) / 2) // 2 chunks → 分享区 1 + 账号区 1

	var restoreCalls atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case r.URL.Path == "/share/dl" || r.URL.Path == "/drive/dl":
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/share/restore":
			restoreCalls.Add(1)
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
		case r.URL.Path == "/drive/v1/files":
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/files/restored-1" || (len(r.URL.Path) > 20 && r.URL.Path[:21] == "/drive/v1/files/restored"):
			writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srv.URL + "/drive/dl"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	now := time.Now()
	pool, _ := newTestPool(t, 1<<30, &now)
	addAcct(t, pool, "acct-only", `{"access_token":"tok"}`, 1<<30)

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 2,
		AutoDelete: false, AccountPool: pool,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch")
	}
	if restoreCalls.Load() != 1 {
		t.Fatalf("restore calls = %d, want 1 (single account restores once)", restoreCalls.Load())
	}
}

// TestMultiAccountLink_SessionAffinity 锁定 round-13 Critical：accounted 命中路径的
// DownloadLink 必须在 Use（会话）内执行。顺序驱动：预置 accounted 后先取 acct-a 再取
// acct-b 的直链——旧实现第二次调用用 acct-a 缓存 token 签 fid-b（或首次用空会话），
// fake 按 fid 断言所需 Bearer token，错账号请求即 wrongAuth 计数。
func TestMultiAccountLink_SessionAffinity(t *testing.T) {
	t.Parallel()
	now := time.Now()
	pool, _ := newTestPool(t, 1<<30, &now)
	addAcct(t, pool, "acct-a", `{"access_token":"tok-a"}`, 1<<30)
	addAcct(t, pool, "acct-b", `{"access_token":"tok-b"}`, 1<<30)

	var wrongAuth atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/drive/v1/files/fid-a":
			if !strings.HasPrefix(auth, "Bearer tok-a") {
				wrongAuth.Add(1)
			}
			writeJSON(w, map[string]any{"web_content_link": srv.URL + "/dl-a"})
		case "/drive/v1/files/fid-b":
			if !strings.HasPrefix(auth, "Bearer tok-b") {
				wrongAuth.Add(1)
			}
			writeJSON(w, map[string]any{"web_content_link": srv.URL + "/dl-b"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// API 凭据路径指向池的会话目录（生产默认同为 ~/.pikpak/.credentials.json；
	// 测试池 credDir 是临时目录须显式对齐，否则 ensureToken 读到本机真实 ~/.pikpak token）。
	api := NewAPI(APIConfig{Host: srv.URL, CredentialPath: filepath.Join(pool.credDir, ".credentials.json"), HTTPClient: srv.Client()}, nil)
	// 直接驱动 multiAccountLink（单元级，不跑完整下载）：只依赖 d.api + d.pool。
	hd := &HybridDownloader{
		api: api, pool: pool,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx := context.Background()
	// 预置：两账号均已转存（restore/定位阶段已过，只测直链的会话亲和）。
	dc := &downloadCtx{accounted: map[string]string{"acct-a": "fid-a", "acct-b": "fid-b"}}
	acctA, err := pool.Select(ctx, 1)
	if err != nil {
		t.Fatalf("select a: %v", err)
	}
	linkA, err := hd.multiAccountLink(ctx, dc, acctA)
	if err != nil {
		t.Fatalf("link a: %v", err)
	}
	acctB, err := pool.Select(ctx, 1)
	if err != nil {
		t.Fatalf("select b: %v", err)
	}
	linkB, err := hd.multiAccountLink(ctx, dc, acctB)
	if err != nil {
		t.Fatalf("link b: %v", err)
	}
	if linkA != srv.URL+"/dl-a" || linkB != srv.URL+"/dl-b" {
		t.Fatalf("unexpected links: %q %q", linkA, linkB)
	}
	if wrongAuth.Load() != 0 {
		t.Fatalf("session-affinity violated: %d request(s) signed with wrong account token", wrongAuth.Load())
	}
}

// TestHybridDownload_MultiAccountOwned 锁定 round-4 Critical：owned（file_restore_own，源文件
// 已在网盘）分支先前把文件 ID 直接当下载 URL → downloadChunkRange(GET "share-f1") 必然失败
// → 整任务失败。必须与单账号路径一致：经 DownloadLink 取真实直链。自分享（share 自己的文件
// 再下载）是 PikPak 常见场景。同时锁定 NH-P1：owned 的普通 restor 不登记删除（batchDelete=0）。
func TestHybridDownload_MultiAccountOwned(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i % 47)
	}
	chunkLen := int64(len(payload) / 2) // 2 chunks → 分享区 1 + 账号区 1

	var batchDelete atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-1", "expires_in": 3600})
		case "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case "/share/dl":
			serveRange(w, r, payload)
		case "/drive/v1/share/restore":
			// 自分享：源文件已在个人网盘（file_restore_own，错误码 9）
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error_code":9,"error":"file_restore_own"}`))
		case "/drive/v1/files":
			if r.URL.Query().Get("parent_id") != "" {
				writeJSON(w, map[string]any{"files": []any{}})
				return
			}
			// 源文件（自分享 owned）在个人网盘根目录：locateRestored → FindByID 经
			// ListRecursive 全盘查找必须能命中，否则「file not found」。
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case "/drive/v1/files/share-f1":
			writeJSON(w, map[string]any{"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/drive/dl"})
		case "/drive/dl":
			serveRange(w, r, payload)
		case "/drive/v1/files:batchDelete":
			batchDelete.Add(1)
			writeJSON(w, map[string]any{"task_id": "del-1"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	now := time.Now()
	pool, _ := newTestPool(t, 1<<30, &now)
	addAcct(t, pool, "acct-a", `{"access_token":"tok-a"}`, 1<<30)
	addAcct(t, pool, "acct-b", `{"access_token":"tok-b"}`, 1<<30)

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 2,
		AutoDelete: true, AccountPool: pool,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download: %v (owned 分支误用文件 ID 当直链 → 本处红)", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %d want %d bytes", len(got), len(payload))
	}
	if batchDelete.Load() != 0 {
		t.Fatalf("owned（源文件已在网盘）不得被 AutoDelete 删除: batchDelete=%d", batchDelete.Load())
	}
}

// TestHybridDownload_MultiAccount_AutoDelete_PerAccountRelease 锁定 round-4 Critical：
// 多账号下载的转存副本分属 N 个账号，Release 必须逐账号在对应会话内永久删除——此前单次
// batchDelete 用最后一个会话账号的 token 删全部 → 其余账号副本 403 永久泄漏（占满免费账号
// 空间，AutoDelete 默认 true）。断言：每个账号副本都用自己 token 删、删除请求按账号分组（2 次）。
func TestHybridDownload_MultiAccount_AutoDelete_PerAccountRelease(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i % 73)
	}
	chunkLen := int64(len(payload) / 4) // 4 chunks → 分享区 2 + 账号区 2（a、b 各一）

	var deleteCalls, wrongAuth atomic.Int64
	srv := httptest.NewServer(perAccountReleaseHandler(payload, &deleteCalls, &wrongAuth))
	defer srv.Close()

	now := time.Now()
	pool, _ := newTestPool(t, 1<<30, &now)
	addAcct(t, pool, "acct-a", `{"access_token":"tok-a"}`, 1<<30)
	addAcct(t, pool, "acct-b", `{"access_token":"tok-b"}`, 1<<30)
	// API 凭据路径指向池的会话目录：batchDelete 鉴权断言需读真实账号 token（cfgToken 恒
	// fakeServerToken 会掩盖会话切换，Release 逐账号删便不可验证）。
	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, CredentialPath: filepath.Join(pool.credDir, ".credentials.json"), HTTPClient: srv.Client()}, nil)

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 4,
		AutoDelete: true, AccountPool: pool,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %d want %d bytes", len(got), len(payload))
	}
	if deleteCalls.Load() != 2 {
		t.Fatalf("release calls = %d, want 2 (逐账号分组删除；此前单次混删 → 1)", deleteCalls.Load())
	}
	if wrongAuth.Load() != 0 {
		t.Fatalf("cross-account delete leak: %d deleted with wrong account token", wrongAuth.Load())
	}
}

// TestHybridDownload_MultiAccount_SelectByChunkQuota 锁定 round-4 Important：Select 预检此前
// 按整文件大小——文件大于单账号剩余配额时池总量充足也 ErrNoAccountAvailable → 整任务失败。
// 按 chunk 预检后 round-robin 按剩余配额自平衡。800B 文件、chunk 200B、每账号配额 300B：
// 旧实现 Select(800) 直接失败；新实现 Select(200) 分摊成功。
func TestHybridDownload_MultiAccount_SelectByChunkQuota(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 800)
	for i := range payload {
		payload[i] = byte(i % 31)
	}
	chunkLen := int64(200) // 4 chunks → 分享区 2 + 账号区 2（每账号 1）

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-1", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case r.URL.Path == "/share/dl" || strings.HasPrefix(r.URL.Path, "/dl-restored"):
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/share/restore":
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-a"})
		case r.URL.Path == "/drive/v1/files":
			if r.URL.Query().Get("parent_id") != "" {
				writeJSON(w, map[string]any{"files": []any{}})
				return
			}
			// restore 副本在账号网盘根目录：FindByID（全盘查找）需命中。
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-a", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case strings.HasPrefix(r.URL.Path, "/drive/v1/files/restored"):
			writeJSON(w, map[string]any{"id": "restored-a", "name": "movie.mp4", "web_content_link": srv.URL + "/dl-restored-a"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	now := time.Now()
	pool, _ := newTestPool(t, 400, &now)
	addAcct(t, pool, "acct-a", `{"access_token":"tok-a"}`, 300)
	addAcct(t, pool, "acct-b", `{"access_token":"tok-b"}`, 300)

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 4,
		AutoDelete: false, AccountPool: pool,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download: %v (旧实现按整文件 Select(800) 在本处红)", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch")
	}
}

// TestHybridDownload_MultiAccount_RuntimeAddedAccount 锁定 round-14：CLI 运行期 add 的账号
// 经 RefreshAccounts 对账后对 hybrid 生效（旧下载器已有，hybrid 此前缺失 → F5 不对称）。
// 模拟：池装配后 0 账号；另一进程直接把凭据写入 secrets 卷（不经池 Add）；下载开始前
// RefreshAccounts 拾取 → 下载成功。无修复时池恒空 → Select no-account → 下载失败。
func TestHybridDownload_MultiAccount_RuntimeAddedAccount(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 41)
	}
	chunkLen := int64(len(payload) / 2) // 2 chunks → 分享区 1 + 账号区 1

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-1", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case r.URL.Path == "/share/dl" || strings.HasPrefix(r.URL.Path, "/dl-restored"):
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/share/restore":
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
		case r.URL.Path == "/drive/v1/files":
			if r.URL.Query().Get("parent_id") != "" {
				writeJSON(w, map[string]any{"files": []any{}})
				return
			}
			// restore 副本在账号网盘根目录：FindByID（全盘查找）需命中。
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case strings.HasPrefix(r.URL.Path, "/drive/v1/files/restored"):
			writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srv.URL + "/dl-restored-1"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	now := time.Now()
	pool, sec := newTestPool(t, 1<<30, &now)
	// 模拟另一进程（CLI `sproxy pikpak account add`）直接写 secrets 卷：池内无账号。
	if err := sec.Write(context.Background(), "pikpak-cli-added.json",
		[]byte(`{"access_token":"tok-runtime"}`)); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, CredentialPath: filepath.Join(pool.credDir, ".credentials.json"), HTTPClient: srv.Client()}, nil)

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 2,
		AutoDelete: false, AccountPool: pool,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download: %v (缺 RefreshAccounts → 池空 no-account 红)", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %d want %d bytes", len(got), len(payload))
	}
	if len(pool.Accounts()) != 1 {
		t.Fatalf("RefreshAccounts 应拾取运行时添加的账号，got %d", len(pool.Accounts()))
	}
}

// TestHybridDownload_MultiAccount_EmptyPoolFallsBackToSingle 锁定 round-14：已装配池但账号
// 为空时 hybrid 回退当前 CLI 登录态单账号（与旧下载器语义一致），不报「no account」硬错。
// 无修复时 d.pool != nil → 多账号路由 → Select 空池 no-account → 下载失败。
func TestHybridDownload_MultiAccount_EmptyPoolFallsBackToSingle(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 53)
	}
	chunkLen := int64(len(payload) / 2) // 2 chunks → 分享区 1 + 账号区 1

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-1", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case r.URL.Path == "/share/dl" || strings.HasPrefix(r.URL.Path, "/dl-restored"):
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/share/restore":
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
		case r.URL.Path == "/drive/v1/files":
			if r.URL.Query().Get("parent_id") != "" {
				writeJSON(w, map[string]any{"files": []any{}})
				return
			}
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case strings.HasPrefix(r.URL.Path, "/drive/v1/files/restored"):
			writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srv.URL + "/dl-restored-1"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	now := time.Now()
	pool, _ := newTestPool(t, 1<<30, &now) // 空池（未添加任何账号）
	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	// 单账号路径用当前登录态（cfgToken）：空池回落时走 restoreAndLink。
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 2,
		AutoDelete: false, AccountPool: pool,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download: %v (空池应回落单账号，此处红)", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %d want %d bytes", len(got), len(payload))
	}
}

// TestHybridDownload_MultiAccount_ShareDowngrade 多账号路径的分享区→账号区降级：
// /share/dl 恒 500（分享直链失效），probe 失败回落 ratio 边界 → 分享区 chunk 连续失败
// → 降级到账号区（经账号池 Select 分摊）。断言：下载成功 + 内容一致 + DowngradeTotal≥1 +
// 账号区字节记账（AccountBytesUsed == 全文件）。
func TestHybridDownload_MultiAccount_ShareDowngrade(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 61)
	}
	chunkLen := int64(len(payload) / 2) // 2 chunks → 分享区 1 + 账号区 1（分享区失败全降级）

	var restoreCalls atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-1", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case r.URL.Path == "/share/dl":
			// 探针（短 Range ≤1KB）返回 206 → shareEnd 保持 ratio，分享区 chunk 被规划；
			// chunk 请求（长 Range）返回 500 → 分享区 chunk 连续失败 → 降级账号区。
			var s, e int64
			fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e)
			if e-s+1 <= 1024 {
				serveRange(w, r, payload)
				return
			}
			http.Error(w, "share chunk dead", http.StatusInternalServerError)
		case r.URL.Path == "/drive/v1/share/restore":
			restoreCalls.Add(1)
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
		case r.URL.Path == "/drive/v1/files":
			if r.URL.Query().Get("parent_id") != "" {
				writeJSON(w, map[string]any{"files": []any{}})
				return
			}
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case strings.HasPrefix(r.URL.Path, "/drive/v1/files/restored"):
			writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srv.URL + "/dl-restored-1"})
		case strings.HasPrefix(r.URL.Path, "/dl-restored"):
			serveRange(w, r, payload)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	now := time.Now()
	pool, _ := newTestPool(t, 1<<30, &now)
	addAcct(t, pool, "acct-a", `{"access_token":"tok-a"}`, 1<<30)
	addAcct(t, pool, "acct-b", `{"access_token":"tok-b"}`, 1<<30)
	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	metrics := &HybridMetrics{}

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 2,
		AutoDelete: false, AccountPool: pool, Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %d want %d bytes", len(got), len(payload))
	}
	if metrics.DowngradeTotal.Load() < 1 {
		t.Fatalf("downgrade_total = %d, want >=1 (share chunk failed → account)", metrics.DowngradeTotal.Load())
	}
	if metrics.AccountBytesUsed.Load() != int64(len(payload)) {
		t.Fatalf("account_bytes_used = %d, want %d (全部字节经账号区)", metrics.AccountBytesUsed.Load(), len(payload))
	}
	if restoreCalls.Load() < 1 {
		t.Fatalf("restore calls = %d, want >=1", restoreCalls.Load())
	}
}

// TestHybridDownload_MultiAccount_CrashResume 多账号 + 崩溃恢复真实场景：首次下载分享区
// chunk 完成（写 manifest）、账号区 chunk 失败（模拟中途失败，成功路径会 removeManifest，
// 失败路径保留 manifest）→ 二次重跑：分享区 chunk 被 manifest 跳过（无长 Range 请求），
// 账号区 chunk 重新转存下载（失败下载的副本已被 Release 清理），最终文件完整。
// 锁定：manifest 不记账号分配、accounted 每次下载独立重建，resume 语义不受多账号影响。
func TestHybridDownload_MultiAccount_CrashResume(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 59)
	}
	chunkLen := int64(len(payload) / 2) // 2 chunks → 分享区 1 + 账号区 1（shareEnd = 0.5 total）

	var restoreCalls, longShareCalls, acctDLFails atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-1", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case r.URL.Path == "/share/dl":
			var s, e int64
			fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &s, &e)
			if e-s+1 <= 1024 {
				serveRange(w, r, payload) // 探针（短 Range）
				return
			}
			longShareCalls.Add(1) // 分享区 chunk（长 Range）
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/share/restore":
			restoreCalls.Add(1)
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
		case r.URL.Path == "/drive/v1/files":
			if r.URL.Query().Get("parent_id") != "" {
				writeJSON(w, map[string]any{"files": []any{}})
				return
			}
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case strings.HasPrefix(r.URL.Path, "/drive/v1/files/restored"):
			writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srv.URL + "/dl-restored-1"})
		case strings.HasPrefix(r.URL.Path, "/dl-restored"):
			// 账号区 Range：首次下载恒 500（模拟中途失败，重试也失败）；重跑时恢复成功。
			if acctDLFails.Add(1) <= 2 {
				http.Error(w, "transient account dl failure", http.StatusInternalServerError)
				return
			}
			serveRange(w, r, payload)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	now := time.Now()
	pool, _ := newTestPool(t, 1<<30, &now)
	addAcct(t, pool, "acct-a", `{"access_token":"tok-a"}`, 1<<30)
	addAcct(t, pool, "acct-b", `{"access_token":"tok-b"}`, 1<<30)
	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 2,
		AutoDelete: false, AccountPool: pool,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")

	// 第一次：分享区 chunk 完成（manifest 记录），账号区 chunk 失败（2 次重试均失败）→ 报错。
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err == nil {
		t.Fatalf("first download should fail (account chunk transient failure)")
	}
	// 第二次：重跑 → 分享区 chunk 被 manifest 跳过（longShareCalls 不再增长），
	// 账号区 chunk 重新转存下载成功 → 文件完整。
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("resume download: %v", err)
	}
	if longShareCalls.Load() != 1 {
		t.Fatalf("share chunk long-range calls = %d, want 1 (第二次应跳过分享区 chunk)", longShareCalls.Load())
	}
	// 转存计数：首次下载两次尝试各在 a、b 转存一次副本（每次尝试新账号需新副本，失败
	// 下载 Release 已清理）→ 2；重跑再转存 1 次 → 共 3。账号区 chunk 必然重新转存。
	if restoreCalls.Load() != 3 {
		t.Fatalf("restore calls = %d, want 3 (首次 2 次尝试 + 重跑 1 次)", restoreCalls.Load())
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch after crash resume")
	}
}

// TestHybridDownload_MultiAccount_ConcurrentSharedPool 并发多任务共享**同一** HybridDownloader
// 单例 + 同一账号池下载（注册制 cloud manager 并发任务共享形态）：分享直链恒 500（probe 失败
// → shareEnd=0），全程走账号区。两路并发 Download，锁定：
//  1. 每任务文件与 payload 逐字节一致（共享单例 + 每个 downloadCtx 独立状态，不跨任务污染）；
//  2. 账号字节记账精确：metrics.AccountBytesUsed == 2×len(payload)，且池内各账号 DailyUsed 之和
//     亦 == 2×len(payload)（跨任务并发 RecordUsage 无丢更新）；
//  3. -race 探测共享单例下载器的数据竞争。
//
// 用 AutoDelete=false：Release 不删转存副本，规避「跨任务同一分享副本删除干扰」这一已文档化
// 限制（注释见 idempotentRestored），专注断言并发共享单例的记账与内容完整性。
func TestHybridDownload_MultiAccount_ConcurrentSharedPool(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 67)
	}
	chunkLen := int64(len(payload) / 2)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-1", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case r.URL.Path == "/share/dl":
			http.Error(w, "share link dead", http.StatusInternalServerError) // probe 失败 → shareEnd=0
		case r.URL.Path == "/drive/v1/share/restore":
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
		case r.URL.Path == "/drive/v1/files":
			if r.URL.Query().Get("parent_id") != "" {
				writeJSON(w, map[string]any{"files": []any{}})
				return
			}
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case strings.HasPrefix(r.URL.Path, "/drive/v1/files/restored"):
			writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srv.URL + "/dl-restored-1"})
		case strings.HasPrefix(r.URL.Path, "/dl-restored"):
			serveRange(w, r, payload)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	now := time.Now()
	pool, _ := newTestPool(t, 1<<30, &now)
	addAcct(t, pool, "acct-a", `{"access_token":"tok-a"}`, 1<<30)
	addAcct(t, pool, "acct-b", `{"access_token":"tok-b"}`, 1<<30)
	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	metrics := &HybridMetrics{}

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 2,
		AutoDelete: false, AccountPool: pool, Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}

	const tasks = 2
	dests := make([]string, tasks)
	var wg sync.WaitGroup
	errs := make(chan error, tasks)
	for i := range tasks {
		dests[i] = filepath.Join(t.TempDir(), fmt.Sprintf("out%d.mp4", i))
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, derr := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dests[i], nil); derr != nil {
				errs <- fmt.Errorf("task %d: %w", i, derr)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("并发下载失败: %v", e)
	}

	// 内容完整性：每任务 dest 与 payload 逐字节一致（共享单例并发不跨任务污染）。
	for i, d := range dests {
		got, rerr := os.ReadFile(d)
		if rerr != nil {
			t.Fatalf("read task %d: %v", i, rerr)
		}
		if string(got) != string(payload) {
			t.Fatalf("content mismatch task %d: got %d want %d bytes", i, len(got), len(payload))
		}
	}

	// 会计精确：shareEnd=0 全走账号区 → 每任务账号区字节 == len(payload)，两任务合计 2×。
	want := int64(tasks) * int64(len(payload))
	if got := metrics.AccountBytesUsed.Load(); got != want {
		t.Fatalf("account_bytes_used = %d, want %d (跨任务共享池并发无丢更新)", got, want)
	}
	// 池内各账号日用量之和亦 == 2×len(payload)（跨任务并发 RecordUsage 无丢更新）。
	var poolSum int64
	for _, a := range pool.Accounts() {
		poolSum += a.DailyUsed
	}
	if poolSum != want {
		t.Fatalf("pool daily used sum = %d, want %d (跨任务并发记账无丢更新)", poolSum, want)
	}
}

// perAccountReleaseHandler 构造 PerAccountRelease 用例的 mock PikPak API handler：
// 按请求账号返回各自的 restore fid（restored-a/b），batchDelete 校验每个 id 用对的
// 账号 token（跨账号混删 → wrongAuth 计数）。baseURL 经 r.Host 推导（httptest server
// 请求 Host 即其地址），避免 handler 闭包依赖 srv 变量（S3776 收敛：路由 switch 抽为
// 独立函数，测试函数复杂度显著降低）。
func perAccountReleaseHandler(payload []byte, deleteCalls, wrongAuth *atomic.Int64) http.Handler {
	base := func(r *http.Request) string { return "http://" + r.Host }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-1", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": base(r) + "/share/dl",
			}})
		case r.URL.Path == "/share/dl":
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/share/restore":
			fid := "restored-a"
			if strings.HasPrefix(auth, "Bearer tok-b") {
				fid = "restored-b"
			}
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": fid})
		case r.URL.Path == "/drive/v1/files":
			if r.URL.Query().Get("parent_id") != "" {
				writeJSON(w, map[string]any{"files": []any{}})
				return
			}
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-a", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
				{"kind": "drive#file", "id": "restored-b", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/files/restored-a" || r.URL.Path == "/drive/v1/files/restored-b":
			fid := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
			writeJSON(w, map[string]any{"id": fid, "name": "movie.mp4", "web_content_link": base(r) + "/dl-" + fid})
		case strings.HasPrefix(r.URL.Path, "/dl-restored"):
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/files:batchDelete":
			deleteCalls.Add(1)
			var body struct {
				IDs []string `json:"ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, id := range body.IDs {
				want := "tok-a"
				if id == "restored-b" {
					want = "tok-b"
				}
				if !strings.HasPrefix(auth, "Bearer "+want) {
					wrongAuth.Add(1)
				}
			}
			writeJSON(w, map[string]any{"task_id": "del-1"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	})
}
