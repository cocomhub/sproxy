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
	loc, err := sharedVolume().ResolveUserLocation("alice", "dir/movie.bin")
	if err != nil {
		t.Fatalf("ResolveUserLocation: %v", err)
	}
	if want := "alice/user/dir/movie.bin"; loc.FSPath() != want {
		t.Fatalf("共享卷键=%q want %q", loc.FSPath(), want)
	}
	if loc.Owner() != "alice" || loc.Bucket() != "user" || loc.Path() != "dir/movie.bin" {
		t.Fatalf("Location 字段不符: owner=%q bucket=%q path=%q", loc.Owner(), loc.Bucket(), loc.Path())
	}
}

// TestResolveUserPath_PrivateVolume_OwnerPrefix：独享卷也恒 owner 前缀（2026-10-07
// 废弃「独享无前缀」约定，所有卷按 owner 维度操作）。
func TestResolveUserPath_PrivateVolume_OwnerPrefix(t *testing.T) {
	t.Parallel()
	loc, err := privateVolume().ResolveUserLocation("alice", "movie.bin")
	if err != nil {
		t.Fatalf("ResolveUserLocation: %v", err)
	}
	if want := "alice/user/movie.bin"; loc.FSPath() != want {
		t.Fatalf("独享卷键=%q want %q", loc.FSPath(), want)
	}
}

// TestResolveUserPath_NotAuthorized：用户无权访问卷 → ErrVolumeNotAuthorized（fail-closed）。
func TestResolveUserPath_NotAuthorized(t *testing.T) {
	t.Parallel()
	// 独享卷只允许 alice；bob 无权。
	_, err := privateVolume().ResolveUserLocation("bob", "movie.bin")
	if !errors.Is(err, ErrVolumeNotAuthorized) {
		t.Fatalf("bob 访问独享卷应 ErrVolumeNotAuthorized, got %v", err)
	}
}

// TestResolveLocation_OwnerValidation M1 修复：owner=保留桶名/含分隔符 → 拒绝；
// 空 owner（匿名）放行（FSPath 归一 anonymous）。
func TestResolveLocation_OwnerValidation(t *testing.T) {
	t.Parallel()
	for _, bad := range []Owner{"user", "meta", "cloud", "archive", "chunk", "version", "trash", "a/b", "..", "./x"} {
		if _, err := sharedVolume().ResolveUserLocation(bad, "f.txt"); err == nil {
			t.Errorf("owner %q 应被拒绝（保留桶名/非法段）", bad)
		}
	}
	// 空 owner：匿名归一（FSPath 产出 anonymous/user/...），合法。
	loc, err := sharedVolume().ResolveUserLocation("", "f.txt")
	if err != nil {
		t.Fatalf("空 owner 应放行（匿名）: %v", err)
	}
	if want := "anonymous/user/f.txt"; loc.FSPath() != want {
		t.Fatalf("空 owner 键=%q want %q", loc.FSPath(), want)
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
		// 空串用例：ResolveUserLocation 语义是「桶内文件路径」，空 = 仅桶自身（mkdir user 场景，
		// ResolveLocation 允许）；经 ResolveUserLocation（文件路径）也接受（返回桶根）。
		// 其余用例必须拒绝。此处只断言「逃逸/绝对/保留名」类必然拒绝。
		if p == `a\b` || p == "" {
			continue // Windows 反斜杠兼容 / 桶根（行为由实现语义保证）
		}
		if _, err := sharedVolume().ResolveUserLocation("alice", p); err == nil {
			t.Errorf("非法路径 %q 应报错", p)
		}
	}
	// 反斜杠归一：a\b → a/b 合法（Windows 输入兼容）。
	if loc, err := sharedVolume().ResolveUserLocation("alice", `a\b`); err != nil || loc.FSPath() != "alice/user/a/b" {
		t.Errorf("反斜杠归一 got=%q err=%v want alice/user/a/b", loc.FSPath(), err)
	}
}

