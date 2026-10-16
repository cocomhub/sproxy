// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
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

// encVol 造一条**嵌套形态**封装声明卷：卷 Type 即处理层名（唯一声明形态，见 IsDeclared）。
func encVol(name, src string, extra map[string]any) volume.Volume {
	e := map[string]any{"wrapper": "basetest", "source": src}
	maps.Copy(e, extra)
	return volume.Volume{Name: name, Type: "basetest", Extra: e}
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
	registerTestWrapper(t, "othertest") // 供「wrapper 与卷类型不一致」用例（两值都已注册）
	base := func(extra map[string]any) volume.Volume {
		e := map[string]any{}
		maps.Copy(e, extra)
		return volume.Volume{Name: "vault", Type: "basetest", Extra: e}
	}
	cases := []struct {
		name string
		vols []volume.Volume
		want error
	}{
		{"source 无冒号", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest", "source": "main"})}, ErrBadSource},
		{"source 穿越", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest", "source": "main:../x"})}, ErrBadSource},
		{"源卷不存在", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest", "source": "nope:x"})}, ErrSourceNotFound},
		{"wrapper 未注册", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "ghost", "source": "main:x"})}, ErrUnknownWrapper},
		// wrapper 键冗余：显式写出时**必须等于卷类型**（唯一声明形态 = wrapper 类型 + source），
		// 指向另一个已注册处理层 ⇒ fail-closed（不在两值间猜哪个是处理层）。
		{"wrapper 与卷类型不一致", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "othertest", "source": "main:x"})}, ErrWrapperMismatch},
		{"自封装", []volume.Volume{base(map[string]any{"wrapper": "basetest", "source": "vault:x"})}, ErrSelfEnclosure},
		{"read_mode 非法", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest", "source": "main:x", "source_read_mode": "public"})}, ErrBadReadMode},
		{"max_bytes 负", []volume.Volume{{Name: "main"}, base(map[string]any{"wrapper": "basetest", "source": "main:x", "max_bytes": float64(-1)})}, ErrBadMaxBytes},
	}
	for _, tc := range cases {
		if _, err := Parse(tc.vols); !errors.Is(err, tc.want) {
			t.Errorf("%s: 应拒绝并返回 %v, got %v", tc.name, tc.want, err)
		}
	}

	// wrapper 类型**无** `extra.source` ⇒ 不是封装声明（普通 wrapper 卷，如本地整卷形态），
	// Parse 静默跳过而非报错——这是「缺 source/缺 wrapper 不再是错误」的落点。
	plain := []volume.Volume{
		{Name: "main"},
		{Name: "vault", Type: "basetest", Extra: map[string]any{"target": "local"}},
	}
	if ds, derr := Parse(plain); derr != nil || len(ds) != 0 {
		t.Errorf("wrapper 类型无 source 不应是封装声明: decls=%v err=%v", ds, derr)
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

// TestParse_RejectsReservedBucketFirstSegment 评审 P1：源子路径首段不得是**保留功能桶**
// （user/meta/...）——封印会按卷根命名空间注入并原样传播到每个租户子根，声明 `source: "main:user"`
// （完全合法且与设计示例同形）会把**全体租户**的 user 桶封印（读 404/写拒）= 服务级数据不可用。
// 变异：去掉保留桶名判据 → 该用例不再报错 → 红。
func TestParse_RejectsReservedBucketFirstSegment(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "basetest")
	for _, path := range []string{"user", "meta", "Meta/secret", "cloud/x"} {
		vols := []volume.Volume{
			{Name: "main"},
			encVol("vault", "main:"+path, nil),
		}
		if _, err := Parse(vols); !errors.Is(err, ErrBadSource) {
			t.Errorf("保留桶首段 %q 应被拒绝(ErrBadSource), got %v", path, err)
		}
	}
}

// TestParse_RejectsEncryptedSource 设计 §4 承诺「源卷同时 extra.encrypt → 装配期拒绝」，
// 此前 `ErrSourceEncrypted` 只有声明无实现（评审 P1/P2 双确认死哨兵）。
// 变异：去掉 encrypt 判据 → 用例不再报错 → 红。
func TestParse_RejectsEncryptedSource(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "basetest")
	vols := []volume.Volume{
		{Name: "main", Extra: map[string]any{"encrypt": true}},
		encVol("vault", "main:private/vault", nil),
	}
	if _, err := Parse(vols); !errors.Is(err, ErrSourceEncrypted) {
		t.Fatalf("源卷已加密时应拒绝封装, got %v", err)
	}
}

