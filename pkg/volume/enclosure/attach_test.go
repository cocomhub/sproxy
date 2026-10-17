// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// captureWrapper 捕获 Build 收到的源视图（验证「源视图 = 加前缀的 PrefixFS」）。
type captureWrapper struct {
	name string
	mu   sync.Mutex
	view syncpkg.FS
	vols []string
}

func (w *captureWrapper) Name() string { return w.name }

func (w *captureWrapper) Build(_ context.Context, v volume.Volume, src syncpkg.FS, _ map[string]any) (registry.ExternalBackend, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.view = src
	w.vols = append(w.vols, v.Name)
	return &nilBackend{fs: src}, nil
}

func (w *captureWrapper) gotView() syncpkg.FS {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.view
}

// nilBackend 最小后端（FS 即源视图）。
type nilBackend struct{ fs syncpkg.FS }

func (b *nilBackend) FS() syncpkg.FS { return b.fs }
func (b *nilBackend) Close() error   { return nil }

// newCaptureWrapper 注册一个**本用例私有**的捕获处理器（全局注册表 + 并行用例 ⇒ 不可共享
// 实例，否则 view 会跨用例污染）。
func newCaptureWrapper(t *testing.T, name string) *captureWrapper {
	t.Helper()
	w := &captureWrapper{name: name}
	RegisterWrapper(w)
	return w
}

// TestEffectiveTarget_ComposesNestedPath 嵌套封装的有效目标是「链底卷 + 逐层拼接路径」——
// 封印与容量必须落在真正承载字节的那一层。
func TestEffectiveTarget_ComposesNestedPath(t *testing.T) {
	t.Parallel()
	decls := []Decl{
		{Vol: "inner", Source: "main", Path: "base"},
		{Vol: "outer", Source: "inner", Path: "sub"},
	}
	base, p := EffectiveTarget(decls[0], decls)
	if base != "main" || p != "base" {
		t.Fatalf("inner 有效目标应为 main:base, got %s:%s", base, p)
	}
	base, p = EffectiveTarget(decls[1], decls)
	if base != "main" || p != "base/sub" {
		t.Fatalf("outer 有效目标应为 main:base/sub, got %s:%s", base, p)
	}
	// 内层整卷封装：外层路径不变。
	whole := []Decl{{Vol: "inner", Source: "main", Path: ""}, {Vol: "outer", Source: "inner", Path: "sub"}}
	if _, p := EffectiveTarget(whole[1], whole); p != "sub" {
		t.Fatalf("内层整卷封装时有效路径应为 sub, got %q", p)
	}
}

// TestAttachAll_BuildsBackendWithPrefixView 补装：源视图必须是**加前缀**的 PrefixFS，
// 且后端挂回卷集（否则封装卷不可用 / wrapper 会绕过前缀直写源存储）。
func TestAttachAll_BuildsBackendWithPrefixView(t *testing.T) {
	t.Parallel()
	cw := newCaptureWrapper(t, "capture_a")
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if merr := os.MkdirAll(filepath.Join(dir, "base"), 0o755); merr != nil {
		t.Fatal(merr)
	}
	vols := []volume.Volume{
		{Name: "main", Type: volume.TypeLocal, RootDir: dir},
		{Name: "vault", Type: "capture_a", Extra: map[string]any{"source": "main:base"}},
	}
	set := registry.NewSet(vols, map[string]*storage.Root{"main": rt}, nil, nil, "main")

	if aerr := AttachAll(context.Background(), set, nil); aerr != nil {
		t.Fatalf("AttachAll: %v", aerr)
	}
	be := set.External("vault")
	if be == nil {
		t.Fatal("封装卷后端应已挂回卷集")
	}
	view := cw.gotView()
	if view == nil {
		t.Fatal("wrapper 应收到源视图")
	}
	// 经视图写入 → 必须落在 <dir>/base/ 下（证明前缀已生效）。
	if werr := view.WriteFile(context.Background(), syncpkg.LogicalKey{}, "x.bin", bytes.NewReader([]byte("v")), 1, 0); werr != nil {
		t.Fatalf("视图写入: %v", werr)
	}
	if _, serr := os.Stat(filepath.Join(dir, "base", "x.bin")); serr != nil {
		t.Fatalf("数据应落在前缀目录 base/ 下: %v", serr)
	}
}

// TestAttachAll_Idempotent 已挂回的卷不重复构造（补装必须幂等）。
func TestAttachAll_Idempotent(t *testing.T) {
	t.Parallel()
	newCaptureWrapper(t, "capture_b")
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	vols := []volume.Volume{
		{Name: "main", Type: volume.TypeLocal, RootDir: dir},
		{Name: "vault", Type: "capture_b", Extra: map[string]any{"source": "main:base"}},
	}
	set := registry.NewSet(vols, map[string]*storage.Root{"main": rt}, nil, nil, "main")
	if aerr := AttachAll(context.Background(), set, nil); aerr != nil {
		t.Fatal(aerr)
	}
	first := set.External("vault")
	if aerr := AttachAll(context.Background(), set, nil); aerr != nil {
		t.Fatal(aerr)
	}
	if set.External("vault") != first {
		t.Fatal("重复补装不应重建后端")
	}
}