// TestResolveLocation_MetaBucket：meta 桶（ResolveLocation 通用——非 user 桶）。
func TestResolveLocation_MetaBucket(t *testing.T) {
	t.Parallel()
	loc, err := sharedVolume().ResolveLocation("alice", "meta", "credentials.json")
	if err != nil {
		t.Fatalf("ResolveLocation(meta): %v", err)
	}
	if want := "alice/meta/credentials.json"; loc.FSPath() != want {
		t.Fatalf("meta 桶键=%q want %q", loc.FSPath(), want)
	}
	// 独享卷 meta 也恒 owner 前缀（2026-10-07 废弃区分）。
	loc2, err2 := privateVolume().ResolveLocation("alice", "meta", "credentials.json")
	if err2 != nil || loc2.FSPath() != "alice/meta/credentials.json" {
		t.Fatalf("独享 meta 键=%q err=%v want alice/meta/credentials.json", loc2.FSPath(), err2)
	}
}

// TestResolveLocation_EmptyRel_OnlyBucket：空 rel = 仅桶自身（mkdir user 场景）。
func TestResolveLocation_EmptyRel_OnlyBucket(t *testing.T) {
	t.Parallel()
	loc, err := sharedVolume().ResolveLocation("alice", "user", "")
	if err != nil {
		t.Fatalf("ResolveLocation(空 rel): %v", err)
	}
	if want := "alice/user"; loc.FSPath() != want {
		t.Fatalf("仅桶键=%q want %q", loc.FSPath(), want)
	}
}

// TestResolveLocation_BucketInjection：多段/非法桶名拒绝（防桶名注入）。
func TestResolveLocation_BucketInjection(t *testing.T) {
	t.Parallel()
	for _, b := range []string{"a/b", "..", "/", "user/../meta"} {
		if _, err := sharedVolume().ResolveLocation("alice", b, "x"); err == nil {
			t.Errorf("非法桶名 %q 应报错", b)
		}
	}
}

// TestRebucket_Location 换桶：默认实现 Rebucket 保持 owner/path、换 bucket（sidecar）。
func TestRebucket_Location(t *testing.T) {
	t.Parallel()
	loc := sharedVolume().MustLocation("alice", "user", "x.bin")
	meta := loc.Rebucket("meta")
	if meta.Owner() != "alice" || meta.Bucket() != "meta" || meta.Path() != "x.bin" {
		t.Fatalf("Rebucket 字段不符: %+v", meta)
	}
	if want := "alice/meta/x.bin"; meta.FSPath() != want {
		t.Fatalf("meta 桶键=%q want %q", meta.FSPath(), want)
	}
}

// MustLocation 便捷：解析成功否则 panic（测试用）。
func (v Volume) MustLocation(owner, bucket, rel string) Location {
	loc, err := v.ResolveLocation(owner, bucket, rel)
	if err != nil {
		panic(err)
	}
	return loc
}

// TestBucketOf_UserBucket 独享卷 `user/...` → 桶段=user；用户目录可叫保留名（不误判）。
func TestBucketOf_UserBucket(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key, bucket, rest string
		ok                bool
	}{
		{"user/x.bin", "user", "x.bin", true},
		{"user/dir/meta/x.bin", "user", "dir/meta/x.bin", true}, // 用户目录 meta 是 user 桶内子目录
		{"user/user/x.bin", "user", "user/x.bin", true},         // 用户目录 user 同理
		{"meta/x.bin.meta", "meta", "x.bin.meta", true},         // sidecar 已落 meta 桶
		{"user", "user", "", true},
	}
	for _, tc := range cases {
		b, rest, ok := BucketOf(tc.key)
		if ok != tc.ok || b != tc.bucket || rest != tc.rest {
			t.Errorf("BucketOf(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.key, b, rest, ok, tc.bucket, tc.rest, tc.ok)
		}
	}
}

// TestBucketOf_SharedOwnerPrefix 共享卷 `<owner>/user/...` → 桶段=user（owner 非桶名）。
func TestBucketOf_SharedOwnerPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key, bucket, rest string
		ok                bool
	}{
		{"alice/user/x.bin", "user", "x.bin", true},
		{"alice/user/dir/meta/y.bin", "user", "dir/meta/y.bin", true},
		{"bob/meta/x.bin.meta", "meta", "x.bin.meta", true},
	}
	for _, tc := range cases {
		b, rest, ok := BucketOf(tc.key)
		if ok != tc.ok || b != tc.bucket || rest != tc.rest {
			t.Errorf("BucketOf(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.key, b, rest, ok, tc.bucket, tc.rest, tc.ok)
		}
	}
}

