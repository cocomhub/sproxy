// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

// ownerfs_test.go 钉住 **owner 限定声明的「调用方面」键映射视图**（设计 §4.1）与**两个视图的分离**：
//
//	调用方面（backend.FS()）：<owner>/<bucket>/<rel> → <declOwner>/<bucket>/<sub>/<rel>（非嵌套）
//	wrapper 面（NewFS）：     区域相对键 + 声明前缀（封装处理器自建容器/blob 名）
//
// 不变式（每条都有判据）：owner 由声明给定（精确相等，不折叠/不通配）；bucket 白名单 = 全部 8 个
// 保留功能桶（与封印足迹同源）；严格段校验（拒绝 `.`/`..`/反斜杠/空段/控制字符，绝不用会归一的
// `path.Join`）；根/无 owner 键拒绝；映射单射无别名；真值报告键翻译。
//
// 变异：去掉 owner 相等校验 → 越权断言红；把严格校验换成 path.Join → 穿越断言红；
// 桶白名单放宽为「任意段」→ 非保留桶断言红；映射装到 wrapper 面 → 区域键断言红。

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// fakeOwnerBackend 是最小外部后端替身（只提供 FS/Close）。
type fakeOwnerBackend struct{ fs syncpkg.FS }

func (b fakeOwnerBackend) FS() syncpkg.FS { return b.fs }
func (b fakeOwnerBackend) Close() error   { return nil }

// ownerViewEnv 构造「源卷根 + owner 限定封装（调用方面视图）」。
func ownerViewEnv(t *testing.T) (syncpkg.FS, string) {
	t.Helper()
	dir := t.TempDir()
	inner := syncpkg.NewLocalFS(dir, nil)
	d := Decl{Vol: "vault", Source: "ext", Path: "alice/user/vault", Owner: "alice", Sub: "vault", Wrapper: ownerFSTestWrapperName}
	be := WrapOwnerFacing(fakeOwnerBackend{fs: inner}, d)
	return be.FS(), dir
}

