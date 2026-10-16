// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_local_backend_test.go —— 本地卷**统一后端**（M1a）回归。
//
// 设计：`docs/designs/2026-10-16-quota-volume-layer.md` §11。本片只提供实现、**不注册**
// ⇒ 零行为变更（`NewBackend` 仍守卫 local、`BackendSchemas` 仍不含 local）。
//
// 覆盖：
//  1. 明文卷：`Kind == KindStorage` + `FS()` 读写往返（键空间 = 卷根相对）；
//  2. **加密卷**：经同一 `OpenLocalVolumeRoot` ⇒ 磁盘**密文**、读回**明文**（证明复用而非复制
//     了加密逻辑 ✓）；
//  3. 接缝：实例同时满足 `registry.ExternalBackend` 与 `registry.StorageVolume`（编译期已在
//     实现文件断言，此处运行期复核 ✓）。
//
// 变异：`Kind()` 改返回 `KindWrapper` ⇒ 断言红；`NewLocalBackend` 绕过 `OpenLocalVolumeRoot`
// （自行 `storage.OpenRoot`）⇒ 加密卷用例红（key 未注入 ⇒ 明文落盘）。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

func TestLocalBackend_PlainVolumeRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	be, err := NewLocalBackend(volume.Volume{Name: "v1"}, dir, testLogger())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	if be.Name() != "v1" {
		t.Fatalf("Name = %q, want v1", be.Name())
	}
	if be.Kind() != volume.KindStorage {
		t.Fatalf("Kind = %v, want KindStorage（本地卷承载真实字节 ⇒ 需配额层）", be.Kind())
	}
	fs := be.FS()
	if fs == nil {
		t.Fatal("FS() 不应为 nil")
	}
	ctx := context.Background()
	if werr := fs.WriteFile(ctx, "user/a.txt", strings.NewReader("hello"), 5, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, oerr := fs.OpenRead(ctx, "user/a.txt")
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	defer func() { _ = rc.Close() }()
	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if string(got) != "hello" {
		t.Fatalf("往返内容 = %q, want hello", got)
	}
	if be.Root() == nil {
		t.Fatal("Root() 过渡期访问器不应为 nil")
	}
}

func TestLocalBackend_EncryptedVolumeCiphertextOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "enc.key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	be, err := NewLocalBackend(volume.Volume{Name: "enc", Extra: map[string]any{
		"encrypt":          true,
		"encrypt_key_file": keyFile,
	}}, dir, testLogger())
	if err != nil {
		t.Fatalf("NewLocalBackend(加密): %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	const body = "top secret body"
	fs := be.FS()
	ctx := context.Background()
	if werr := fs.WriteFile(ctx, "user/s.txt", strings.NewReader(body), int64(len(body)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, "user", "s.txt"))
	if rerr != nil {
		t.Fatalf("读原始文件: %v", rerr)
	}
	if bytes.Contains(raw, []byte("top secret")) {
		t.Fatal("加密卷必须**密文**落盘（说明未复用 OpenLocalVolumeRoot 的加密注入）")
	}
	rc, oerr := fs.OpenRead(ctx, "user/s.txt")
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Fatalf("读回明文 = %q, want %q", got, body)
	}
}

// TestLocalBackend_SatisfiesUnifiedContracts 运行期复核接缝（编译期断言在实现文件内）。
func TestLocalBackend_SatisfiesUnifiedContracts(t *testing.T) {
	t.Parallel()
	be, err := NewLocalBackend(volume.Volume{Name: "v2"}, t.TempDir(), testLogger())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	var sv registry.StorageVolume = be
	var eb registry.ExternalBackend = be
	if sv.FS() == nil || eb.FS() == nil {
		t.Fatal("两侧 FS 视图都应可用")
	}
	if sv.Name() != eb.(interface{ Name() string }).Name() {
		t.Fatal("两接口应指向同一实例的同一卷名")
	}
}

// ---- 设计 §9 片 2：物理足迹 / 写前上界（加法式 ✓）----
//
// 键约定（`Location.FSPath()` 同口径 ✓）：卷内键 = `<归一 owner>/<bucket>/<path>` ⇒ 用户可见
// `(owner, rel)` 与卷内键的映射**只在卷里**（调用方不得自行拼接 ✗ —— 那正是「单位/归属漂移」根因）。
// 故本用例直接以**全键**写入原始卷，再断言 `(owner, rel)` 视图测得同一对象 ✓。

