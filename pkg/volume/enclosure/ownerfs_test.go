// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

// ownerfs_test.go 钉住 **owner 限定声明的键映射**（设计 §4.1「客户端可达的封装区间」）：
// 调用方键 `<owner>/<bucket>/<rel>` → 源卷键 `<declOwner>/<bucket>/<sub>/<rel>`（**非嵌套**），
// owner 必须与声明**精确相等**（不折叠、不通配）；bucket 沿用调用方（user/meta 镜像同样可用）。
//
// 为什么必须断言**精确键**：此前实现是「声明前缀 + 调用方整键」的前插 ⇒ 落盘为
// `<decl>/<owner>/<bucket>/<rel>`（嵌套重复 owner/bucket），而既有测试只断言「内容落在声明
// 子树内」⇒ 文档（§4.1）与实现长期不一致而无人发现。精确键断言是这类偏离的唯一有效判据。
//
// 变异：把映射改回前缀前插 → 精确键断言红；去掉 owner 相等校验 → 越权（他人 owner）断言红。

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// newOwnerViewEnv 构造“源卷根 + owner 限定封装视图”（decl: alice/user/vault）。
func newOwnerViewEnv(t *testing.T) (*FS, string) {
	t.Helper()
	dir := t.TempDir()
	inner := syncpkg.NewLocalFS(dir, nil)
	d := Decl{Vol: "vault", Source: "ext", Path: "alice/user/vault", Owner: "alice", Sub: "vault", Wrapper: "testsealwrapper"}
	return NewFS(inner, d), dir
}

func TestOwnerFS_MapsToDeclaredRegionExactly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	view, root := newOwnerViewEnv(t)
	if err := view.MakeDir(ctx, "alice/user/docs"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if err := view.WriteFile(ctx, "alice/user/docs/x.bin", strings.NewReader("payload"), 7, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 精确键：<declOwner>/<bucket>/<sub>/<rel>（非嵌套）。
	want := filepath.Join(root, "alice", "user", "vault", "docs", "x.bin")
	if _, serr := os.Stat(want); serr != nil {
		t.Fatalf("内容应精确落在 %s: %v", want, serr)
	}
	// 反向：嵌套重复形态**必须不存在**（若存在说明映射退化为前缀前插）。
	bad := filepath.Join(root, "alice", "user", "vault", "alice")
	if _, serr := os.Stat(bad); serr == nil {
		t.Fatalf("落盘不得出现嵌套重复 owner（%s 存在 ⇒ 映射退化为前缀前插）", bad)
	}
	// 读回一致。
	rc, oerr := view.OpenRead(ctx, "alice/user/docs/x.bin")
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	defer rc.Close()
	buf := make([]byte, 7)
	if _, rerr := rc.Read(buf); rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if string(buf) != "payload" {
		t.Fatalf("读回内容不符: %q", buf)
	}
}

func TestOwnerFS_RejectsOtherOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	view, root := newOwnerViewEnv(t)
	// 他人 owner：读/写都必须按「不存在」拒绝（不泄漏存在性、不跨 owner 写入）。
	if werr := view.WriteFile(ctx, "bob/user/x.bin", strings.NewReader("x"), 1, 0); !errors.Is(werr, fs.ErrNotExist) {
		t.Fatalf("他人 owner 写入必须按不存在拒绝, got %v", werr)
	}
	if _, rerr := view.OpenRead(ctx, "bob/user/x.bin"); !errors.Is(rerr, fs.ErrNotExist) {
		t.Fatalf("他人 owner 读必须按不存在拒绝, got %v", rerr)
	}
	if _, serr := os.Stat(filepath.Join(root, "bob")); serr == nil {
		t.Fatal("他人 owner 的目录不得被创建（越权写入）")
	}
}

