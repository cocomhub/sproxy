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
	"github.com/cocomhub/sproxy/pkg/storage"
)

// testRootAt 在 dir 上打开 storage.Root（测试统一走**卷句柄**：卷决定是否加密，
// 使用方不判加密态）；t.Cleanup 关闭。
func testRootAt(t *testing.T, dir string) *storage.Root {
	t.Helper()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("打开 Root 失败: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// readRootForTest 经卷句柄读文件（加密卷上旧台账是密文）。
func stateTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// readRootForTest 经卷句柄读文件（加密卷上旧台账是密文，裸 os 读会得到密文/失败）。
func readRootForTest(t *testing.T, root *storage.Root, rel string) ([]byte, error) {
	t.Helper()
	rc, err := root.OpenDecrypted(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// writeLegacyChecksumsFile 经**卷句柄**在 relDir 下写旧 <meta>/checksums.json
// （map JSON 格式），返回其卷内相对路径。经 Root 写 ⇒ 加密卷上旧台账是密文，
// 故回退读也必须经 Root（本文件的 encrypted 用例钉住这一点）。
func writeLegacyChecksumsFile(t *testing.T, root *storage.Root, relDir string, m map[string]string) string {
	t.Helper()
	if err := root.MkdirAll(relDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	rel := relDir + "/checksums.json"
	w, err := root.OpenFileEncrypted(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := w.Write(raw); werr != nil {
		_ = w.Close()
		t.Fatal(werr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	return rel
}

// TestStateBackedChecksumStore_RoundTrip 验证适配器全流程往返（Set/Get/Rename/DeletePrefix/
// Delete/GetAll 全部经 StateStore 持久化，重载后状态一致）。
func TestStateBackedChecksumStore_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), stateTestLogger())
	s := NewStateBackedChecksumStore(st, "checksum/alice/all", nil, "", stateTestLogger())

	s.Set("user/dir/f.txt", "sha256hex")
	s.Set("user/other.txt", "other")
	s.Set("user/dir/sub/g.txt", "sub")
	if err := s.Flush(); err != nil { // 落盘经合并窗口（≤1s），断言前显式 flush
		t.Fatalf("flush: %v", err)
	}
	testStateChecksumRoundTrip_AssertSetAndPersisted(t, ctx, st, s)

	// 重载：新适配器从 StateStore 读回（不回退旧文件）。
	s2 := NewStateBackedChecksumStore(st, "checksum/alice/all", nil, "", stateTestLogger())
	if got, ok := s2.Get("user/dir/f.txt"); !ok || got != "sha256hex" {
		t.Fatalf("重载 Get(user/dir/f.txt)=%q,%v", got, ok)
	}
	testStateChecksumRoundTrip_Rename(t, s2)
	testStateChecksumRoundTrip_Delete(t, s2)

	// 再次重载（全流程落盘后状态可恢复）——写经合并窗口，重载前显式 flush。
	if err := s2.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	s3 := NewStateBackedChecksumStore(st, "checksum/alice/all", nil, "", stateTestLogger())
	if all := s3.GetAll(); len(all) != 1 || all["user/renamed.txt"] != "sha256hex" {
		t.Fatalf("二次重载 GetAll=%+v", all)
	}
}

// testStateChecksumRoundTrip_AssertSetAndPersisted 断言初始 Set 的 Get 结果与 StateStore
// 落盘值（值格式与既有 map JSON 一致）。
func testStateChecksumRoundTrip_AssertSetAndPersisted(t *testing.T, ctx context.Context, st *state.LocalStateStore, s *StateBackedChecksumStore) {
	t.Helper()
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
}

// testStateChecksumRoundTrip_Rename 验证 Rename：from → to（to 已存在被覆盖）。
func testStateChecksumRoundTrip_Rename(t *testing.T, s *StateBackedChecksumStore) {
	t.Helper()
	// Rename：from → to（to 已存在被覆盖）。
	s.Rename("user/dir/f.txt", "user/renamed.txt")
	if _, ok := s.Get("user/dir/f.txt"); ok {
		t.Fatal("Rename 后旧 key 不应存在")
	}
	if got, ok := s.Get("user/renamed.txt"); !ok || got != "sha256hex" {
		t.Fatalf("Rename 后新 key=%q,%v", got, ok)
	}
}

// testStateChecksumRoundTrip_Delete 验证 DeletePrefix 与 Delete 单条后的最终 GetAll 状态。
func testStateChecksumRoundTrip_Delete(t *testing.T, s *StateBackedChecksumStore) {
	t.Helper()
	// DeletePrefix：删 user/dir/ 前缀（只删子目录）。
	s.DeletePrefix("user/dir/")
	if _, ok := s.Get("user/dir/sub/g.txt"); ok {
		t.Fatal("DeletePrefix 后子文件不应存在")
	}
	if _, ok := s.Get("user/other.txt"); !ok {
		t.Fatal("DeletePrefix 不应误删前缀外记录")
	}
	// Delete 单条。
	s.Delete("user/other.txt")
	if all := s.GetAll(); len(all) != 1 || all["user/renamed.txt"] != "sha256hex" {
		t.Fatalf("最终 GetAll=%+v", all)
	}
}

// TestStateBackedChecksumStore_LegacyFallback 验证双读：StateStore 未命中 → 回退读旧
// <meta>/checksums.json 可载入（迁移前存量零丢失）。
func TestStateBackedChecksumStore_LegacyFallback(t *testing.T) {
	t.Parallel()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), stateTestLogger())
	root := testRootAt(t, t.TempDir())
	legacyRel := writeLegacyChecksumsFile(t, root, "anonymous/meta", map[string]string{
		"user/a.txt": "legacy-cs",
	})

	s := NewStateBackedChecksumStore(st, "checksum/alice/all", root, legacyRel, stateTestLogger())
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
	root := testRootAt(t, t.TempDir())
	legacyRel := writeLegacyChecksumsFile(t, root, "anonymous/meta", map[string]string{
		"user/a.txt": "legacy-cs",
	})
	legacyRaw, err := readRootForTest(t, root, legacyRel)
	if err != nil {
		t.Fatal(err)
	}

	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), stateTestLogger())
	s := NewStateBackedChecksumStore(st, "checksum/alice/all", root, legacyRel, stateTestLogger())
	if _, ok := s.Get("user/a.txt"); !ok {
		t.Fatal("回退载入应命中")
	}
	// 首写 → StateStore；旧 meta 文件保持原内容不被改写（单写）。
	s.Set("user/b.txt", "new-cs")
	if ferr := s.Flush(); ferr != nil {
		t.Fatalf("flush: %v", ferr)
	}
	if _, gerr := st.Get(ctx, "checksum/alice/all"); gerr != nil {
		t.Fatalf("首写应写 StateStore 新路径: %v", gerr)
	}
	cur, err := readRootForTest(t, root, legacyRel)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(string(cur), string(legacyRaw)) {
		t.Fatalf("旧 meta 文件不应被改写（单写语义）")
	}
}
