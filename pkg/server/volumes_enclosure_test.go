// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// sealTestWrapper 是装配接线测试用最小封装处理器（不构造真实后端）。
type sealTestWrapper struct{}

func (sealTestWrapper) Name() string { return "testsealwrapper" }

func (sealTestWrapper) Build(context.Context, volume.Volume, syncpkg.FS, map[string]any) (registry.ExternalBackend, error) {
	return nil, nil
}

// registerSealTestWrapper 幂等注册（并发用例共享；重复注册会 panic）。
var sealWrapOnce sync.Once

func registerSealTestWrapper() {
	sealWrapOnce.Do(func() { enclosure.RegisterWrapper(sealTestWrapper{}) })
}

// volAccumForTest 构造装配累积（仅 roots/volumes 参与封装接线）。
func volAccumForTest(t *testing.T, rootDir string) *volumeAccum {
	t.Helper()
	rt, err := storage.OpenRoot(rootDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return &volumeAccum{roots: map[string]*storage.Root{"main": rt}}
}

// TestApplyEnclosureSeals_InjectsAndBlocksDirectAccess 装配接线：封装声明 → 源卷封印生效
// （写恒拒 ErrSealed；读默认按不存在），且**不误伤**同目录其它路径。
// 变异：去掉 applyEnclosureSeals 的 SetSeals 调用 → 写入不再被拒 → 红。
func TestApplyEnclosureSeals_InjectsAndBlocksDirectAccess(t *testing.T) {
	t.Parallel()
	registerSealTestWrapper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "private", "vault"), 0o755); err != nil {
		t.Fatal(err)
	}
	acc := volAccumForTest(t, dir)
	acc.volumes = []volume.Volume{
		{Name: "main", Type: volume.TypeLocal},
		{Name: "vault", Type: "testsealwrapper", Extra: map[string]any{
			"source": "main:private/vault",
		}},
	}
	if err := applyEnclosureSeals(nil, acc); err != nil {
		t.Fatalf("applyEnclosureSeals: %v", err)
	}
	rt := acc.roots["main"]
	if len(rt.Seals()) != 1 || rt.Seals()[0].Path != "private/vault" {
		t.Fatalf("源卷应被注入封印, got %+v", rt.Seals())
	}

	// 写：绝对禁止（可归因 ErrSealed）。
	w, werr := rt.OpenFile("private/vault/x.bin", os.O_CREATE|os.O_WRONLY, 0o644)
	if werr == nil {
		_ = w.Close()
		t.Fatal("封印路径写入必须被拒绝")
	}
	if !errors.Is(werr, storage.ErrSealed) {
		t.Fatalf("写拒绝应可归因 ErrSealed, got %v", werr)
	}
	// 读：默认按不存在（hidden）。
	if _, rerr := rt.Stat("private/vault"); !errors.Is(rerr, fs.ErrNotExist) {
		t.Fatalf("读封印路径应按不存在, got %v", rerr)
	}
	// 未封印路径不受影响。
	if err := rt.MkdirAll("private/other", 0o755); err != nil {
		t.Fatalf("同目录未封印路径应可用: %v", err)
	}
}

// TestApplyEnclosureSeals_WholeVolumeSealsEntireSource 「子路径可空 = 整卷封装」是设计 §4/§5、
// docs/config.md 与 config.example.yaml 明确承诺的配置（`enclosure.KeySpacePrefix()` 亦按空前缀
// 实现），但 `storage.SetSeals` 把空路径判非法 ⇒ 该配置在装配期**恒失败 = boot fail**（文档承诺
// 与实现矛盾；enclosure 侧测试只覆盖 Parse/视图，从未走到 SetSeals，故长期未暴露）。
//
// 变异：`normalizeSealPath` 重新拒绝空路径 → 本用例装配报错 → 红。
func TestApplyEnclosureSeals_WholeVolumeSealsEntireSource(t *testing.T) {
	t.Parallel()
	registerSealTestWrapper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "user"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user", "a.bin"), []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	acc := volAccumForTest(t, dir)
	acc.volumes = []volume.Volume{
		{Name: "main", Type: volume.TypeLocal},
		{Name: "vault", Type: "testsealwrapper", Extra: map[string]any{
			"source": "main:", // 空子路径 = 整卷封装
		}},
	}
	if err := applyEnclosureSeals(nil, acc); err != nil {
		t.Fatalf("整卷封装装配应成功（空路径 = 整卷封印）: %v", err)
	}
	rt := acc.roots["main"]
	got := rt.Seals()
	if len(got) != 1 || got[0].Path != "" {
		t.Fatalf("整卷封装应注入空前缀封印规则, got %+v", got)
	}
	// 卷内任意路径直连：写恒拒、读 hidden 按不存在（同目录「不误伤」不再适用于整卷）。
	if f, werr := rt.OpenFile("user/new.bin", os.O_CREATE|os.O_WRONLY, 0o644); !errors.Is(werr, storage.ErrSealed) {
		_ = f
		t.Fatalf("整卷封装下直连写应被拒（ErrSealed）, got %v", werr)
	}
	if _, rerr := rt.Stat("user/a.bin"); !errors.Is(rerr, fs.ErrNotExist) {
		t.Fatalf("整卷封装下直连读应按不存在, got %v", rerr)
	}
}

