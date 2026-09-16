// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestShare_Persist_RestartRestores 验证 ShareStore 持久化：Create 后落盘，新建
// ShareStore（模拟重启）从同一目录恢复未过期链接；Consume/Revoke 后落盘删除。
func TestShare_Persist_RestartRestores(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ss := NewShareStore(slog.Default())
	ss.EnablePersist(dir)
	link, err := ss.Create("a.txt", "alice", "user/a.txt", "ak-1", time.Hour, 0, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 落盘文件存在（<token>.json）。
	persistFile := filepath.Join(dir, link.Token+".json")
	if _, err := os.Stat(persistFile); err != nil {
		t.Fatalf("Create 后应落盘 %s: %v", persistFile, err)
	}

	// 模拟重启：新建 ShareStore，应恢复该链接。
	ss2 := NewShareStore(slog.Default())
	ss2.EnablePersist(dir)
	got := ss2.Peek(link.Token)
	if got == nil {
		t.Fatalf("重启后应恢复分享链接 %s", link.Token)
	}
	if got.TenantID != "alice" || got.Rel != "user/a.txt" {
		t.Fatalf("恢复链接字段不一致: tenant=%q rel=%q", got.TenantID, got.Rel)
	}

	// Revoke 后落盘删除，重启不再恢复。
	if err := ss2.Revoke(link.Token, ""); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := os.Stat(persistFile); !os.IsNotExist(err) {
		t.Fatalf("Revoke 后落盘应删除 %s: %v", persistFile, err)
	}
	ss3 := NewShareStore(slog.Default())
	ss3.EnablePersist(dir)
	if got := ss3.Peek(link.Token); got != nil {
		t.Fatalf("Revoke 后重启不应再恢复 %s", link.Token)
	}
}

// TestShare_Persist_SkipExpiredOnRestore 验证重启恢复时跳过已过期链接。
func TestShare_Persist_SkipExpiredOnRestore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ss := NewShareStore(slog.Default())
	ss.EnablePersist(dir)
	// TTL 极短：恢复时已过期。
	link, err := ss.Create("b.txt", "bob", "user/b.txt", "ak-2", -time.Millisecond, 0, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ss2 := NewShareStore(slog.Default())
	ss2.EnablePersist(dir)
	if got := ss2.Peek(link.Token); got != nil {
		t.Fatalf("过期链接重启后不应恢复: %s", link.Token)
	}
}

// TestShare_Persist_ConsumePersists 验证 Consume（一次性/计数）变更落盘，重启反映计数。
func TestShare_Persist_ConsumePersists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ss := NewShareStore(slog.Default())
	ss.EnablePersist(dir)
	// 不限次数（MaxDownloads=0）普通分享：Consume 只递增计数。
	link, err := ss.Create("c.txt", "carol", "user/c.txt", "ak-3", time.Hour, 0, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 同一 token 在旧实例 Consume 一次。
	if got := ss.Consume(link.Token); got == nil {
		t.Fatal("Consume 应成功")
	}

	// 重启后计数保留：Peek 的 Downloads==1。
	ss2 := NewShareStore(slog.Default())
	ss2.EnablePersist(dir)
	got := ss2.Peek(link.Token)
	if got == nil {
		t.Fatalf("重启后应恢复 %s", link.Token)
	}
	if got.Downloads != 1 {
		t.Fatalf("重启后 Downloads=%d want 1（Consume 计数持久化）", got.Downloads)
	}
}

// TestShare_Persist_DisabledByDefault 验证 EnablePersist 未调用时纯内存（无落盘、无恢复）。
func TestShare_Persist_DisabledByDefault(t *testing.T) {
	t.Parallel()
	ss := NewShareStore(slog.Default())
	link, err := ss.Create("d.txt", "dan", "user/d.txt", "ak-4", time.Hour, 0, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 无 persistDir：不写盘。
	ss2 := NewShareStore(slog.Default())
	if got := ss2.Peek(link.Token); got != nil {
		t.Fatal("未启用持久化时新实例不应恢复")
	}
}

// TestShare_Persist_EvictionRemovesFiles 验证淘汰（过期清理/容量淘汰）同步删除落盘文件。
func TestShare_Persist_EvictionRemovesFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ss := NewShareStore(slog.Default())
	ss.EnablePersist(dir)

	// 创建 3 条 TTL 极短链接。
	var tokens []string
	for i := range 3 {
		l, err := ss.Create(fmt.Sprintf("e%d.txt", i), "eve", fmt.Sprintf("user/e%d.txt", i), "ak-5", -time.Millisecond, 0, false)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		tokens = append(tokens, l.Token)
	}

	// 触发 cleanupExpired（等价于 Stop 前的清理）：过期条目删除且落盘删除。
	ss.cleanupExpired()
	for _, tok := range tokens {
		if _, err := os.Stat(filepath.Join(dir, tok+".json")); !os.IsNotExist(err) {
			t.Fatalf("过期清理后落盘应删除 %s: %v", tok, err)
		}
	}
	ss.Stop()
}

// assemblyShareTest 手工装配带持久化的分享服务（复用 RegisterRoutes），返回 URL 与 Handlers。
// storageRoot 由调用方固定（多实例共享同一根目录以模拟重启恢复）。
func assemblyShareTest(t *testing.T, storageRoot string) (string, *Handlers) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = storageRoot
	cfg.LogLevel = "error"
	cfg.Addr = "127.0.0.1:0"

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test-version",
		BuildAt:               "test-buildat",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close() })
	return ts.URL, h
}

// TestShare_Persist_AssemblyRestartRestores 验证**装配级**持久化：真实 RegisterRoutes
// 装配创建分享后落盘；重建装配（模拟重启）从同一 storage_root 恢复，分享 token 仍可访问。
func TestShare_Persist_AssemblyRestartRestores(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := []byte("persisted share content")

	// 第一次装配：上传 + 创建分享，然后关服（Handlers.Close）。
	url, h1 := assemblyShareTest(t, root)
	uploadFile(t, url, "p.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})
	reqBody := `{"filename":"p.txt","ttl":"1h"}`
	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建分享应 200, got %d", resp.StatusCode)
	}
	var shareResp map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&shareResp); err != nil {
		t.Fatal(err)
	}
	token := shareResp["token"].(string)
	if closeErr := h1.Close(); closeErr != nil {
		t.Fatalf("停服: %v", closeErr)
	}

	// 第二次装配（同一根目录）：分享 token 应恢复并可访问（内容一致）。
	url2, h2 := assemblyShareTest(t, root)
	defer func() { _ = h2.Close() }()
	resp2, err := http.Get(url2 + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("重启后分享应可访问, got %d", resp2.StatusCode)
	}
	data, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(body) {
		t.Fatalf("重启后分享内容不一致: got %q want %q", data, body)
	}
}
