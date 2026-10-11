// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

import (
	"context"
	"io"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// markerFS 是 Root 感知提供者的替身（仅用于身份断言；FS 方法由嵌入接口占位）。
type markerFS struct{ syncpkg.FS }

// TestExecutor_LocalFS_PrefersRootAwareProvider 评审 P0：本地侧必须优先使用**Root 感知**FS
// 提供者（装配层注入 `NewRootFS(tenantRoot)` ⇒ at-rest 透明加解密 + 封印）；未注入时回落
// 路径式 LocalFS（测试/未接线装配零回归）。
// 变异：把 localFS 改回恒 NewLocalFS → 本用例红（加密卷上将落明文/传密文）。
func TestExecutor_LocalFS_PrefersRootAwareProvider(t *testing.T) {
	t.Parallel()
	marker := &markerFS{}
	e := &Executor{
		TenantRootFS: func(owner string) (syncpkg.FS, bool) {
			if owner == "alice" {
				return marker, true
			}
			return nil, false
		},
	}
	if got := e.localFS("/tmp/whatever", "alice"); got != syncpkg.FS(marker) {
		t.Fatalf("已注入提供者时必须使用 Root 感知 FS（不接受裸 LocalFS），got %T", got)
	}
	// provider 对未知 owner 返回 false → 回落 LocalFS（非 nil）。
	if got := e.localFS(t.TempDir(), "bob"); got == nil {
		t.Fatal("provider 未命中时应回落路径式 LocalFS")
	}
	// 未注入（nil）→ 回落 LocalFS（零回归）。
	e2 := &Executor{}
	if got := e2.localFS(t.TempDir(), "alice"); got == nil {
		t.Fatal("未注入时应回落路径式 LocalFS")
	}
}

// tenantRootFake 是「租户根」替身：ListDir 返回 <bucket>/<name> 形态（模拟 Root 的键空间）。
type tenantRootFake struct {
	syncpkg.FS
	entries []syncpkg.Entry
	read    []string
}

// ListDir 按目录**过滤**（与真实 FS 一致：条目必须是该目录的直接子项——PrefixFS 只做前缀
// 映射与剥离，不做过滤，过滤由底层 FS 提供）。
func (f *tenantRootFake) ListDir(_ context.Context, dir string) ([]syncpkg.Entry, error) {
	prefix := strings.Trim(dir, "/")
	if prefix != "" {
		prefix += "/"
	}
	out := make([]syncpkg.Entry, 0, len(f.entries))
	for _, e := range f.entries {
		if strings.HasPrefix(e.Path, prefix) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *tenantRootFake) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	for i := range f.entries {
		if f.entries[i].Path == p {
			e := f.entries[i]
			return &e, nil
		}
	}
	return nil, nil
}

func (f *tenantRootFake) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	f.read = append(f.read, p)
	return io.NopCloser(strings.NewReader("x")), nil
}

// TestExecutor_PushSourceFS_IsUserBucketScoped 评审第 2 轮 P0 回归：push 源必须是 **user 桶**
// 作用域——注入 Root 感知提供者后（返回**租户根** FS）必须叠加 `user/` 前缀，否则整租户根
// （含 meta/ 等内部桶）会被当源推送。
// 变异：pushSourceFS 去掉 PrefixFS 前缀 → 断言红（会看到 meta/ 条目）。
func TestExecutor_PushSourceFS_IsUserBucketScoped(t *testing.T) {
	t.Parallel()
	fake := &tenantRootFake{entries: []syncpkg.Entry{
		{Name: "a.txt", Path: "user/a.txt", Size: 3},
		{Name: "x.meta", Path: "meta/a.txt.meta", Size: 9},
	}}
	e := &Executor{TenantRootFS: func(string) (syncpkg.FS, bool) { return fake, true }}
	src := e.pushSourceFS("/tmp/irrelevant", "alice")

	entries, err := src.ListDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "a.txt" {
		t.Fatalf("push 源必须是 user 桶作用域（引擎键空间 = 桶内相对路径 a.txt），got %+v", entries)
	}
	for _, en := range entries {
		if strings.HasPrefix(en.Path, "meta/") || strings.HasPrefix(en.Path, "user/") {
			t.Fatalf("push 源不得包含租户根内部桶/桶前缀条目: %+v", entries)
		}
	}
	// 读也必须落在 user 桶键空间。
	rc, rerr := src.OpenRead(context.Background(), "a.txt")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	_ = rc.Close()
	if len(fake.read) != 1 || fake.read[0] != "user/a.txt" {
		t.Fatalf("读应映射到 user/a.txt, got %v", fake.read)
	}
}