// TestApplyEnclosureSeals_NoDeclZeroRegression 无封装声明时不注入任何封印。
func TestApplyEnclosureSeals_NoDeclZeroRegression(t *testing.T) {
	t.Parallel()
	acc := volAccumForTest(t, t.TempDir())
	acc.volumes = []volume.Volume{{Name: "main", Type: volume.TypeLocal}}
	if err := applyEnclosureSeals(nil, acc); err != nil {
		t.Fatalf("无声明应零回归: %v", err)
	}
	if got := acc.roots["main"].Seals(); len(got) != 0 {
		t.Fatalf("无声明不应有封印, got %+v", got)
	}
}

// TestApplyEnclosureSeals_InvalidDeclFailsClosed 声明非法（`extra.wrapper` 未注册）⇒ 装配失败且可归因。
//
// **自带注册**：判据 `IsDeclared` 要求「卷类型是已注册 wrapper」——若依赖同文件其它用例先注册
// （sealWrapOnce），并行调度下本用例可能在注册前执行 ⇒ 波动的 nil 错误（实测 `./...` 全量跑
// 偶发红）。故本用例显式调用幂等注册（与同胞用例同口径）。
func TestApplyEnclosureSeals_InvalidDeclFailsClosed(t *testing.T) {
	t.Parallel()
	registerSealTestWrapper()
	acc := volAccumForTest(t, t.TempDir())
	acc.volumes = []volume.Volume{
		{Name: "main", Type: volume.TypeLocal},
		{Name: "vault", Type: "testsealwrapper", Extra: map[string]any{"source": "main:x", "wrapper": "ghost-wrapper"}},
	}
	if err := applyEnclosureSeals(nil, acc); !errors.Is(err, enclosure.ErrUnknownWrapper) {
		t.Fatalf("非法声明应 fail-closed 且可归因, got %v", err)
	}
}

// fakeExtBackend 是外部卷后端替身（只提供 FS/Close）。
type fakeExtBackend struct{ fs syncpkg.FS }

func (b fakeExtBackend) FS() syncpkg.FS { return b.fs }
func (b fakeExtBackend) Close() error   { return nil }