// TestOwnerFS_MapsExactlyIntoDeclaredRegion：调用方键 → 精确落点（非嵌套），且 wrapper 面
// 仍是区域相对键（两个视图互不干扰）。
func TestOwnerFS_MapsExactlyIntoDeclaredRegion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	view, root := ownerViewEnv(t)
	if err := view.MakeDir(ctx, "alice/user/docs"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if err := view.WriteFile(ctx, "alice/user/docs/x.bin", strings.NewReader("payload"), 7, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want := filepath.Join(root, "alice", "user", "vault", "docs", "x.bin")
	if _, serr := os.Stat(want); serr != nil {
		t.Fatalf("内容应精确落在 %s: %v", want, serr)
	}
	if _, serr := os.Stat(filepath.Join(root, "alice", "user", "vault", "alice")); serr == nil {
		t.Fatal("落盘不得出现嵌套重复 owner（映射退化为前缀前插）")
	}
	rc, oerr := view.OpenRead(ctx, "alice/user/docs/x.bin")
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	defer rc.Close()
	buf := make([]byte, 7)
	if _, rerr := io.ReadFull(rc, buf); rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if string(buf) != "payload" {
		t.Fatalf("读回内容不符: %q", buf)
	}
	// wrapper 面（区域相对键）：同一源 FS 上，`a.bin` 落在区域根下。
	regionFace := NewFS(syncpkg.NewLocalFS(root, nil), Decl{Vol: "vault", Source: "ext", Path: "alice/user/vault", Owner: "alice", Sub: "vault", Wrapper: ownerFSTestWrapperName})
	if err := regionFace.WriteFile(ctx, "a.bin", strings.NewReader("w"), 1, 0); err != nil {
		t.Fatalf("wrapper 面写入: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(root, "alice", "user", "vault", "a.bin")); serr != nil {
		t.Fatalf("wrapper 面应保持区域相对键语义（声明前缀 + 内部键）: %v", serr)
	}
}

// TestOwnerFS_FootprintCoversAllReservedBuckets：**映射可触达的桶 = 封印足迹的桶**（H1 结构性修复）。
// 逐桶精确断言落点，且 `Decl.Footprint()` 规则数 = 保留桶数。
func TestOwnerFS_FootprintCoversAllReservedBuckets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	view, root := ownerViewEnv(t)
	buckets := storage.ReservedBucketNames()
	if len(buckets) < 8 {
		t.Fatalf("保留桶清单异常（%d）", len(buckets))
	}
	for _, b := range buckets {
		key := "alice/" + b + "/obj.bin"
		if err := view.WriteFile(ctx, key, strings.NewReader("x"), 1, 0); err != nil {
			t.Fatalf("桶 %s 写入应放行（在白名单与足迹内）: %v", b, err)
		}
		want := filepath.Join(root, "alice", b, "vault", "obj.bin")
		if _, serr := os.Stat(want); serr != nil {
			t.Fatalf("桶 %s 应精确落在 %s: %v", b, want, serr)
		}
	}
	d := Decl{Vol: "vault", Source: "ext", Path: "alice/user/vault", Owner: "alice", Sub: "vault"}
	rules := d.Footprint()
	if len(rules) != len(buckets) {
		t.Fatalf("封印足迹应覆盖全部保留桶（%d 条），got %d", len(buckets), len(rules))
	}
	// 足迹路径集合必须与映射落点一致（逐桶）。
	got := map[string]bool{}
	for _, r := range rules {
		got[r.Path] = true
	}
	for _, b := range buckets {
		if !got["alice/"+b+"/vault"] {
			t.Fatalf("足迹缺少 %s 桶规则: %v", b, got)
		}
	}
}

// TestOwnerFS_RejectsOutOfFootprintAndBadKeys：非保留桶 / 穿越 / 根键 / 他人 owner 一律 fail-closed。
func TestOwnerFS_RejectsOutOfFootprintAndBadKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	view, root := ownerViewEnv(t)
	cases := []struct{ key, why string }{
		{"alice/private/x.bin", "非保留功能桶（不在足迹内）"},
		{"alice/user/../meta/x.bin", "路径穿越（不得被 path.Join 静默折叠）"},
		{"alice/user/a/../../bob/user/x", "越出 owner"},
		{"alice/user/vault/../../meta/x", "越出区域"},
		{`alice\user\x`, "反斜杠"},
		{"", "根键（区域按桶定义，无对应物）"},
		{"alice", "缺少 bucket 段"},
		{"bob/user/x.bin", "他人 owner"},
	}
	for _, c := range cases {
		if _, err := view.Stat(ctx, c.key); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("键 %q（%s）应按不存在拒绝, got %v", c.key, c.why, err)
		}
		if werr := view.WriteFile(ctx, c.key, strings.NewReader("x"), 1, 0); !errors.Is(werr, fs.ErrNotExist) {
			t.Fatalf("键 %q（%s）写应按不存在拒绝, got %v", c.key, c.why, werr)
		}
	}
	// 越权/穿越都不得在源卷留下痕迹。
	for _, bad := range []string{"bob", "private"} {
		if _, serr := os.Stat(filepath.Join(root, bad)); serr == nil {
			t.Fatalf("不得创建 %s（越权或越界写入）", bad)
		}
	}
	if _, serr := os.Stat(filepath.Join(root, "alice", "meta")); serr == nil {
		t.Fatal("穿越键不得改写 meta 桶（折叠越界）")
	}
}

// TestOwnerFS_TrueSizeTranslatesKey：真值报告键翻译（调用方键 → 区域键）。
func TestOwnerFS_TrueSizeTranslatesKey(t *testing.T) {
	t.Parallel()
	inner := &recTrueUsageFS{}
	be := WrapOwnerFacing(fakeOwnerBackend{fs: inner}, Decl{
		Vol: "vault", Source: "ext", Path: "alice/user/vault", Owner: "alice", Sub: "vault",
	})
	rep, ok := capacity.TrueUsageOf(be.FS())
	if !ok {
		t.Fatal("调用方面视图应暴露真值报告能力")
	}
	if _, ok := rep.TrueSize("alice/user/docs/x.bin"); !ok {
		t.Fatal("TrueSize 应可回答")
	}
	if inner.lastKey != "alice/user/vault/docs/x.bin" {
		t.Fatalf("真值键应翻译为区域键, got %q", inner.lastKey)
	}
	// 非法键 ⇒ 不掌握（ok=false），不得伪造。
	if _, ok := rep.TrueSize("bob/user/x.bin"); ok {
		t.Fatal("越权键的真值必须 ok=false")
	}
}

// recTrueUsageFS 记录真值查询用的键（钉住翻译）。
type recTrueUsageFS struct {
	syncpkg.FS
	lastKey string
}

func (r *recTrueUsageFS) TrueUsage() (int64, bool) { return 42, true }

