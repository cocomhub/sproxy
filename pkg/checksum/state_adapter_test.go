// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

// state_adapter_test.go 验证 checksum 台账的 StateStore 适配（statestore.md §5.1 P0）：
//   - 适配器往返：Set/Delete/Rename/DeletePrefix/GetAll 全流程经 StateStore 落盘与还原
//     （接口 ChecksumStoreIface 不变，磁盘字节与既有 <meta>/checksums.json JSON 逐字一致）；
//   - 双读单写（零回归铁律）：StateStore 未命中回退读旧 <meta>/checksums.json；首写恒写
//     StateStore 新路径（旧 meta 不再改写）。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
)

// stateTestLogger 返回丢弃日志的 logger（本包测试专用）。
func stateTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// writeLegacyChecksumsFile 在 metaDir 写旧 <meta>/checksums.json（map JSON 格式）。
func writeLegacyChecksumsFile(t *testing.T, metaDir string, m map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(metaDir, "checksums.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestStateBackedChecksumStore_RoundTrip 验证适配器全流程往返（Set/Get/Rename/DeletePrefix/
// Delete/GetAll 全部经 StateStore 持久化，重载后状态一致）。
func TestStateBackedChecksumStore_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), stateTestLogger())
	s := NewStateBackedChecksumStore(st, "checksum/alice/all", filepath.Join(t.TempDir(), "checksums.json"), stateTestLogger())

	s.Set("user/dir/f.txt", "sha256hex")
	s.Set("user/other.txt", "other")
	s.Set("user/dir/sub/g.txt", "sub")
	if got, ok := s.Get("user/dir/f.txt"); !ok || got != "sha256hex" {
		t.Fatalf("Get(user/dir/f.txt)=%q,%v want sha256hex,true", got, ok)
	}

	// StateStore key 已落盘（单写：迁移后不写旧 meta）。
	if _, gerr := st.Get(ctx, "checksum/alice/all"); gerr != nil {
		t.Fatalf("StateStore 应已有快照值: %v", gerr)
	}
	// 值格式与既有 map JSON 一致（顶层对象，key = rel）。
	raw, gerr := st.Get(ctx, "checksum/alice/all")
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	var m map[string]string
	if jerr := json.Unmarshal(raw, &m); jerr != nil {
		t.Fatalf("StateStore 值不是 map JSON: %v", jerr)
	}
	if len(m) != 3 || m["user/dir/f.txt"] != "sha256hex" {
		t.Fatalf("StateStore 快照内容不符: %+v", m)
	}

	// 重载：新适配器从 StateStore 读回（不回退旧文件）。
	s2 := NewStateBackedChecksumStore(st, "checksum/alice/all", filepath.Join(t.TempDir(), "missing", "checksums.json"), stateTestLogger())
	if got, ok := s2.Get("user/dir/f.txt"); !ok || got != "sha256hex" {
		t.Fatalf("重载 Get(user/dir/f.txt)=%q,%v", got, ok)
	}

	// Rename：from → to（to 已存在被覆盖）。
	s2.Rename("user/dir/f.txt", "user/renamed.txt")
	if _, ok := s2.Get("user/dir/f.txt"); ok {
		t.Fatal("Rename 后旧 key 不应存在")
	}
	if got, ok := s2.Get("user/renamed.txt"); !ok || got != "sha256hex" {
		t.Fatalf("Rename 后新 key=%q,%v", got, ok)
	}

	// DeletePrefix：删 user/dir/ 前缀（只删子目录）。
	s2.DeletePrefix("user/dir/")
	if _, ok := s2.Get("user/dir/sub/g.txt"); ok {
		t.Fatal("DeletePrefix 后子文件不应存在")
	}
	if _, ok := s2.Get("user/other.txt"); !ok {
		t.Fatal("DeletePrefix 不应误删前缀外记录")
	}
	// Delete 单条。
	s2.Delete("user/other.txt")
	if all := s2.GetAll(); len(all) != 1 || all["user/renamed.txt"] != "sha256hex" {
		t.Fatalf("最终 GetAll=%+v", all)
	}

	// 再次重载（全流程落盘后状态可恢复）。
	s3 := NewStateBackedChecksumStore(st, "checksum/alice/all", "", stateTestLogger())
	if all := s3.GetAll(); len(all) != 1 || all["user/renamed.txt"] != "sha256hex" {
		t.Fatalf("二次重载 GetAll=%+v", all)
	}
}

// TestStateBackedChecksumStore_LegacyFallback 验证双读：StateStore 未命中 → 回退读旧
// <meta>/checksums.json 可载入（迁移前存量零丢失）。
func TestStateBackedChecksumStore_LegacyFallback(t *testing.T) {
	t.Parallel()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), stateTestLogger())
	legacyPath := writeLegacyChecksumsFile(t, filepath.Join(t.TempDir(), "anonymous", "meta"), map[string]string{
		"user/a.txt": "legacy-cs",
	})

	s := NewStateBackedChecksumStore(st, "checksum/alice/all", legacyPath, stateTestLogger())
	if got, ok := s.Get("user/a.txt"); !ok || got != "legacy-cs" {
		t.Fatalf("回退载入 Get(user/a.txt)=%q,%v want legacy-cs,true", got, ok)
	}
	if all := s.GetAll(); len(all) != 1 || all["user/a.txt"] != "legacy-cs" {
		t.Fatalf("回退载入 GetAll=%+v", all)
	}
}

// TestStateBackedChecksumStore_SaveMigratesToState 验证单写：回退载入后首次写恒写
// StateStore 新路径（迁移发生，旧 meta 文件不被改写）。
func TestStateBackedChecksumStore_SaveMigratesToState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	metaDir := filepath.Join(t.TempDir(), "anonymous", "meta")
	legacyPath := writeLegacyChecksumsFile(t, metaDir, map[string]string{
		"user/a.txt": "legacy-cs",
	})
	legacyRaw, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}

	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), stateTestLogger())
	s := NewStateBackedChecksumStore(st, "checksum/alice/all", legacyPath, stateTestLogger())
	if _, ok := s.Get("user/a.txt"); !ok {
		t.Fatal("回退载入应命中")
	}
	// 首写 → StateStore；旧 meta 文件保持原内容不被改写（单写）。
	s.Set("user/b.txt", "new-cs")
	if _, gerr := st.Get(ctx, "checksum/alice/all"); gerr != nil {
		t.Fatalf("首写应写 StateStore 新路径: %v", gerr)
	}
	cur, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(string(cur), string(legacyRaw)) {
		t.Fatalf("旧 meta 文件不应被改写（单写语义）")
	}
}
