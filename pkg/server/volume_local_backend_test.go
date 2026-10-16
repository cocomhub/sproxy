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