func (r *recTrueUsageFS) TrueSize(rel string) (int64, bool) {
	r.lastKey = rel
	return 1, true
}

// TestWrapOwnerFacing_LegacyDeclarationUnchanged：卷根专有声明不做映射（零回归）。
func TestWrapOwnerFacing_LegacyDeclarationUnchanged(t *testing.T) {
	t.Parallel()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	be := fakeOwnerBackend{fs: inner}
	out := WrapOwnerFacing(be, Decl{Vol: "v", Source: "main", Path: "private/vault", Wrapper: "w"})
	if out.FS() != inner {
		t.Fatal("非 owner 限定声明的 backend.FS() 必须保持原样（零回归）")
	}
}

// TestEffectiveFootprint_NestingAndMixedChain：嵌套链的足迹拼接与混链 fail-closed。
func TestEffectiveFootprint_NestingAndMixedChain(t *testing.T) {
	t.Parallel()
	inner := Decl{Vol: "inner", Source: "ext", Path: "alice/user/base", Owner: "alice", Sub: "base"}
	outer := Decl{Vol: "outer", Source: "inner", Path: "alice/user/v2", Owner: "alice", Sub: "v2"}
	baseVol, rules, err := EffectiveFootprint(outer, []Decl{inner, outer})
	if err != nil {
		t.Fatalf("同 owner 嵌套链应可解析: %v", err)
	}
	if baseVol != "ext" {
		t.Fatalf("基底卷应为 ext, got %q", baseVol)
	}
	if len(rules) != len(storage.ReservedBucketNames()) {
		t.Fatalf("嵌套链足迹仍应覆盖全部保留桶, got %d", len(rules))
	}
	found := false
	for _, r := range rules {
		if r.Path == "alice/user/base/v2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("嵌套链应拼接 sub（内层在前）, got %+v", rules)
	}
	// owner 不一致 ⇒ fail-closed。
	other := Decl{Vol: "x", Source: "inner", Path: "bob/user/v2", Owner: "bob", Sub: "v2"}
	if _, _, err := EffectiveFootprint(other, []Decl{inner, other}); err == nil {
		t.Fatal("链内 owner 不一致必须 fail-closed")
	}
	// 混链 ⇒ fail-closed。
	legacy := Decl{Vol: "y", Source: "inner", Path: "private/v", Wrapper: "w"}
	if _, _, err := EffectiveFootprint(legacy, []Decl{inner, legacy}); err == nil {
		t.Fatal("owner 限定与卷根专有混链必须 fail-closed")
	}
}

// TestParse_OwnerQualifiedDetection：声明形态判定矩阵（含非限定与单段）。
func TestParse_OwnerQualifiedDetection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path               string
		wantOwner, wantSub string
	}{
		{"alice/user/vault", "alice", "vault"},
		{"alice/meta", "alice", ""},
		{"alice/user/vault/deep/x", "alice", "vault/deep/x"},
		{"private/vault", "", ""},
		{"alice", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		owner, sub := splitOwnerQualified(c.path)
		if owner != c.wantOwner || sub != c.wantSub {
			t.Fatalf("splitOwnerQualified(%q)=(%q,%q) want (%q,%q)", c.path, owner, sub, c.wantOwner, c.wantSub)
		}
	}
	registerOwnerFSTestWrapper()
	vols := []volume.Volume{
		{Name: "ext", Type: "s3"},
		{Name: "vault", Type: ownerFSTestWrapperName, Extra: map[string]any{
			"source": "ext:alice/user/vault",
		}},
	}
	decls, err := Parse(vols)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(decls) != 1 || decls[0].Owner != "alice" || decls[0].Sub != "vault" || !decls[0].OwnerQualified() {
		t.Fatalf("owner 限定声明解析异常: %+v", decls)
	}
}

// ---- 测试用 wrapper（Parse 需要已注册 wrapper；唯一名 + Once 避免重复注册 panic） ----

const ownerFSTestWrapperName = "ownerfs-test-wrapper"

var ownerFSWrapOnce sync.Once

type ownerFSTestWrapper struct{}

func (ownerFSTestWrapper) Name() string { return ownerFSTestWrapperName }

func (ownerFSTestWrapper) Build(context.Context, volume.Volume, syncpkg.FS, map[string]any) (registry.ExternalBackend, error) {
	return nil, nil
}

func registerOwnerFSTestWrapper() {
	ownerFSWrapOnce.Do(func() { RegisterWrapper(ownerFSTestWrapper{}) })
}
