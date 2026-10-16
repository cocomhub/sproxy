// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
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
	// M-7 回归：`.`/`..` 显式拒绝——Stat("..") 可命中上层目录条目，Exists/SelectDefault
	// 可能选中它随后 Read 失败；统一入口（Create/Read/Exists/SelectDefault）均拒。
	for _, bad := range []string{".", ".."} {
		if _, err := mgr.Create(ctx, bad); err == nil {
			t.Errorf("Create(%q) 应报错（路径穿越名）", bad)
		}
		if _, err := mgr.Read(ctx, bad); err == nil {
			t.Errorf("Read(%q) 应报错（路径穿越名）", bad)
		}
		if _, err := mgr.Exists(ctx, bad); err == nil {
			t.Errorf("Exists(%q) 应报错（路径穿越名）", bad)
		}
		if _, err := mgr.SelectDefault(ctx, bad); err == nil {
			t.Errorf("SelectDefault(%q) 应报错（路径穿越名）", bad)
		}
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

// TestManager_ChmodHookInvoked 注入 `WithLocalChmod` 回调时，本地加固必须走**回调**而非
// `*syncpkg.LocalFS` 类型断言：经 `storage.Root` 适配的 secrets 视图（加密/封印语义必需，
// 见 pkg/server/secrets_fs.go）不是 LocalFS ⇒ 断言会静默跳过，本地 0600 加固消失。
// 本用例平台无关（只断言回调被调用），可在 Windows 上守住该接线。
//
// 变异：`chmodSecret` 忽略回调（只走类型断言）→ 回调不被调用 → 红。
func TestManager_ChmodHookInvoked(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var mu sync.Mutex
	var got []string
	m := NewManager(syncpkg.NewLocalFS(root, nil), "testvol", true,
		WithLocalChmod(func(name string) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, name)
			return nil
		}))
	if _, err := m.Create(context.Background(), "k1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "k1" {
		t.Fatalf("本地加固回调应被调用一次且带 secret 名, got %v", got)
	}
}

// TestVolumeFromURL_And_IsDefaultVolumeRef 锁定 `secrets://` URL 的 **authority（卷名）解析**
// 与「是否 per-owner 默认桶」判定（同一实现的两种用法：装配层按卷名判「secret_url 是否指向封装
// 声明卷」，服务级解析按判定分流到 per-owner 桶）。
//
// 变异：把 authority 解析改回 `u.Path`（或漏掉 Opaque 兼容）→ `secrets://keys/datakey` 的卷名
// 解析不再是 keys，装配层分轮判据随之失效 ⇒ 红。
func TestVolumeFromURL_And_IsDefaultVolumeRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url       string
		wantVol   string // VolumeFromURL 期望值（"" = 空 authority）
		isDefault bool   // IsDefaultVolumeRef 期望值
		wantErr   bool   // VolumeFromURL 期望报错（非法 URL）
	}{
		{"secrets://keys/datakey", "keys", false, false},
		{"secrets://default-secrets/master", DefaultVolumeName, true, false},
		{"secrets://default/k", "default", true, false},
		// opaque 形态（无 authority）：`secrets://datakey` 经 url.Parse 落在 Opaque 段
		// （NameFromURL 亦按此形态取 secret 名，故卷名解析须与之一致，不得当成默认桶）。
		{"secrets://datakey", "datakey", false, false},
	}
	for _, tc := range cases {
		vol, err := VolumeFromURL(tc.url)
		if (err != nil) != tc.wantErr {
			t.Fatalf("VolumeFromURL(%q) err=%v wantErr=%v", tc.url, err, tc.wantErr)
		}
		if vol != tc.wantVol {
			t.Errorf("VolumeFromURL(%q)=%q want %q", tc.url, vol, tc.wantVol)
		}
		if got := IsDefaultVolumeRef(tc.url); got != tc.isDefault {
			t.Errorf("IsDefaultVolumeRef(%q)=%v want %v", tc.url, got, tc.isDefault)
		}
	}
	// 非法 URL：VolumeFromURL 报错（调用方按各自口径处理），IsDefaultVolumeRef 恒 false
	// （不得把「解析不出来」当成「默认桶」）。
	if _, err := VolumeFromURL("secrets://%zz/bad"); err == nil {
		t.Error("非法 URL 应返回错误")
	}
	if IsDefaultVolumeRef("secrets://%zz/bad") {
		t.Error("非法 URL 不得判为 per-owner 默认桶引用")
	}
}