// TestParse_MaxBytesHumanReadable 文档/示例用人类可读 size（"8GiB"），实现必须同口径。
func TestParse_MaxBytesHumanReadable(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "basetest")
	vols := []volume.Volume{
		{Name: "main"},
		encVol("vault", "main:private/vault", map[string]any{"max_bytes": "8GiB"}),
	}
	decls, err := Parse(vols)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if decls[0].MaxBytes != 8<<30 {
		t.Fatalf("max_bytes=8GiB 应解析为 %d, got %d", int64(8)<<30, decls[0].MaxBytes)
	}
}

// TestParse_RejectsFootprintOverlap 互斥占用（阶段 2 迁移保留的独有能力）：同基底卷上两条
// **互不相关**的声明的封印足迹不得重叠（父/子/同目录）——否则两卷映射到的键集合相交，
// 一方写入落进另一方的封印区间（互相覆写 / 数据不可见）。
//
// 与 ErrConflictClaim 的分工：精确相等仍走 ErrConflictClaim（既有用例），本判据覆盖
// 「一方区域落在另一方之内」这类此前漏放的形态。
// 变异：去掉 validateFootprints → 下面前 4 例不再报错 → 红。
func TestParse_RejectsFootprintOverlap(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "basetest")
	cases := []struct {
		name     string
		pathA    string
		pathB    string
		conflict bool
	}{
		{"父吃子", "private", "private/vault", true},
		{"子被父吃", "private/vault", "private", true},
		{"整卷吃一切", "", "private", true},
		{"兄弟分支不冲突", "photos", "movies", false},
		{"深层兄弟不冲突", "a/photos", "a/movies", false},
		{"不同 owner 桶不冲突", "alice/user", "bob/user", false},
	}
	for _, tc := range cases {
		vols := []volume.Volume{
			{Name: "main"},
			encVol("a", "main:"+tc.pathA, nil),
			encVol("b", "main:"+tc.pathB, nil),
		}
		_, err := Parse(vols)
		if tc.conflict && !errors.Is(err, ErrFootprintOverlap) {
			t.Errorf("%s: %q vs %q 应拒绝(ErrFootprintOverlap), got %v", tc.name, tc.pathA, tc.pathB, err)
		}
		if !tc.conflict && err != nil {
			t.Errorf("%s: %q vs %q 不应冲突, got %v", tc.name, tc.pathA, tc.pathB, err)
		}
	}
}

// TestParse_ChainNestingAllowsFootprintOverlap 链内叠加（`outer: inner:`、`outer: inner:sub`）
// **允许**足迹重叠：外层变换的是内层已变换的字节，两者本就共用同一物理区间，封印象限由最外层
// 承担。变异：把链内判定去掉 → 本用例（master 既有的嵌套拓扑用例同形）变红。
func TestParse_ChainNestingAllowsFootprintOverlap(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "basetest")
	vols := []volume.Volume{
		{Name: "main"},
		encVol("inner", "main:base", nil),
		encVol("outer", "inner:", nil),     // 整内层（足迹 base ≡ inner）
		encVol("deeper", "outer:sub", nil), // 内层子区域（足迹 base/sub ⊂ base）
	}
	decls, err := Parse(vols)
	if err != nil {
		t.Fatalf("链内叠加应允许足迹重叠, got %v", err)
	}
	if len(decls) != 3 {
		t.Fatalf("应 3 条声明, got %d", len(decls))
	}
}

// TestParse_MixedChainRejected 混链（owner 限定与卷根专有混用）无法给出唯一足迹 ⇒ fail-closed。
func TestParse_MixedChainRejected(t *testing.T) {
	t.Parallel()
	registerTestWrapper(t, "basetest")
	vols := []volume.Volume{
		{Name: "main"},
		encVol("inner", "main:private/vault", nil), // 卷根专有
		encVol("outer", "inner:alice/user", nil),   // owner 限定（第二段 user 是保留桶名）
	}
	if _, err := Parse(vols); err == nil {
		t.Fatal("混链应 fail-closed 拒绝")
	}
}
