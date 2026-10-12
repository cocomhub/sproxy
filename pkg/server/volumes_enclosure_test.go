// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
		{Name: "vault", Type: enclosure.TypeEnclosure, Extra: map[string]any{
			"source": "main:private/vault", "wrapper": "testsealwrapper",
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
		{Name: "vault", Type: enclosure.TypeEnclosure, Extra: map[string]any{
			"source": "main:", "wrapper": "testsealwrapper", // 空子路径 = 整卷封装
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

// TestApplyEnclosureSeals_ExternalSourceRejected 跨外部卷源当前未接入 ⇒ 装配期显式拒绝
// （禁静默降级：否则会留下「以为封了其实没封」的安全缺口）。
func TestApplyEnclosureSeals_ExternalSourceRejected(t *testing.T) {
	t.Parallel()
	registerSealTestWrapper()
	acc := volAccumForTest(t, t.TempDir())
	acc.volumes = []volume.Volume{
		{Name: "main", Type: volume.TypeLocal},
		{Name: "ext", Type: "baidupcs"},
		{Name: "vault", Type: enclosure.TypeEnclosure, Extra: map[string]any{
			"source": "ext:some/dir", "wrapper": "testsealwrapper",
		}},
	}
	if err := applyEnclosureSeals(nil, acc); err == nil {
		t.Fatal("跨外部卷源应装配期拒绝")
	}
}

// TestApplyEnclosureSeals_InvalidDeclFailsClosed 声明非法（如缺 wrapper）⇒ 装配失败且可归因。
func TestApplyEnclosureSeals_InvalidDeclFailsClosed(t *testing.T) {
	t.Parallel()
	acc := volAccumForTest(t, t.TempDir())
	acc.volumes = []volume.Volume{
		{Name: "main", Type: volume.TypeLocal},
		{Name: "vault", Type: enclosure.TypeEnclosure, Extra: map[string]any{"source": "main:x"}},
	}
	if err := applyEnclosureSeals(nil, acc); !errors.Is(err, enclosure.ErrMissingWrapper) {
		t.Fatalf("非法声明应 fail-closed 且可归因, got %v", err)
	}
}