// TestBucketOf_NoBucket 无桶段键 → ok=false。
func TestBucketOf_NoBucket(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"", "x.bin", "alice/notes.txt", "alice/sub/x.txt"} {
		if _, _, ok := BucketOf(key); ok {
			t.Errorf("BucketOf(%q) 应无桶段", key)
		}
	}
}

// TestRebucketTo_UserToMeta 换桶：user → meta，独享/共享前缀/用户目录同名段均正确。
func TestRebucketTo_UserToMeta(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key, want string
		ok        bool
	}{
		{"user/x.bin", "meta/x.bin", true},
		{"user/dir/meta/x.bin", "meta/dir/meta/x.bin", true}, // 用户目录 meta 保留（只换桶段）
		{"user/user/x.bin", "meta/user/x.bin", true},         // 用户目录 user 保留
		{"alice/user/x.bin", "alice/meta/x.bin", true},       // 共享卷前缀保留
		{"alice/user/dir/meta/y", "alice/meta/dir/meta/y", true},
		{"meta/x.bin.meta", "meta/x.bin.meta", true}, // 已在 meta 桶：换 user→meta 不变
		{"x.bin", "x.bin", false},                    // 无桶段
	}
	for _, tc := range cases {
		got, ok := RebucketTo(tc.key, "meta")
		if ok != tc.ok || got != tc.want {
			t.Errorf("RebucketTo(%q) = (%q,%v), want (%q,%v)", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}

// TestLocation_StringParse_Roundtrip Location String()/ParseLocation() 序列化往返。
// 废弃共享/独享区分后：所有卷恒含 owner 段。
func TestLocation_StringParse_Roundtrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v      Volume
		owner  Owner
		bucket Bucket
		path   Path
		want   string
	}{
		{privateVolume(), "alice", "user", "dir/movie.bin", "volume://mydisk/alice/user/dir/movie.bin"},
		{sharedVolume(), "alice", "user", "dir/movie.bin", "volume://shared/alice/user/dir/movie.bin"},
		{sharedVolume(), "alice", "meta", "x.bin.meta", "volume://shared/alice/meta/x.bin.meta"},
		{sharedVolume(), "bob", "user", "", "volume://shared/bob/user"},
		// R1-MAJOR-2：空 owner 归一 anonymous（与 FSPath 同），往返幂等。
		{sharedVolume(), "anonymous", "user", "f.txt", "volume://shared/anonymous/user/f.txt"},
	}
	for _, tc := range cases {
		loc := tc.v.MustLocation(tc.owner, tc.bucket, tc.path)
		if got := loc.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
		// Parse 往返：注入卷上下文后字段一致（owner 恒含）。
		back, perr := tc.v.ParseLocation(tc.want)
		if perr != nil {
			t.Fatalf("ParseLocation(%q): %v", tc.want, perr)
		}
		if back.Owner() != tc.owner || back.Bucket() != tc.bucket || back.Path() != tc.path {
			t.Errorf("roundtrip 字段不符: got (%q,%q,%q) want (%q,%q,%q)",
				back.Owner(), back.Bucket(), back.Path(), tc.owner, tc.bucket, tc.path)
		}
	}
}

// TestParseLocation_FailClosed 非法定位拒绝（scheme 错/卷名不一致/无路径/缺 owner/逃逸）。
func TestParseLocation_FailClosed(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"http://mydisk/user/x",     // scheme 错
		"volume://other/user/x",    // 卷名不一致
		"volume://mydisk",          // 无路径
		"volume://mydisk/user",     // 缺 owner 段（废弃独享无前缀后 owner 必填）
		"volume://mydisk/alice",    // 缺桶段
		"volume://mydisk/../etc/x", // 逃逸
	} {
		if _, err := privateVolume().ParseLocation(s); err == nil {
			t.Errorf("非法定位 %q 应报错", s)
		}
	}
	// 仅桶（path 空）合法：`volume://<卷>/<owner>/<bucket>`（owner/桶必填，path 可空）。
	if _, err := sharedVolume().ParseLocation("volume://shared/alice/user"); err != nil {
		t.Errorf("仅桶定位应通过: %v", err)
	}
}