// TestLocalBackend_FootprintIsDiskTruth 断言明文卷足迹 = **磁盘真值**，且归属按 owner 前缀。
func TestLocalBackend_FootprintIsDiskTruth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	be, err := NewLocalBackend(volume.Volume{Name: "v1"}, dir, testLogger())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	const body = "0123456789"
	ctx := context.Background()
	if werr := be.FS().WriteFile(ctx, "alice/user/a.txt", strings.NewReader(body), int64(len(body)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	raw, serr := os.Stat(filepath.Join(dir, "alice", "user", "a.txt"))
	if serr != nil {
		t.Fatalf("原始文件 stat: %v", serr)
	}
	n, ok := be.Footprint("alice", "user/a.txt")
	if !ok {
		t.Fatal("Footprint 应测得")
	}
	if n != raw.Size() || n != int64(len(body)) {
		t.Fatalf("Footprint=%d，磁盘=%d，逻辑=%d（明文卷三者应相等）", n, raw.Size(), len(body))
	}
	// **归属**：另一 owner 下同名路径不存在 ⇒ 必须 ok=false（禁跨 owner 误计）。
	if _, ok := be.Footprint("bob", "user/a.txt"); ok {
		t.Fatal("跨 owner 路径必须不可得（归属按 owner 前缀解析）")
	}
	// 不存在 ⇒ ok=false（**不可得**，不得静默当 0）。
	if _, ok := be.Footprint("alice", "user/nope.txt"); ok {
		t.Fatal("不存在的对象必须 ok=false")
	}
	// owner 根目录：明文本地卷无 meta blob ⇒ 0 是**实测值**（ok=true）。
	if dn, dok := be.Footprint("alice", ""); !dok || dn != 0 {
		t.Fatalf("owner 目录 = (%d,%v)，want (0,true)", dn, dok)
	}
}

// TestLocalBackend_EncryptedFootprintIsCiphertextAndBoundDominates 断言加密卷：
// 足迹 = **密文长度**（≠ 逻辑）；写前上界 **≥** 实际足迹（G1「绝不超真实配额」的机制 ✓）。
func TestLocalBackend_EncryptedFootprintIsCiphertextAndBoundDominates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "enc.key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{9}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	be, err := NewLocalBackend(volume.Volume{Name: "enc", Extra: map[string]any{
		"encrypt":          true,
		"encrypt_key_file": keyFile,
	}}, dir, testLogger())
	if err != nil {
		t.Fatalf("NewLocalBackend(加密): %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	logical := int64(4096)
	body := bytes.Repeat([]byte{'x'}, int(logical))
	ctx := context.Background()
	if werr := be.FS().WriteFile(ctx, "alice/user/big.bin", bytes.NewReader(body), logical, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	raw, serr := os.Stat(filepath.Join(dir, "alice", "user", "big.bin"))
	if serr != nil {
		t.Fatalf("原始文件 stat: %v", serr)
	}
	n, ok := be.Footprint("alice", "user/big.bin")
	if !ok {
		t.Fatal("Footprint 应测得")
	}
	if n != raw.Size() {
		t.Fatalf("Footprint=%d，磁盘真值=%d（必须相等）", n, raw.Size())
	}
	if n <= logical {
		t.Fatalf("加密卷足迹 %d 应 > 逻辑 %d（密文长度）", n, logical)
	}
	if n != storage.EncryptedPhysicalSize(logical) {
		t.Fatalf("足迹=%d，EncryptedPhysicalSize=%d（应与落盘公式同源）", n, storage.EncryptedPhysicalSize(logical))
	}
	if bound := be.MaxPhysical("alice", "user/big.bin", logical); bound < n {
		t.Fatalf("MaxPhysical=%d 必须 ≥ 实际足迹 %d（否则预占小于真实 ⇒ 可超配额）", bound, n)
	}
	// 明文卷上界恒等（零回归）。
	plain, perr := NewLocalBackend(volume.Volume{Name: "p"}, t.TempDir(), testLogger())
	if perr != nil {
		t.Fatalf("NewLocalBackend(明文): %v", perr)
	}
	t.Cleanup(func() { _ = plain.Close() })
	if got := plain.MaxPhysical("alice", "user/x", 100); got != 100 {
		t.Fatalf("明文卷 MaxPhysical=%d，want 100（恒等）", got)
	}
}
