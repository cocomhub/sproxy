// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// fakeWrapper 记录 Build 收到的源视图并返回空后端。
type fakeWrapper struct {
	name   string
	gotSrc syncpkg.FS
	built  int
}

func (w *fakeWrapper) Name() string { return w.name }

func (w *fakeWrapper) Build(_ context.Context, _ volume.Volume, src syncpkg.FS, _ map[string]any) (registry.ExternalBackend, error) {
	w.gotSrc = src
	w.built++
	return nil, nil
}

// recFS 记录写入的键（FS 方法用嵌入接口占位，测试只调 Write/Open/Delete）。
type recFS struct {
	syncpkg.FS // 显式限定：包内 `FS` 会解析为 enclosure.FS（替身会意外获得 DirectURL）
	written    []string
}

func (r *recFS) WriteFile(_ context.Context, path string, _ io.Reader, _, _ int64) error {
	r.written = append(r.written, path)
	return nil
}

// directFS 提供直链能力。
type directFS struct {
	syncpkg.FS
	url string
}

func (d *directFS) DirectURL(_ context.Context, relPath string) (string, bool, error) {
	return d.url + "?key=" + relPath, true, nil
}

func encVol(name, src string, extra map[string]any) volume.Volume {
	e := map[string]any{"wrapper": "basetest", "source": src}
	for k, v := range extra {
		e[k] = v
	}
	return volume.Volume{Name: name, Type: TypeEnclosure, Extra: e}
}

func TestParse_ValidDeclAndSealRules(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "basetest")
	vols := []volume.Volume{
		{Name: "main", Type: volume.TypeLocal},
		encVol("vault", "main:private/vault", map[string]any{"source_read_mode": "readable", "max_bytes": float64(1024)}),
	}
	decls, err := Parse(vols)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(decls) != 1 {
		t.Fatalf("应解析出 1 条声明, got %d", len(decls))
	}
	d := decls[0]
	if d.Vol != "vault" || d.Source != "main" || d.Path != "private/vault" || d.Wrapper != "basetest" {
		t.Fatalf("声明字段错误: %+v", d)
	}
	if d.ReadMode != storage.SealReadable {
		t.Fatalf("source_read_mode=readable 应映射 SealReadable, got %v", d.ReadMode)
	}
	if d.MaxBytes != 1024 {
		t.Fatalf("max_bytes 应为 1024, got %d", d.MaxBytes)
	}
	if p := d.KeySpacePrefix(); p != "private/vault/" {
		t.Fatalf("键空间前缀应为 private/vault/, got %q", p)
	}
	rules := d.SealRules()
	if len(rules) != 1 || rules[0].Path != "private/vault" || rules[0].Wrapper != "vault" || rules[0].Mode != storage.SealReadable {
		t.Fatalf("封印规则错误: %+v", rules)
	}

	// 整卷封装：空前缀 + 默认 hidden。
	whole := []volume.Volume{{Name: "main"}, encVol("w", "main:", nil)}
	ds, err := Parse(whole)
	if err != nil {
		t.Fatalf("Parse(整卷): %v", err)
	}
	if ds[0].Path != "" || ds[0].KeySpacePrefix() != "" || ds[0].ReadMode != storage.SealHidden {
		t.Fatalf("整卷封装应空前缀/默认 hidden: %+v", ds[0])
	}
}

