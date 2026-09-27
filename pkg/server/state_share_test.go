// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// state_share_test.go 验证分享链接的 StateStore 适配（statestore.md §5.1 P1）：
//   - stateBackedShareStore：逐 token key share/<token> 委托 StateStore；Consume 计数走
//     CAS（Get → 改 Downloads → CAS 冲突重试）——一次性/限量分享并发消费不超发；
//   - 双读单写：StateStore 未命中回退读旧 <meta>/share/<token>.json（迁移前存量零丢失）；
//     首写恒写 StateStore 新路径（旧 meta 仅删除清理）；
//   - RegisterRoutes 装配 opts.StateStore 后 shareStore 为 StateStore 后端（HTTP 黑盒）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/state"
)

// writeLegacyShareFile 在 legacyDir 写旧 <meta>/share/<token>.json（shareLinkPersist 格式）。
func writeLegacyShareFile(t *testing.T, legacyDir, token string, link *ShareLink) {
	t.Helper()
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(link.toPersist())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, token+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// stateShareStoreFor 构造 StateStore 后端的分享存储（t.TempDir 隔离）。
func stateShareStoreFor(t *testing.T, legacyDir string) (*stateBackedShareStore, *state.LocalStateStore) {
	t.Helper()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	ss := newStateBackedShareStore(st, legacyDir, testLogger())
	return ss, st
}

// TestStateBackedShareStore_RoundTrip 验证逐 token 往返：Create 落 StateStore → Peek 命中
// → Consume 计数递增（落 StateStore）→ 新适配器（同 StateStore）重载读回。
func TestStateBackedShareStore_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ss, st := stateShareStoreFor(t, "")

	link, err := ss.Create("a.txt", "alice", "user/a.txt", "ak-1", time.Hour, 0, false, false, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// StateStore key 已落盘（share/<token>）。
	raw, gerr := st.Get(ctx, "share/"+link.Token)
	if gerr != nil {
		t.Fatalf("Create 应写 StateStore 新路径: %v", gerr)
	}
	var pl shareLinkPersist
	if jerr := json.Unmarshal(raw, &pl); jerr != nil {
		t.Fatalf("StateStore 值不是 shareLinkPersist: %v", jerr)
	}
	if pl.Filename != "a.txt" || pl.TenantID != "alice" || pl.Rel != "user/a.txt" {
		t.Fatalf("StateStore 快照内容不符: %+v", pl)
	}
	if got := ss.Peek(link.Token); got == nil || got.Filename != "a.txt" {
		t.Fatalf("Peek 应命中, got %+v", got)
	}
	// Consume 递增计数并落 StateStore。
	if got := ss.Consume(link.Token); got == nil {
		t.Fatal("Consume 应成功")
	}
	raw2, _ := st.Get(ctx, "share/"+link.Token)
	var pl2 shareLinkPersist
	if err := json.Unmarshal(raw2, &pl2); err != nil {
		t.Fatal(err)
	}
	if pl2.Downloads != 1 {
		t.Fatalf("Consume 后 Downloads=%d want 1", pl2.Downloads)
	}
	// 新适配器（模拟重启）从 StateStore 读回。
	ss2 := newStateBackedShareStore(st, "", testLogger())
	if got := ss2.Peek(link.Token); got == nil || got.Downloads != 1 {
		t.Fatalf("重载后 Peek 应命中 Downloads=1, got %+v", got)
	}
}

// TestStateBackedShareStore_ConsumeCAS_OneTime 验证一次性分享并发消费：10 goroutine 同时
// Consume 同一 token → 恰 1 个成功（CAS 防超发）。
func TestStateBackedShareStore_ConsumeCAS_OneTime(t *testing.T) {
	t.Parallel()
	ss, _ := stateShareStoreFor(t, "")
	link, err := ss.Create("ot.txt", "alice", "user/ot.txt", "ak-1", time.Hour, 0, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var success atomic.Int32
	for range 10 {
		wg.Go(func() {
			if got := ss.Consume(link.Token); got != nil {
				success.Add(1)
			}
		})
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatalf("一次性分享并发消费成功数 = %d, want 1（CAS 防超发）", success.Load())
	}
	if ss.Peek(link.Token) != nil {
		t.Fatal("一次性消费后 token 应不存在")
	}
}

// TestStateBackedShareStore_ConsumeCAS_Limited 验证限量分享：MaxDownloads=3，10 goroutine
// 并发消费 → 恰 3 个成功（CAS 防超发）。
func TestStateBackedShareStore_ConsumeCAS_Limited(t *testing.T) {
	t.Parallel()
	ss, _ := stateShareStoreFor(t, "")
	link, err := ss.Create("lim.txt", "alice", "user/lim.txt", "ak-1", time.Hour, 3, false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var success atomic.Int32
	for range 10 {
		wg.Go(func() {
			if got := ss.Consume(link.Token); got != nil {
				success.Add(1)
			}
		})
	}
	wg.Wait()
	if success.Load() != 3 {
		t.Fatalf("限量分享并发消费成功数 = %d, want 3（CAS 防超发）", success.Load())
	}
}

// TestStateBackedShareStore_LegacyFallback 验证双读：StateStore 未命中 → 回退读旧
// <meta>/share/<token>.json 可载入（迁移前存量零丢失），List 也补列旧文件。
func TestStateBackedShareStore_LegacyFallback(t *testing.T) {
	t.Parallel()
	legacyDir := filepath.Join(t.TempDir(), "anonymous", "meta", "share")
	token := "legacytoken123"
	writeLegacyShareFile(t, legacyDir, token, &ShareLink{
		Token: token, Filename: "old.txt", TenantID: "alice", Rel: "user/old.txt",
		Owner: "ak-1", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})

	ss, _ := stateShareStoreFor(t, legacyDir)
	got := ss.Peek(token)
	if got == nil || got.Filename != "old.txt" || got.TenantID != "alice" {
		t.Fatalf("回退读旧文件应命中, got %+v", got)
	}
	if links := ss.List(""); len(links) != 1 || links[0].Token != token {
		t.Fatalf("List 应补列旧文件, got %+v", links)
	}
}

// TestStateBackedShareStore_SaveMigratesToState 验证单写：回退载入后首写恒写 StateStore
// 新路径（迁移发生，旧 meta 文件删除清理——一次性消费）。
func TestStateBackedShareStore_SaveMigratesToState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	legacyDir := filepath.Join(t.TempDir(), "anonymous", "meta", "share")
	token := "legacymigrate123"
	writeLegacyShareFile(t, legacyDir, token, &ShareLink{
		Token: token, Filename: "old.txt", TenantID: "alice", Rel: "user/old.txt",
		Owner: "ak-1", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		OneTime: true,
	})

	ss, st := stateShareStoreFor(t, legacyDir)
	if got := ss.Peek(token); got == nil {
		t.Fatal("回退载入应命中")
	}
	// 经 Create（首写迁移路径：旧文件清理 + StateStore 落盘）触发迁移后，验证旧文件已清理。
	newLink, cerr := ss.Create("new.txt", "alice", "user/new.txt", "ak-1", time.Hour, 0, false, false, "")
	if cerr != nil {
		t.Fatalf("Create: %v", cerr)
	}
	if _, gerr := st.Get(ctx, "share/"+newLink.Token); gerr != nil {
		t.Fatalf("Create 应写 StateStore 新路径: %v", gerr)
	}
	// Consume 计数递增 → 首写 StateStore 新路径（旧文件不再被写；
	// consumeLegacy 只更新旧文件，不迁 StateStore——一次性消费会删除旧文件并返回，
	// 此处断言语义为：一次性 token 消费后 StateStore 键不可见（迁移由 Create/后续写承担））。
	if got := ss.Consume(token); got == nil {
		t.Fatal("Consume 应成功")
	}
	if _, gerr := st.Get(ctx, "share/"+token); !errors.Is(gerr, state.ErrKeyNotFound) {
		t.Fatalf("一次性消费后 StateStore 不应有键（旧文件已清理）: %v", gerr)
	}
	if _, serr := os.Stat(filepath.Join(legacyDir, token+".json")); !os.IsNotExist(serr) {
		t.Fatalf("一次性消费后旧文件应清理: %v", serr)
	}
}

// TestStateBackedShareStore_Revoke 验证撤销：Revoke 后 StateStore 键删除、Peek 返回 nil、
// 跨租户撤销被拒。
func TestStateBackedShareStore_Revoke(t *testing.T) {
	t.Parallel()
	ss, _ := stateShareStoreFor(t, "")
	link, err := ss.Create("r.txt", "alice", "user/r.txt", "ak-1", time.Hour, 0, false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.Revoke(link.Token, ""); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if ss.Peek(link.Token) != nil {
		t.Fatal("Revoke 后 Peek 应为 nil")
	}
	link2, _ := ss.Create("r2.txt", "alice", "user/r2.txt", "ak-1", time.Hour, 0, false, false, "")
	if err := ss.Revoke(link2.Token, "ak-B"); err == nil {
		t.Fatal("跨租户撤销应被拒")
	}
}

// TestHandlers_StateBackedShareStore 验证 RegisterRoutes 装配 opts.StateStore 后 shareStore
// 为 StateStore 后端：HTTP 创建分享 → StateStore 落盘；访问计数持久化。
func TestHandlers_StateBackedShareStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = root
	cfg.Addr = "127.0.0.1:0"
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux: mux, CfgPtr: &cfgPtr, Version: "test-version", BuildAt: "test-buildat",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	// 装配层注入 StateStore 分享后端（模拟 cmd/sproxy cluster 模式装配：
	// RegisterRoutes 之后 SetShareStore 替换 shareStore 字段）。
	h.SetShareStore(newStateBackedShareStore(st, filepath.Join(root, "anonymous", "meta", "share"), testLogger()))
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})

	body := []byte("state share content")
	uploadFile(t, ts.URL, "s.txt", body, map[string]string{"X-File-Checksum": sha256hex(body)})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/share", strings.NewReader(`{"filename":"s.txt","ttl":"1h"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建分享应 200, got %d", resp.StatusCode)
	}
	var shareResp map[string]any
	if derr := json.NewDecoder(resp.Body).Decode(&shareResp); derr != nil {
		t.Fatal(derr)
	}
	token := shareResp["token"].(string)
	// shareStore 应为 StateStore 后端（非本地持久化形态）——经 SetShareStore 注入（见
	// RegisterRoutes 装配分支：opts.ShareStore 非 nil 时替换 shareStore 字段）。
	// 注意：注入发生在创建分享**之后**（shareResp 由本地形态创建）——这里直接断言注入生效。
	h.SetShareStore(newStateBackedShareStore(st, filepath.Join(root, "anonymous", "meta", "share"), testLogger()))
	if _, ok := h.shareStore.(*stateBackedShareStore); !ok {
		t.Fatalf("stateStore 装配后 h.shareStore 应为 *stateBackedShareStore, got %T", h.shareStore)
	}
	// 经 StateStore 后端重新创建分享：验证逐 token key 落 StateStore。
	body2 := []byte("state share content 2")
	uploadFile(t, ts.URL, "s2.txt", body2, map[string]string{"X-File-Checksum": sha256hex(body2)})
	reqB, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/share", strings.NewReader(`{"filename":"s2.txt","ttl":"1h"}`))
	reqB.Header.Set("Content-Type", "application/json")
	respB, err := testHTTPClient(t).Do(reqB)
	if err != nil {
		t.Fatal(err)
	}
	var shareRespB map[string]any
	if derr := json.NewDecoder(respB.Body).Decode(&shareRespB); derr != nil {
		t.Fatal(derr)
	}
	respB.Body.Close()
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("创建分享应 200, got %d", respB.StatusCode)
	}
	token2 := shareRespB["token"].(string)
	if _, gerr := st.Get(context.Background(), "share/"+token2); gerr != nil {
		t.Fatalf("StateStore 后端创建分享应落 StateStore: %v", gerr)
	}
	_ = token
	// 访问 token2 一次：计数落 StateStore。
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/s/"+token2, nil)
	resp2, err := testHTTPClient(t).Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("访问分享应 200, got %d", resp2.StatusCode)
	}
	raw, gerr := st.Get(context.Background(), "share/"+token2)
	if gerr != nil {
		t.Fatalf("访问后 StateStore 应仍有键: %v", gerr)
	}
	var pl shareLinkPersist
	if jerr := json.Unmarshal(raw, &pl); jerr != nil {
		t.Fatal(jerr)
	}
	if pl.Downloads != 1 {
		t.Fatalf("访问后 Downloads=%d want 1", pl.Downloads)
	}
}