// TestAttachAll_WrapsCapacityAccounting 封装卷后端必须接卷级容量记账：写入按声明大小入账、
// 超 `max_bytes` 即拒（否则封装卷的容量上限形同虚设）。
// 变异：去掉 AttachAll 的 WrapBackend → 超限写入不再被拒 → 红。
func TestAttachAll_WrapsCapacityAccounting(t *testing.T) {
	t.Parallel()
	newCaptureWrapper(t, "capture_pool")
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if merr := os.MkdirAll(filepath.Join(dir, "base"), 0o755); merr != nil {
		t.Fatal(merr)
	}
	vols := []volume.Volume{
		{Name: "main", Type: volume.TypeLocal, RootDir: dir},
		{Name: "vault", Type: "capture_pool", Extra: map[string]any{"source": "main:base"}},
	}
	pools := map[string]*quota.Pool{"vault": quota.NewPool(4)}
	set := registry.NewSet(vols, map[string]*storage.Root{"main": rt}, nil, pools, "main")
	if aerr := AttachAll(context.Background(), set, nil); aerr != nil {
		t.Fatalf("AttachAll: %v", aerr)
	}
	be := set.External("vault")
	if be == nil {
		t.Fatal("封装卷后端应已挂回")
	}
	// 超限写入（10B > max_bytes=4B）必须被拒。
	werr := be.FS().WriteFile(context.Background(), syncpkg.LogicalKey{}, "x.bin", bytes.NewReader(make([]byte, 10)), 10, 0)
	if werr == nil {
		t.Fatal("超过封装卷 max_bytes 的写入必须被拒")
	}
	// 未超限写入成功并入账。
	if werr := be.FS().WriteFile(context.Background(), syncpkg.LogicalKey{}, "y.bin", bytes.NewReader(make([]byte, 3)), 3, 0); werr != nil {
		t.Fatalf("未超限写入应成功: %v", werr)
	}
	if got := pools["vault"].Usage(); got != 3 {
		t.Fatalf("封装卷池应入账 3, got %d", got)
	}
}

// protSrcFS 是「记写入 + 出直链」的源替身（嵌入 nil FS：只用被覆写的方法）。
type protSrcFS struct {
	syncpkg.FS
	writes []string
}

func (f *protSrcFS) WriteFile(_ context.Context, key syncpkg.LogicalKey, p string, _ io.Reader, _, _ int64) error {
	f.writes = append(f.writes, p)
	return nil
}

func (f *protSrcFS) DirectURL(_ context.Context, rel string) (string, bool, error) {
	return "https://src.example/obj?key=" + rel, true, nil
}

// TestBuild_WiresProtectionLayerIntoSourceView 设计 §5 装配图要求源视图是
// `EnclosureFS(PrefixFS(源))`——薄层必须在链上。此前 `Decl.Build` 直接传裸
// `NewPrefixFS` ⇒ ① `allow_direct_url` 策略与「默认抑制直链」**完全失效**（防护层是死代码，
// 外部源存储下客户端可拿 302 直连，绕过封印与处理层）；② 身份归因丢失。
//
// 变异：把 `NewFS(src, d)` 改回 `NewPrefixFS(src, d.KeySpacePrefix())` → 本用例红。
func TestBuild_WiresProtectionLayerIntoSourceView(t *testing.T) {
	t.Parallel()
	w := newCaptureWrapper(t, "capture_prot")
	src := &protSrcFS{}
	d := Decl{Vol: "vault", Source: "main", Path: "alice/user/vault", Wrapper: "capture_prot"}
	if _, err := d.Build(context.Background(), volume.Volume{Name: "vault"}, src); err != nil {
		t.Fatalf("Build: %v", err)
	}
	view := w.gotView()
	if view == nil {
		t.Fatal("wrapper 应收到源视图")
	}
	// ① 默认抑制直链（裸 PrefixFS 会无条件透传 → 直连源存储）。
	dp, dok := view.(syncpkg.DirectURLProvider)
	if !dok {
		t.Fatal("源视图须暴露 DirectURLProvider")
	}
	if u, ok, err := dp.DirectURL(context.Background(), "x.bin"); ok || err != nil || u != "" {
		t.Fatalf("默认须抑制直链, got (%q,%v,%v)", u, ok, err)
	}
	// ② 身份归因（薄层携带 Decl）。
	fs, ok := view.(*FS)
	if !ok {
		t.Fatalf("源视图须为 *enclosure.FS（薄层在链上），got %T", view)
	}
	if fs.Decl().Vol != "vault" || fs.Decl().Source != "main" {
		t.Fatalf("身份归因缺失: %+v", fs.Decl())
	}
	// ③ 前缀映射仍在（薄层不得改变键空间）。
	if werr := view.WriteFile(context.Background(), syncpkg.LogicalKey{}, "d/x.bin", strings.NewReader("v"), 1, 0); werr != nil {
		t.Fatalf("视图写入: %v", werr)
	}
	if len(src.writes) != 1 || src.writes[0] != "alice/user/vault/d/x.bin" {
		t.Fatalf("前缀映射被破坏: %v", src.writes)
	}
	// ④ 显式 allow_direct_url 时透传，且以**源键**（加前缀）询问源存储。
	d2 := d
	d2.AllowDirectURL = true
	if _, err := d2.Build(context.Background(), volume.Volume{Name: "vault"}, src); err != nil {
		t.Fatalf("Build(allow): %v", err)
	}
	dp2, ok := w.gotView().(syncpkg.DirectURLProvider)
	if !ok {
		t.Fatalf("源视图须暴露 DirectURLProvider（got %T）", w.gotView())
	}
	u, ok, err := dp2.DirectURL(context.Background(), "x.bin")
	if err != nil || !ok || u != "https://src.example/obj?key=alice/user/vault/x.bin" {
		t.Fatalf("allow_direct_url 应透传源键直链, got (%q,%v,%v)", u, ok, err)
	}
}