// TestApplyEnclosureSeals_ExternalSourceSealedViaBackendFS：**外部卷作源**时封印套在后端 FS 上
// （装配处收口）——所有 `be.FS()` 调用者（HTTP/DAV/S3/分享/云任务/接缝）自动继承；
// 封装卷自身经 UnsealedSource 取未封印特权视图（只剥封印层）。
// 变异：去掉 injectSeals 的 SealBackend 包装 → 写入不再被拒 → 红。
func TestApplyEnclosureSeals_ExternalSourceSealedViaBackendFS(t *testing.T) {
	t.Parallel()
	registerSealTestWrapper()
	raw := syncpkg.NewLocalFS(t.TempDir(), nil)
	ctx := context.Background()
	for _, p := range []string{"alice/user/vault", "alice/user/other"} {
		if err := raw.MakeDir(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	acc := &volumeAccum{
		roots:    map[string]*storage.Root{},
		external: map[string]registry.ExternalBackend{"ext": fakeExtBackend{fs: raw}},
		pools:    map[string]*quota.Pool{"ext": quota.NewPool(0), "vault": quota.NewPool(0)},
	}
	acc.volumes = []volume.Volume{
		{Name: "ext", Type: "s3"},
		{Name: "vault", Type: "testsealwrapper", Extra: map[string]any{
			"source": "ext:alice/user/vault",
		}},
	}
	if err := applyEnclosureSeals(nil, acc); err != nil {
		t.Fatalf("外部源封装装配应成功（FS 级封印已接入）: %v", err)
	}
	be := acc.external["ext"]
	view := be.FS()
	// 写：恒拒（可归因 ErrSealed）——覆盖「直连外部卷后端」的十余处调用者。
	if werr := view.WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/vault/x.bin", strings.NewReader("p"), 1, 0); !errors.Is(werr, storage.ErrSealed) {
		t.Fatalf("外部卷封印路径写入必须被拒（ErrSealed）, got %v", werr)
	}
	// 读：hidden ⇒ 同时可归因 fs.ErrNotExist 与 ErrSealed。
	if _, rerr := view.OpenRead(ctx, "alice/user/vault/x.bin"); !errors.Is(rerr, fs.ErrNotExist) || !errors.Is(rerr, storage.ErrSealed) {
		t.Fatalf("外部卷封印路径读应按「不存在」且可归因, got %v", rerr)
	}
	// 列举过滤：hidden 下不泄漏封装布局。
	if err := raw.WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/vault/real.bin", strings.NewReader("payload"), 7, 0); err != nil {
		t.Fatal(err)
	}
	entries, lerr := view.ListDir(ctx, "alice/user")
	if lerr != nil {
		t.Fatalf("父目录列举应可用: %v", lerr)
	}
	for _, e := range entries {
		if e.Name == "vault" {
			t.Fatalf("hidden 模式必须过滤封印条目, got %+v", entries)
		}
	}
	// **H1 回归（足迹 = 映射可触达桶集合）**：封装卷的 sidecar/版本等写入落在 **meta/version 等桶**
	// 的同名子树下 ⇒ 这些桶**同样必须被封印**（否则客户端可直连源卷改写 sidecar/版本，绕过服务端）。
	for _, b := range storage.ReservedBucketNames() {
		key := "alice/" + b + "/vault/x.bin"
		if werr := view.WriteFile(ctx, syncpkg.LogicalKey{}, key, strings.NewReader("p"), 1, 0); !errors.Is(werr, storage.ErrSealed) {
			t.Fatalf("桶 %s 的足迹子路径直连写入必须被拒（否则可绕过封装卷）, got %v", b, werr)
		}
	}
	// 未封印路径不受影响（零回归）。
	if werr := view.WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/other/ok.bin", strings.NewReader("ok"), 2, 0); werr != nil {
		t.Fatalf("同卷未封印路径写应放行, got %v", werr)
	}
	// 特权视图：封装卷自身仍能读写被封区间（只剥封印层 ⇒ 源卷配额/可信装饰器保留）。
	priv := enclosure.UnsealedSource(be)
	if priv == nil {
		t.Fatal("未封印特权视图应可用")
	}
	if werr := priv.WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/vault/by-enclosure.bin", strings.NewReader("plain"), 5, 0); werr != nil {
		t.Fatalf("特权视图必须能写入被封区间, got %v", werr)
	}
	if _, serr := raw.Stat(ctx, "alice/user/vault/by-enclosure.bin"); serr != nil {
		t.Fatalf("特权写入应真的落到源存储: %v", serr)
	}
}

// TestApplyEnclosureSeals_ExternalWholeVolume：外部源「空子路径 = 整卷封印」同样生效。
func TestApplyEnclosureSeals_ExternalWholeVolume(t *testing.T) {
	t.Parallel()
	registerSealTestWrapper()
	raw := syncpkg.NewLocalFS(t.TempDir(), nil)
	ctx := context.Background()
	acc := &volumeAccum{
		roots:    map[string]*storage.Root{},
		external: map[string]registry.ExternalBackend{"ext": fakeExtBackend{fs: raw}},
		pools:    map[string]*quota.Pool{"ext": quota.NewPool(0)},
	}
	acc.volumes = []volume.Volume{
		{Name: "ext", Type: "s3"},
		{Name: "vault", Type: "testsealwrapper", Extra: map[string]any{
			"source": "ext:",
		}},
	}
	if err := applyEnclosureSeals(nil, acc); err != nil {
		t.Fatalf("外部源整卷封装装配应成功: %v", err)
	}
	view := acc.external["ext"].FS()
	for _, p := range []string{"alice/user/a.bin", "meta/x.meta", "deep/nested/p.bin"} {
		if werr := view.WriteFile(ctx, syncpkg.LogicalKey{}, p, strings.NewReader("x"), 1, 0); !errors.Is(werr, storage.ErrSealed) {
			t.Fatalf("整卷封印下 %s 写入必须被拒, got %v", p, werr)
		}
	}
}

// TestApplyEnclosureSeals_ExternalSourceMissingBackendFailsClosed：外部源后端未装配 →
// **fail-closed 拒绝启动**（禁静默降级：否则会留下「以为封了其实没封」的安全缺口）。
func TestApplyEnclosureSeals_ExternalSourceMissingBackendFailsClosed(t *testing.T) {
	t.Parallel()
	registerSealTestWrapper()
	acc := &volumeAccum{
		roots:    map[string]*storage.Root{},
		external: map[string]registry.ExternalBackend{},
		pools:    map[string]*quota.Pool{},
	}
	acc.volumes = []volume.Volume{
		{Name: "ext", Type: "s3"},
		{Name: "vault", Type: "testsealwrapper", Extra: map[string]any{
			"source": "ext:alice/user/vault",
		}},
	}
	if err := applyEnclosureSeals(nil, acc); err == nil {
		t.Fatal("外部源后端缺失必须装配失败（fail-closed）")
	}
}
