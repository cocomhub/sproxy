// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// state_ledger_test.go 验证 checksum/dedup 台账的 StateStore 适配装配（statestore.md
// §5.1 P0 / cluster-state-migration.md §2.2）：
//   - Handlers 装配 stateStore（SetStateStore）后 checksumStoreFor/dedupStoreFor 返回
//     StateStore 后端适配器（本地 JSON 仅作回退读；首写迁 StateStore 新路径）；
//   - 未装配（默认）零回归：返回既有本地 JSON 实现（ChecksumStore / DedupStore 本地形态）。

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/state"
)

// TestHandlers_StateBackedChecksumStore 验证 stateStore 装配后 checksumStoreFor 返回
// StateStore 适配器：读写经 StateStore 落盘；旧 meta 文件仅回退读。
func TestHandlers_StateBackedChecksumStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	h.SetStateStore(st)

	cs := h.checksumStoreFor("alice")
	sb, ok := cs.(*checksum.StateBackedChecksumStore)
	if !ok {
		t.Fatalf("stateStore 装配后 checksumStoreFor(alice) 应为 *StateBackedChecksumStore, got %T", cs)
	}
	if err := sb.Flush(); err != nil { // 写经合并窗口
		t.Fatalf("flush: %v", err)
	}
	cs.Set("user/dir/f.txt", "sha256hex")
	if err := sb.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got, ok := cs.Get("user/dir/f.txt"); !ok || got != "sha256hex" {
		t.Fatalf("checksum 读写失败 got=%q ok=%v", got, ok)
	}
	if _, gerr := st.Get(context.Background(), "checksum/alice/all"); gerr != nil {
		t.Fatalf("写应落 StateStore 新路径: %v", gerr)
	}
}

// TestHandlers_StateBackedChecksumStore_LegacyFallback 验证 stateStore 装配后
// checksumStoreFor 回退读旧 <meta>/checksums.json（迁移前存量零丢失）。
func TestHandlers_StateBackedChecksumStore_LegacyFallback(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "alice", "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(root, "alice", "meta", "checksums.json")
	if err := os.WriteFile(legacyPath, []byte(`{"user/a.txt":"legacy-cs"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	h := newAssemblyTestHandlers(t, root)
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	h.SetStateStore(st)

	cs := h.checksumStoreFor("alice")
	if got, ok := cs.Get("user/a.txt"); !ok || got != "legacy-cs" {
		t.Fatalf("回退读旧 meta 失败 got=%q ok=%v", got, ok)
	}
}

// TestHandlers_StateBackedDedupStore 验证 stateStore 装配后 dedupStoreFor 返回 StateStore
// 后端（dedup 启用时）：Add 引用经 StateStore 落盘。
func TestHandlers_StateBackedDedupStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = root
	cfg.Dedup.Enabled = true
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	h := newAssemblyTestHandlers(t, root)
	h.cfgPtr = &cfgPtr
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	h.SetStateStore(st)

	ds := h.dedupStoreFor("alice")
	if ds == nil {
		t.Fatal("dedup 启用时 dedupStoreFor(alice) 不应为 nil")
	}
	if ds.Add("user/a.txt", "vol0", "abc123") != true {
		t.Fatal("首个引用 Add 应返回 true")
	}
	if _, gerr := st.Get(context.Background(), "dedup/alice/all"); gerr != nil {
		t.Fatalf("写应落 StateStore 新路径: %v", gerr)
	}
	// 内部状态后端已装配（非本地 JSON 形态）。
	if ds.StateBackend() == nil {
		t.Fatal("stateStore 装配后 DedupStore 应带 StateStore 后端")
	}
}

// TestHandlers_LedgerNoStateStore_ZeroRegression 验证未装配 stateStore（默认）零回归：
// checksumStoreFor 返回既有 *ChecksumStore（本地 JSON 落 <meta>/checksums.json），
// dedupStoreFor 返回本地 JSON 形态 DedupStore（StateBackend nil）。
func TestHandlers_LedgerNoStateStore_ZeroRegression(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = root
	cfg.Dedup.Enabled = true
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	h := newAssemblyTestHandlers(t, root)
	h.cfgPtr = &cfgPtr

	cs := h.checksumStoreFor("alice")
	if _, ok := cs.(*checksum.ChecksumStore); !ok {
		t.Fatalf("未装配 stateStore 应返回 *ChecksumStore, got %T", cs)
	}
	ds := h.dedupStoreFor("alice")
	if ds == nil {
		t.Fatal("dedup 启用时 dedupStoreFor 不应为 nil")
	}
	if ds.StateBackend() != nil {
		t.Fatal("未装配 stateStore 时 DedupStore 不应带 StateStore 后端（本地 JSON 零回归）")
	}
}

// TestHandlers_StateBackedLedger_KeyPerOwner 验证 StateStore 后端按 owner 分 key：
// alice 与 bob 的台账互不可见（owner 隔离）。
func TestHandlers_StateBackedLedger_KeyPerOwner(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	h.SetStateStore(st)

	aliceCS := h.checksumStoreFor("alice")
	aliceCS.Set("user/a.txt", "alice-cs")
	if f, ok := aliceCS.(checksum.Flusher); ok {
		if err := f.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	if _, ok := h.checksumStoreFor("bob").Get("user/a.txt"); ok {
		t.Fatal("bob 不应看到 alice 的 checksum 记录（owner 隔离）")
	}
	if got, ok := aliceCS.Get("user/a.txt"); !ok || got != "alice-cs" {
		t.Fatalf("alice 记录应保留 got=%q ok=%v", got, ok)
	}
}

// TestHandlers_StateBackedChecksumStore_CachedPerOwner 钉住集群后端台账的**实例缓存**：
// 未缓存时每次 checksumStoreFor 都新建 ⇒ 每次首读从 StateStore 全量拉回 + 反序列化
// （O(全库) KV 读），且写合并器随实例丢弃而失效。
//
// 变异：stateStore 分支改为每次 New（不查缓存）⇒ 本用例红。
func TestHandlers_StateBackedChecksumStore_CachedPerOwner(t *testing.T) {
	t.Parallel()
	h := newAssemblyTestHandlers(t, t.TempDir())
	h.SetStateStore(state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger()))

	a1 := h.checksumStoreFor("alice")
	a2 := h.checksumStoreFor("alice")
	if a1 == nil || a1 != a2 {
		t.Fatal("集群后端 checksum 台账必须按 owner 缓存（否则每请求全量读 StateStore）")
	}
	if h.checksumStoreFor("bob") == a1 {
		t.Fatal("不同 owner 不得共用台账实例")
	}
}

// TestHandlers_DedupStoreFor_CachedInStateMode 钉住集群后端 dedup 台账的实例缓存：
// 未缓存时每次 dedupStoreFor 都新建 ⇒ 每次首读从 StateStore 全量拉回 + 反序列化，
// 且写入落在随后被丢弃的实例上。
//
// 变异：stateStore 分支改为每次 New（不查缓存）⇒ 本用例红。
func TestHandlers_DedupStoreFor_CachedInStateMode(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = root
	cfg.Dedup.Enabled = true
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	h := newAssemblyTestHandlers(t, root)
	h.cfgPtr = &cfgPtr
	h.SetStateStore(state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger()))

	d1 := h.dedupStoreFor("alice")
	d2 := h.dedupStoreFor("alice")
	if d1 == nil || d1 != d2 {
		t.Fatal("集群后端 dedup 台账必须按 owner 缓存（否则每请求全量读 StateStore）")
	}
	if h.dedupStoreFor("bob") == d1 {
		t.Fatal("不同 owner 不得共用 dedup 台账实例")
	}
}
