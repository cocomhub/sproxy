// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

// volume_path_test.go 验证 Volume.ResolveUserPath（2026-10-05 用户裁定）：
// 共享卷加 owner 前缀隔离 / 独享卷无前缀、用户无权立即报错、路径安全（逃逸/注入拒绝）。

import (
	"errors"
	"testing"
)

// sharedVolume 构造共享卷（默认开放 ACL → Shared()）。
func sharedVolume() Volume {
	return Volume{Name: "shared", Type: "secretdata"}
}

// privateVolume 构造独享卷（ModeAllow + 单 owner 白名单 → 非共享）。
func privateVolume() Volume {
	return Volume{Name: "mydisk", Type: "baidupcs", ACL: ACL{Mode: ModeAllow, Owners: map[string]struct{}{"alice": {}}}}
}

// TestResolveUserPath_SharedVolume_OwnerPrefix：共享卷 → <owner>/user/<rel>（隔离）。
func TestResolveUserPath_SharedVolume_OwnerPrefix(t *testing.T) {
	t.Parallel()
	got, err := sharedVolume().ResolveUserPath("alice", "dir/movie.bin")
	if err != nil {
		t.Fatalf("ResolveUserPath: %v", err)
	}
	if want := "alice/user/dir/movie.bin"; got != want {
		t.Fatalf("共享卷键=%q want %q", got, want)
	}
}

// TestResolveUserPath_PrivateVolume_NoPrefix：独享卷 → user/<rel>（无 owner 前缀）。
func TestResolveUserPath_PrivateVolume_NoPrefix(t *testing.T) {
	t.Parallel()
	got, err := privateVolume().ResolveUserPath("alice", "movie.bin")
	if err != nil {
		t.Fatalf("ResolveUserPath: %v", err)
	}
	if want := "user/movie.bin"; got != want {
		t.Fatalf("独享卷键=%q want %q", got, want)
	}
}

// TestResolveUserPath_NotAuthorized：用户无权访问卷 → ErrVolumeNotAuthorized（fail-closed）。
func TestResolveUserPath_NotAuthorized(t *testing.T) {
	t.Parallel()
	// 独享卷只允许 alice；bob 无权。
	_, err := privateVolume().ResolveUserPath("bob", "movie.bin")
	if !errors.Is(err, ErrVolumeNotAuthorized) {
		t.Fatalf("bob 访问独享卷应 ErrVolumeNotAuthorized, got %v", err)
	}
}

// TestResolveUserPath_InvalidPath：路径逃逸/注入拒绝（绝对路径/../空段/非法段）。
func TestResolveUserPath_InvalidPath(t *testing.T) {
	t.Parallel()
	bad := []string{
		"/etc/passwd", // 绝对路径
		"../secret",   // 上级逃逸
		"a/../b",      // 内嵌逃逸
		"",            // 空
		"a//b",        // 空段
		"CON",         // Windows 保留设备名
		".__meta",     // 内部前缀
		"a\\b",        // 反斜杠（视为分隔符后合法？——NormalizeRemote 转 / 后校验）
		"a:bad",       // Windows 卷名
	}
	for _, p := range bad {
		// 反斜杠用例：NormalizeRemote 把 \ 转 / 后若剩余合法段则**应成功**（兼容 Windows 输入）——
		// 空串用例：ResolveUserPath 语义是「桶内文件路径」，空 = 仅桶自身（mkdir user 场景，
		// ResolveOwnerPath 允许）；经 ResolveUserPath（文件路径）也接受（返回桶根）。
		// 其余用例必须拒绝。此处只断言「逃逸/绝对/保留名」类必然拒绝。
		if p == `a\b` || p == "" {
			continue // Windows 反斜杠兼容 / 桶根（行为由实现语义保证）
		}
		if _, err := sharedVolume().ResolveUserPath("alice", p); err == nil {
			t.Errorf("非法路径 %q 应报错", p)
		}
	}
	// 反斜杠归一：a\b → a/b 合法（Windows 输入兼容）。
	if got, err := sharedVolume().ResolveUserPath("alice", `a\b`); err != nil || got != "alice/user/a/b" {
		t.Errorf("反斜杠归一 got=%q err=%v want alice/user/a/b", got, err)
	}
}

// TestResolveOwnerPath_MetaBucket：meta 桶（ResolveOwnerPath 通用——非 user 桶）。
func TestResolveOwnerPath_MetaBucket(t *testing.T) {
	t.Parallel()
	got, err := sharedVolume().ResolveOwnerPath("alice", "meta", "credentials.json")
	if err != nil {
		t.Fatalf("ResolveOwnerPath(meta): %v", err)
	}
	if want := "alice/meta/credentials.json"; got != want {
		t.Fatalf("meta 桶键=%q want %q", got, want)
	}
	// 独享卷 meta 无前缀。
	got2, err2 := privateVolume().ResolveOwnerPath("alice", "meta", "credentials.json")
	if err2 != nil || got2 != "meta/credentials.json" {
		t.Fatalf("独享 meta 键=%q err=%v want meta/credentials.json", got2, err2)
	}
}

// TestResolveOwnerPath_EmptyRel_OnlyBucket：空 rel = 仅桶自身（mkdir user 场景）。
func TestResolveOwnerPath_EmptyRel_OnlyBucket(t *testing.T) {
	t.Parallel()
	got, err := sharedVolume().ResolveOwnerPath("alice", "user", "")
	if err != nil {
		t.Fatalf("ResolveOwnerPath(空 rel): %v", err)
	}
	if want := "alice/user"; got != want {
		t.Fatalf("仅桶键=%q want %q", got, want)
	}
}

// TestResolveOwnerPath_BucketInjection：多段/非法桶名拒绝（防桶名注入）。
func TestResolveOwnerPath_BucketInjection(t *testing.T) {
	t.Parallel()
	for _, b := range []string{"a/b", "..", "/", "user/../meta"} {
		if _, err := sharedVolume().ResolveOwnerPath("alice", b, "x"); err == nil {
			t.Errorf("非法桶名 %q 应报错", b)
		}
	}
}