func TestOwnerFS_BucketPreservedForMetaMirror(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	view, root := newOwnerViewEnv(t)
	// meta 桶（sidecar 镜像）沿用调用方桶名 ⇒ <owner>/meta/<sub>/<rel>。
	if err := view.WriteFile(ctx, "alice/meta/docs/x.bin.meta", strings.NewReader("m"), 1, 0); err != nil {
		t.Fatalf("meta 桶写入: %v", err)
	}
	want := filepath.Join(root, "alice", "meta", "vault", "docs", "x.bin.meta")
	if _, serr := os.Stat(want); serr != nil {
		t.Fatalf("sidecar 应落在 %s: %v", want, serr)
	}
}

func TestOwnerFS_EmptySubIsOwnerBucketView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	inner := syncpkg.NewLocalFS(dir, nil)
	// 声明到「桶」级：alice/user ⇒ Sub 空 ⇒ 该 owner 的整桶视图（映射为恒等）。
	view := NewFS(inner, Decl{Vol: "v", Source: "ext", Path: "alice/user", Owner: "alice", Wrapper: "w"})
	if err := view.WriteFile(ctx, "alice/user/a.bin", strings.NewReader("x"), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "alice", "user", "a.bin")); serr != nil {
		t.Fatalf("整桶视图应是恒等映射: %v", serr)
	}
}

func TestOwnerFS_ListDirMapsBackToCallerSpace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	view, root := newOwnerViewEnv(t)
	if err := os.MkdirAll(filepath.Join(root, "alice", "user", "vault"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alice", "user", "vault", "a.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, lerr := view.ListDir(ctx, "alice/user")
	if lerr != nil {
		t.Fatalf("ListDir: %v", lerr)
	}
	// 视图根（`alice/user`）= 声明区（`alice/user/vault`）⇒ 其子项在调用方键空间呈现为
	// `alice/user/a.bin`（而非带 vault 的源卷键）。
	var got []string
	for _, e := range entries {
		got = append(got, e.Path)
	}
	if len(got) != 1 || got[0] != "alice/user/a.bin" {
		t.Fatalf("列举条目应反向映射回调用方键空间（期望 [alice/user/a.bin]）, got %v", got)
	}
}

func TestParse_OwnerQualifiedDetection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path               string
		wantOwner, wantSub string
	}{
		{"alice/user/vault", "alice", "vault"}, // owner + 保留桶 ⇒ 限定
		{"alice/user", "alice", ""},            // 桶级
		{"alice/user/vault/deep/x", "alice", "vault/deep/x"},
		{"private/vault", "", ""}, // 第二段非保留桶 ⇒ 卷根专有区间（照旧）
		{"alice", "", ""},         // 单段 ⇒ 照旧
		{"", "", ""},
	}
	for _, c := range cases {
		owner, sub := splitOwnerQualified(c.path)
		if owner != c.wantOwner || sub != c.wantSub {
			t.Fatalf("splitOwnerQualified(%q) = (%q,%q), want (%q,%q)", c.path, owner, sub, c.wantOwner, c.wantSub)
		}
	}
	// Parse 端到端：owner 限定声明被正确填充（走生产解析路径；需要已注册 wrapper）。
	registerOwnerFSTestWrapper()
	vols := []volume.Volume{
		{Name: "ext", Type: "s3"},
		{Name: "vault", Type: TypeEnclosure, Extra: map[string]any{
			"source": "ext:alice/user/vault", "wrapper": ownerFSTestWrapperName,
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

func TestFS_LegacyRootPrivateDeclarationKeepsPrefixSemantics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	inner := syncpkg.NewLocalFS(dir, nil)
	// 非 owner 限定（卷根专有区间）：语义照旧 = 声明路径前缀 + 调用方键（客户端本就寻址不到）。
	view := NewFS(inner, Decl{Vol: "v", Source: "main", Path: "private/vault", Wrapper: "w"})
	if err := view.WriteFile(ctx, "docs/a.bin", strings.NewReader("x"), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "private", "vault", "docs", "a.bin")); serr != nil {
		t.Fatalf("卷根专有区间应保持前缀语义: %v", serr)
	}
}

// ownerFSTestWrapperName / registerOwnerFSTestWrapper：本文件用的最小 wrapper（Parse 校验
// 需要 wrapper 已注册）；唯一名 + Once，避免与其它测试文件重复注册 panic。
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