func TestParse_Rejects(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "basetest")
	base := func(extra map[string]any) volume.Volume {
		e := map[string]any{}
		for k, v := range extra {
			e[k] = v
		}
		return volume.Volume{Name: "vault", Type: TypeEnclosure, Extra: e}
	}
	cases := []struct {
		name string
		vols []volume.Volume
		want error
	}{
		{"缺 source", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest"})}, ErrMissingSource},
		{"source 无冒号", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest", "source": "main"})}, ErrBadSource},
		{"source 穿越", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest", "source": "main:../x"})}, ErrBadSource},
		{"源卷不存在", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest", "source": "nope:x"})}, ErrSourceNotFound},
		{"缺 wrapper", []volume.Volume{{Name: "main"}, base(map[string]any{"source": "main:x"})}, ErrMissingWrapper},
		{"wrapper 未注册", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "ghost", "source": "main:x"})}, ErrUnknownWrapper},
		{"自封装", []volume.Volume{base(map[string]any{"wrapper": "basetest", "source": "vault:x"})}, ErrSelfEnclosure},
		{"read_mode 非法", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest", "source": "main:x", "source_read_mode": "public"})}, ErrBadReadMode},
		{"max_bytes 负", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest", "source": "main:x", "max_bytes": float64(-1)})}, ErrBadMaxBytes},
	}
	for _, tc := range cases {
		if _, err := Parse(tc.vols); !errors.Is(err, tc.want) {
			t.Errorf("%s: 应拒绝并返回 %v, got %v", tc.name, tc.want, err)
		}
	}

	// 同一源路径被两个封装卷声明 → 冲突。
	conflict := []volume.Volume{
		{Name: "main"},
		encVol("a", "main:same", nil),
		encVol("b", "main:same", nil),
	}
	if _, err := Parse(conflict); !errors.Is(err, ErrConflictClaim) {
		t.Errorf("同一路径双声明应冲突, got %v", err)
	}

	// 成环：a→b→a。
	cyc := []volume.Volume{encVol("a", "b:", nil), encVol("b", "a:", nil)}
	if _, err := Parse(cyc); !errors.Is(err, ErrEnclosureCycle) {
		t.Errorf("成环应拒绝, got %v", err)
	}
}

func TestParse_NestedTopologicalOrder(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "basetest")
	vols := []volume.Volume{
		{Name: "main"},
		encVol("inner", "main:base", nil),
		encVol("outer", "inner:", nil),
	}
	decls, err := Parse(vols)
	if err != nil {
		t.Fatalf("Parse(嵌套): %v", err)
	}
	if len(decls) != 2 {
		t.Fatalf("应 2 条声明, got %d", len(decls))
	}
	if decls[0].Vol != "inner" {
		t.Fatalf("拓扑序应源在前（inner 先）, got %v", []string{decls[0].Vol, decls[1].Vol})
	}
}

func TestFS_DirectURLSuppressedByDefault(t *testing.T) {
	t.Parallel()
	src := &directFS{url: "https://cdn.example/x.bin"}

	// 默认：抑制（ok=false 且无错误——抑制是策略而非故障）。
	def := NewFS(src, Decl{Vol: "v", Source: "main", Path: "p", Wrapper: "w"})
	if u, ok, err := def.DirectURL(context.Background(), "a.bin"); ok || err != nil || u != "" {
		t.Fatalf("默认必须抑制直链, got url=%q ok=%v err=%v", u, ok, err)
	}

	// 显式放开：透传且以桶键空间询问底层。
	allow := NewFS(src, Decl{Vol: "v", Source: "main", Path: "p", Wrapper: "w", AllowDirectURL: true})
	u, ok, err := allow.DirectURL(context.Background(), "a.bin")
	if err != nil || !ok {
		t.Fatalf("显式放开应透传, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(u, "p/a.bin") {
		t.Fatalf("应使用桶键空间询问底层, got %q", u)
	}
}

func TestFS_DirectURLUnsupportedWhenInnerLacksCapability(t *testing.T) {
	t.Parallel()
	f := NewFS(&recFS{}, Decl{Vol: "v", Source: "main", Path: "p", Wrapper: "w", AllowDirectURL: true})
	if _, _, err := f.DirectURL(context.Background(), "a.bin"); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("底层未实现应返回 ErrUnsupported, got %v", err)
	}
}

func TestFS_KeySpaceAndIdentity(t *testing.T) {
	t.Parallel()
	rec := &recFS{}
	f := NewFS(rec, Decl{Vol: "vault", Source: "main", Path: "private/vault", Wrapper: "secretdata"})
	if err := f.WriteFile(context.Background(), "docs/a.bin", bytes.NewReader([]byte("x")), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if len(rec.written) != 1 || rec.written[0] != "private/vault/docs/a.bin" {
		t.Fatalf("写入键应加前缀, got %v", rec.written)
	}
	id := f.Identity()
	for _, want := range []string{"vault", "secretdata", "main", "private/vault"} {
		if !strings.Contains(id, want) {
			t.Fatalf("Identity 应包含 %q, got %q", want, id)
		}
	}
}

func TestDecl_Build_PassesPrefixViewToWrapper(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "buildtest")
	w := testWrapper(t, "buildtest")
	rec := &recFS{}
	d := Decl{Vol: "vault", Source: "main", Path: "private/vault", Wrapper: "buildtest"}
	if _, err := d.Build(context.Background(), volume.Volume{Name: "vault"}, rec); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if w.built != 1 || w.gotSrc == nil {
		t.Fatalf("wrapper 应收到源视图, built=%d", w.built)
	}
	// 源视图必须是加前缀的 PrefixFS（wrapper 只经视图读写，不得绕过）。
	if err := w.gotSrc.WriteFile(context.Background(), "k.bin", bytes.NewReader(nil), 0, 0); err != nil {
		t.Fatalf("视图写入: %v", err)
	}
	if len(rec.written) != 1 || rec.written[0] != "private/vault/k.bin" {
		t.Fatalf("wrapper 视图应加前缀, got %v", rec.written)
	}

	// 未注册 wrapper → 明确错误。
	bad := Decl{Vol: "v", Wrapper: "nope"}
	if _, err := bad.Build(context.Background(), volume.Volume{Name: "v"}, rec); !errors.Is(err, ErrUnknownWrapper) {
		t.Fatalf("未注册 wrapper 应报错, got %v", err)
	}
	// 源视图为空 → 装配顺序错误必须显式报错。
	if _, err := d.Build(context.Background(), volume.Volume{Name: "vault"}, nil); err == nil {
		t.Fatal("源视图为空应报错（装配顺序不变量）")
	}
}

func TestRegisterWrapper_DuplicatePanics(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "duptest")
	_ = testWrapper(t, "duptest")
	defer func() {
		if recover() == nil {
			t.Fatal("重复注册应 panic（装配期 fail-fast）")
		}
	}()
	RegisterWrapper(&fakeWrapper{name: "duptest"})
}

// testWrapper 取回测试用 wrapper 的具体类型（断言注册表内容）。
func testWrapper(t *testing.T, name string) *fakeWrapper {
	t.Helper()
	w, ok := LookupWrapper(name)
	if !ok {
		t.Fatalf("wrapper %q 应已注册", name)
	}
	fw, ok := w.(*fakeWrapper)
	if !ok {
		t.Fatalf("wrapper %q 类型应为 *fakeWrapper, got %T", name, w)
	}
	return fw
}

// testWrapMu 串行化测试用 wrapper 注册（check-then-register 必须原子，否则并行用例会同名
// 重复注册 panic）；幂等保证 -count=2 重跑安全。
var testWrapMu sync.Mutex

// registerTestWrapper 幂等注册测试用 wrapper（同名跨用例共享）。
func registerTestWrapper(t *testing.T, name string) {
	t.Helper()
	testWrapMu.Lock()
	defer testWrapMu.Unlock()
	if _, ok := LookupWrapper(name); ok {
		return
	}
	RegisterWrapper(&fakeWrapper{name: name})
}
