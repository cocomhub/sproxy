// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package share

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestShare_Persist_RestartRestores 验证 ShareStore 持久化：Create 后落盘，新建
// ShareStore（模拟重启）从同一目录恢复未过期链接；Consume/Revoke 后落盘删除。
func TestShare_Persist_RestartRestores(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ss := NewStore(slog.Default())
	ss.EnablePersist(testRootAt(t, dir), "")
	link, err := ss.Create(Spec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", Owner: "ak-1", TTL: time.Hour, MaxDownloads: 0, OneTime: false, ReadOnly: false, WatermarkSeed: ""})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 落盘文件存在（<token>.json）。
	persistFile := filepath.Join(dir, link.Token+".json")
	if _, err := os.Stat(persistFile); err != nil {
		t.Fatalf("Create 后应落盘 %s: %v", persistFile, err)
	}

	// 模拟重启：新建 ShareStore，应恢复该链接。
	ss2 := NewStore(slog.Default())
	ss2.EnablePersist(testRootAt(t, dir), "")
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
	ss3 := NewStore(slog.Default())
	ss3.EnablePersist(testRootAt(t, dir), "")
	if ss3.Peek(link.Token) != nil {
		t.Fatalf("Revoke 后重启不应再恢复 %s", link.Token)
	}
}

// TestShare_Persist_SkipExpiredOnRestore 验证重启恢复时跳过已过期链接。
func TestShare_Persist_SkipExpiredOnRestore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ss := NewStore(slog.Default())
	ss.EnablePersist(testRootAt(t, dir), "")
	// TTL 极短：恢复时已过期。
	link, err := ss.Create(Spec{Filename: "b.txt", TenantID: "bob", Rel: "user/b.txt", Owner: "ak-2", TTL: -time.Millisecond, MaxDownloads: 0, OneTime: false, ReadOnly: false, WatermarkSeed: ""})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ss2 := NewStore(slog.Default())
	ss2.EnablePersist(testRootAt(t, dir), "")
	if ss2.Peek(link.Token) != nil {
		t.Fatalf("过期链接重启后不应恢复: %s", link.Token)
	}
}

// TestShare_Persist_ConsumePersists 验证 Consume（一次性/计数）变更落盘，重启反映计数。
func TestShare_Persist_ConsumePersists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ss := NewStore(slog.Default())
	ss.EnablePersist(testRootAt(t, dir), "")
	// 不限次数（MaxDownloads=0）普通分享：Consume 只递增计数。
	link, err := ss.Create(Spec{Filename: "c.txt", TenantID: "carol", Rel: "user/c.txt", Owner: "ak-3", TTL: time.Hour, MaxDownloads: 0, OneTime: false, ReadOnly: false, WatermarkSeed: ""})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 同一 token 在旧实例 Consume 一次。
	if ss.Consume(link.Token) == nil {
		t.Fatal("Consume 应成功")
	}

	// 重启后计数保留：Peek 的 Downloads==1。
	ss2 := NewStore(slog.Default())
	ss2.EnablePersist(testRootAt(t, dir), "")
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
	ss := NewStore(slog.Default())
	link, err := ss.Create(Spec{Filename: "d.txt", TenantID: "dan", Rel: "user/d.txt", Owner: "ak-4", TTL: time.Hour, MaxDownloads: 0, OneTime: false, ReadOnly: false, WatermarkSeed: ""})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 无 persistDir：不写盘。
	ss2 := NewStore(slog.Default())
	if ss2.Peek(link.Token) != nil {
		t.Fatal("未启用持久化时新实例不应恢复")
	}
}

// TestShare_Persist_EvictionRemovesFiles 验证淘汰（过期清理/容量淘汰）同步删除落盘文件。
func TestShare_Persist_EvictionRemovesFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ss := NewStore(slog.Default())
	ss.EnablePersist(testRootAt(t, dir), "")

	// 创建 3 条 TTL 极短链接。
	var tokens []string
	for i := range 3 {
		l, err := ss.Create(Spec{Filename: fmt.Sprintf("e%d.txt", i), TenantID: "eve", Rel: fmt.Sprintf("user/e%d.txt", i), Owner: "ak-5", TTL: -time.Millisecond, MaxDownloads: 0, OneTime: false, ReadOnly: false, WatermarkSeed: ""})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		tokens = append(tokens, l.Token)
	}

	// 触发 cleanupExpired（等价于 Stop 前的清理）：过期条目删除且落盘删除。
	ss.CleanupExpired()
	for _, tok := range tokens {
		if _, err := os.Stat(filepath.Join(dir, tok+".json")); !os.IsNotExist(err) {
			t.Fatalf("过期清理后落盘应删除 %s: %v", tok, err)
		}
	}
	ss.Stop()
}

// testRootAt 打开指定目录的存储根（测试用）。
func testRootAt(t *testing.T, dir string) *storage.Root {
	t.Helper()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("打开 Root 失败: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}
