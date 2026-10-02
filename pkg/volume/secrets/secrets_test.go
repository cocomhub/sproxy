// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// newLocalManager 建一个本地 secrets 管理句柄（底层 t.TempDir()+"/secrets" LocalFS）。
func newLocalManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fs := syncpkg.NewLocalFS(root, nil)
	return NewManager(fs, "testvol", true), root
}

func TestCreateAndRead(t *testing.T) {
	t.Parallel()
	mgr, _ := newLocalManager(t)
	ctx := context.Background()
	key, err := mgr.Create(ctx, "mysecret")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 32B raw → 64 hex 字符。
	if len(key) != 64 {
		t.Errorf("密钥长度=%d，应为 64 hex（32B）", len(key))
	}
	for _, c := range key {
		if c < '0' || c > '9' && c < 'a' {
			t.Fatalf("密钥含非 hex 字符 %q", c)
		}
		if c > 'f' {
			t.Fatalf("密钥含非 hex 字符 %q", c)
		}
	}
	got, err := mgr.Read(ctx, "mysecret")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(key) {
		t.Errorf("读回与创建不一致")
	}
}

func TestManagerCreate_LocalFileIs0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无 POSIX 权限位")
	}
	t.Parallel()
	mgr, root := newLocalManager(t)
	ctx := context.Background()
	if _, err := mgr.Create(ctx, "ssh-key-secret"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	st, err := os.Stat(filepath.Join(root, "ssh-key-secret"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("本地 secret 权限=%o，应为 600", perm)
	}
}

func TestManagerListAndDefault(t *testing.T) {
	t.Parallel()
	mgr, _ := newLocalManager(t)
	ctx := context.Background()
	if _, err := mgr.Create(ctx, "bbb"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := mgr.Create(ctx, "aaa"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	names, err := mgr.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 || names[0] != "aaa" || names[1] != "bbb" {
		t.Errorf("List=%v（应排序）", names)
	}
	// 显式默认优先。
	def, err := mgr.SelectDefault(ctx, "bbb")
	if err != nil || def != "bbb" {
		t.Errorf("SelectDefault(bbb)=%q err=%v", def, err)
	}
	// 无显式 → 首个。
	def2, err := mgr.SelectDefault(ctx, "")
	if err != nil || def2 != "aaa" {
		t.Errorf("SelectDefault()=%q err=%v", def2, err)
	}
}

func TestManagerInvalidNames(t *testing.T) {
	t.Parallel()
	mgr, _ := newLocalManager(t)
	ctx := context.Background()
	if _, err := mgr.Create(ctx, ""); err == nil {
		t.Error("空名应报错")
	}
	if _, err := mgr.Create(ctx, "a/b"); err == nil {
		t.Error("含 / 的名应报错")
	}
	if _, err := mgr.Read(ctx, "a/b"); err == nil {
		t.Error("含 / 的名应报错")
	}
}

func TestManagerReadMissing(t *testing.T) {
	t.Parallel()
	mgr, _ := newLocalManager(t)
	ctx := context.Background()
	if _, err := mgr.Read(ctx, "nope"); err == nil {
		t.Fatal("读不存在 secret 应报错，却成功")
	}
}

func TestManagerRandomness(t *testing.T) {
	t.Parallel()
	mgr, _ := newLocalManager(t)
	ctx := context.Background()
	k1, err := mgr.Create(ctx, "k1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	k2, err := mgr.Create(ctx, "k2")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if string(k1) == string(k2) {
		t.Error("两次创建的密钥不应相同（随机性）")
	}
}

// TestOpenURL 验证 secrets backend 的 URL 能力：secrets://<卷>/<name> 读 secret；
// 非法 URL / 空名 / 不存在 fail-closed。
func TestOpenURL(t *testing.T) {
	t.Parallel()
	mgr, root := newLocalManager(t)
	_ = root
	key, err := mgr.Create(context.Background(), "datakey")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	be, err := NewBackend(context.Background(), volume.Volume{Name: "sv", Type: "secrets"}, mgr.fs)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	rc, err := be.(registry.URLResolver).OpenURL(context.Background(), "secrets://sv/datakey")
	if err != nil {
		t.Fatalf("OpenURL: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != string(key) {
		t.Errorf("OpenURL 内容 = %q, want %q", got, key)
	}
	// fail-closed：非法 scheme / 空名 / 不存在。
	if _, err := be.(registry.URLResolver).OpenURL(context.Background(), "http://sv/datakey"); err == nil {
		t.Error("非 secrets scheme 应失败")
	}
	if _, err := be.(registry.URLResolver).OpenURL(context.Background(), "secrets://sv/"); err == nil {
		t.Error("空 secret 名应失败")
	}
	if _, err := be.(registry.URLResolver).OpenURL(context.Background(), "secrets://sv/nope"); err == nil {
		t.Error("不存在 secret 应失败")
	}
}
