// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package trusted

// trusted_fs_test.go 钉住可信卷装饰器：写后生成 .meta（隐藏、占配额）、读列表过滤、
// 删除/重命名联动 meta。断言铁律：正例落到真实副作用（meta 文件存在且可反序列化
// 校验、校验值独立重算比对）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func newInner(t *testing.T) *syncpkg.LocalFS {
	t.Helper()
	return syncpkg.NewLocalFS(t.TempDir(), nil)
}

// refSHA 独立重算（防与实现同源错误）。
func refSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestWrap_ProviderShortCircuit 卷实现 meta.Provider → Wrap 返回原 fs（零封装，
// 交给卷处理——用户裁定 2026-10-07）。LocalFS 未实现 Provider → 返回装饰器。
func TestWrap_ProviderShortCircuit(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	got := Wrap(inner, Options{})
	if _, ok := got.(*TrustedVolumeFS); !ok {
		t.Fatal("LocalFS 未实现 Provider → Wrap 应返回装饰器")
	}
	prov := &providerFS{inner: inner}
	if got2 := Wrap(prov, Options{}); got2 != prov {
		t.Fatalf("实现 Provider 的卷 → Wrap 应返回原 fs（零封装），got %T", got2)
	}
}

// providerFS 是最小 meta.Provider 实现（用于 Wrap 短路断言）。
type providerFS struct{ inner syncpkg.FS }

func (p *providerFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	return p.inner.ListDir(ctx, path)
}
func (p *providerFS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	return p.inner.Stat(ctx, path)
}
func (p *providerFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return p.inner.OpenRead(ctx, path)
}
func (p *providerFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, path string, r io.Reader, size, mtime int64) error {
	return p.inner.WriteFile(ctx, key, path, r, size, mtime)
}
func (p *providerFS) Rename(ctx context.Context, from, to string) error {
	return p.inner.Rename(ctx, from, to)
}
func (p *providerFS) Delete(ctx context.Context, path string) error {
	return p.inner.Delete(ctx, path)
}
func (p *providerFS) MakeDir(ctx context.Context, path string) error {
	return p.inner.MakeDir(ctx, path)
}
func (p *providerFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	return &meta.FileMeta{Version: 1, Size: 0, TotalSHA256: strings.Repeat("a", 64), ChunkSize: 1}, nil
}

var _ meta.Provider = (*providerFS)(nil)

// TestWrite_GeneratesMeta 写文件 → .meta 生成（隐藏、占配额、可反序列化校验）。
func TestWrite_GeneratesMeta(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{})
	ctx := context.Background()
	data := bytes.Repeat([]byte("trusted content 可信内容 "), 200) // ~4KB
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/dir/a.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// .meta 落 meta 桶（inner 可见——占配额语义：meta 是底层文件之一）。
	e, err := inner.Stat(ctx, "meta/dir/a.bin.meta")
	if err != nil || e == nil {
		t.Fatalf("meta sidecar 应存在: %v %v", e, err)
	}
	// meta 可读 + 反序列化校验（值独立重算比对）。
	rc, rerr := inner.OpenRead(ctx, "meta/dir/a.bin.meta")
	if rerr != nil {
		t.Fatalf("OpenRead meta: %v", rerr)
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("meta 反序列化: %v", uerr)
	}
	if fm.TotalSHA256 != refSHA(data) {
		t.Fatalf("meta TotalSHA256 不一致: got %s", fm.TotalSHA256)
	}
	if fm.Size != int64(len(data)) {
		t.Fatalf("meta Size = %d, want %d", fm.Size, len(data))
	}
	// 隐藏过滤：ListDir 不含 meta 桶。
	es, lerr := tv.ListDir(ctx, "user/dir")
	if lerr != nil {
		t.Fatalf("ListDir: %v", lerr)
	}
	if len(es) != 1 || es[0].Name != "a.bin" {
		t.Fatalf("ListDir 应只含 a.bin（meta 隐藏），got %+v", es)
	}
	// Stat 也过滤 meta。
	if e, serr := tv.Stat(ctx, "meta/dir/a.bin.meta"); serr != nil || e != nil {
		t.Fatalf("Stat(meta) 应隐藏: %v %v", e, serr)
	}
}

// TestWrite_ExtraCarried 自定义扩展信息写入 meta（创建人/email 等）。
func TestWrite_ExtraCarried(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{Extra: map[string]any{"creator": "alice", "email": "a@x.io", "priority": "high"}})
	ctx := context.Background()
	data := []byte("extra test")
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "e.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, _ := inner.OpenRead(ctx, meta.MetaPath("e.bin"))
	raw, _ := io.ReadAll(rc)
	rc.Close()
	fm, err := meta.Unmarshal(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fm.Extra["creator"] != "alice" || fm.Extra["email"] != "a@x.io" || fm.Extra["priority"] != "high" {
		t.Fatalf("Extra 未保留: %+v", fm.Extra)
	}
}

// TestDelete_RemovesMeta 删除主文件联动删除 .meta。
func TestDelete_RemovesMeta(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{})
	ctx := context.Background()
	data := []byte("to delete")
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "d.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := tv.Delete(ctx, "d.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if e, err := inner.Stat(ctx, meta.MetaPath("d.bin")); err != nil || e != nil {
		t.Fatalf("删除主文件应联动删 meta: %v %v", e, err)
	}
}

// TestRename_MovesMeta 重命名联动移动 .meta。
func TestRename_MovesMeta(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{})
	ctx := context.Background()
	data := []byte("to rename")
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "r.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := tv.Rename(ctx, "r.bin", "sub/r2.bin"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if e, err := inner.Stat(ctx, meta.MetaPath("r.bin")); err == nil && e != nil {
		t.Fatal("旧 meta 应已移走")
	}
	if e, err := inner.Stat(ctx, meta.MetaPath("sub/r2.bin")); err != nil || e == nil {
		t.Fatalf("新位置 meta 应存在: %v %v", e, err)
	}
}

// TestMetaAlwaysGenerated 用户裁定：meta 恒生成（只能关校验、不能关 meta——sidecar
// 是完整性证据，写入路径必落；装饰器 Options 无 SkipMeta 开关）。
func TestMetaAlwaysGenerated(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{})
	ctx := context.Background()
	data := []byte("meta always")
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "n.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if e, err := inner.Stat(ctx, meta.MetaPath("n.bin")); err != nil || e == nil {
		t.Fatalf("meta 应恒生成（默认落盘）: %v %v", e, err)
	}
}

// TestWrite_ZeroByte 零字节文件写可信卷成功（A-CRITICAL 修复：空文件无分块，
// Validate 放行——touch/.gitkeep 场景不再失败）。
func TestWrite_ZeroByte(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{})
	ctx := context.Background()
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "empty.bin", bytes.NewReader(nil), 0, 0); err != nil {
		t.Fatalf("0 字节文件写入可信卷应成功: %v", err)
	}
	e, err := inner.Stat(ctx, "empty.bin")
	if err != nil || e == nil {
		t.Fatalf("0 字节文件应落盘: %v %v", e, err)
	}
}

// TestUnimplementedCapability_ErrUnsupported B1 修复：装饰器桩方法在 inner 未实现
// 可选能力时返回 ErrUnsupported（非硬错误）——调用方经 errors.Is 识别后回落，
// 与裸 FS 断言失败（ok=false）语义一致。
func TestUnimplementedCapability_ErrUnsupported(t *testing.T) {
	t.Parallel()
	inner := &noCapsFS{} // 仅实现 FS 基础方法，不实现任何可选能力
	tv, ok := Wrap(inner, Options{}).(*TrustedVolumeFS)
	if !ok {
		t.Fatalf("Wrap(noCapsFS) 应返回装饰器, got %T", tv)
	}
	ctx := context.Background()
	if _, err := tv.WriteIfAbsent(ctx, "x", bytes.NewReader(nil), 0, 0); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("WriteIfAbsent 未实现应 ErrUnsupported, got %v", err)
	}
	if err := tv.ReserveSpace(ctx, "x", 0); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("ReserveSpace 未实现应 ErrUnsupported, got %v", err)
	}
	if err := tv.Move(ctx, "a", "b"); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("Move 未实现应 ErrUnsupported, got %v", err)
	}
	if err := tv.Copy(ctx, "a", "b"); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("Copy 未实现应 ErrUnsupported, got %v", err)
	}
	if err := tv.Link(ctx, "a", "b"); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("Link 未实现应 ErrUnsupported, got %v", err)
	}
}

// noCapsFS 是最小 FS 基础实现（不实现任何可选能力：WriteIfAbsent/ReserveSpace/
// Mover/Copier/Linker/LocalVolume），供装饰器桩"未实现"路径断言用。
type noCapsFS struct{}

var _ syncpkg.FS = (*noCapsFS)(nil)

func (*noCapsFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (*noCapsFS) Stat(context.Context, string) (*syncpkg.Entry, error)     { return nil, nil }
func (*noCapsFS) OpenRead(context.Context, string) (io.ReadCloser, error)  { return nil, nil }
func (*noCapsFS) WriteFile(context.Context, syncpkg.LogicalKey, string, io.Reader, int64, int64) error {
	return nil
}
func (*noCapsFS) Rename(context.Context, string, string) error { return nil }
func (*noCapsFS) Delete(context.Context, string) error         { return nil }
func (*noCapsFS) MakeDir(context.Context, string) error        { return nil }

// TestMoveCopyLink_MetaLinkage C1 修复：Move/Copy 成功后联动 sidecar（目标带可信
// meta、源 meta 不孤儿）；Link 复制 sidecar。
func TestMoveCopyLink_MetaLinkage(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv, ok := Wrap(inner, Options{}).(*TrustedVolumeFS)
	if !ok {
		t.Fatalf("Wrap(LocalFS) 应返回装饰器, got %T", tv)
	}
	ctx := context.Background()
	data := []byte("meta-linkage")
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/f.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if e, err := inner.Stat(ctx, "meta/f.bin.meta"); err != nil || e == nil {
		t.Fatalf("写后应有 sidecar: %v %v", e, err)
	}
	// Move：目标带 sidecar、源 sidecar 迁移（不再孤儿）。
	if err := tv.Move(ctx, "user/f.bin", "user/g.bin"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if e, err := inner.Stat(ctx, "meta/g.bin.meta"); err != nil || e == nil {
		t.Fatalf("Move 后目标应有 sidecar: %v %v", e, err)
	}
	if e, err := inner.Stat(ctx, "meta/f.bin.meta"); err != nil || e != nil {
		t.Fatalf("Move 后源 sidecar 应随迁（不残留）: %v %v", e, err)
	}
	// Copy：目标带复制的 sidecar、源保留。
	if err := tv.Copy(ctx, "user/g.bin", "user/h.bin"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if e, err := inner.Stat(ctx, "meta/h.bin.meta"); err != nil || e == nil {
		t.Fatalf("Copy 后目标应有 sidecar: %v %v", e, err)
	}
	if e, err := inner.Stat(ctx, "meta/g.bin.meta"); err != nil || e == nil {
		t.Fatalf("Copy 后源 sidecar 应保留: %v %v", e, err)
	}
	// Link：目标复制源 sidecar、源保留。
	if err := tv.Link(ctx, "user/g.bin", "user/i.bin"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if e, err := inner.Stat(ctx, "meta/i.bin.meta"); err != nil || e == nil {
		t.Fatalf("Link 后目标应有 sidecar: %v %v", e, err)
	}
	if e, err := inner.Stat(ctx, "meta/g.bin.meta"); err != nil || e == nil {
		t.Fatalf("Link 后源 sidecar 应保留: %v %v", e, err)
	}
}

// TestTrustedVolumeFS_RenameDirMigratesMetaSubtree 回归（本轮 B 面）：目录级 Rename 必须把
// 子文件 sidecar 子树整棵迁移（meta/<from>/** → meta/<to>/**）——否则新位置逐分块校验
// 静默失效（读侧退化为不校验）且旧子树成孤儿白占 meta 桶配额。
// 变异：去掉 Rename 里的 relocateMetaSubtree 调用 → 子 sidecar 仍在 meta/dir/** → 红。
func TestTrustedVolumeFS_RenameDirMigratesMetaSubtree(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv, ok := Wrap(inner, Options{}).(*TrustedVolumeFS)
	if !ok {
		t.Fatalf("Wrap(LocalFS) 应返回装饰器, got %T", tv)
	}
	ctx := context.Background()
	for _, dir := range []string{"user/dir", "user/dir/sub"} {
		if err := tv.MakeDir(ctx, dir); err != nil {
			t.Fatalf("MakeDir(%s): %v", dir, err)
		}
	}
	for _, rel := range []string{"user/dir/a.bin", "user/dir/sub/b.bin"} {
		body := []byte("body-" + rel)
		if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(body), int64(len(body)), 0); err != nil {
			t.Fatalf("WriteFile(%s): %v", rel, err)
		}
	}

	if err := tv.Rename(ctx, "user/dir", "user/dir2"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	// 子 sidecar（含嵌套层）必须在新子树。
	for _, rel := range []string{"user/dir2/a.bin", "user/dir2/sub/b.bin"} {
		if e, err := inner.Stat(ctx, meta.MetaPath(rel)); err != nil || e == nil {
			t.Fatalf("%s 的 sidecar 应迁移到新位置（变异：不迁移 → 红）: e=%v err=%v", rel, e, err)
		}
	}
	// 旧子树不得残留 sidecar 文件（孤儿白占 meta 桶配额）。
	for _, rel := range []string{"user/dir/a.bin", "user/dir/sub/b.bin"} {
		if st, err := inner.Stat(ctx, meta.MetaPath(rel)); err != nil || st != nil {
			t.Fatalf("旧位置 %s 的 sidecar 应已随迁不残留: st=%v err=%v", rel, st, err)
		}
	}
	// 可信承诺仍成立：新位置可读到与内容一致的 sidecar。
	fm, err := tv.FileMeta(ctx, "user/dir2/a.bin")
	if err != nil || fm == nil || fm.TotalSHA256 == "" {
		t.Fatalf("新位置应能读到 sidecar（逐分块校验证据）: fm=%v err=%v", fm, err)
	}
}

// metaDirGateFS 是「meta 目录未建则写失败」的替身（**不实现 meta.Provider**，否则 Wrap
// 会短路返回原 FS 而不装饰）——用于验证 UpdateMetaExtra 落 sidecar 前先建父目录。
type metaDirGateFS struct {
	inner syncpkg.FS
	made  map[string]bool
}

func (d *metaDirGateFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return d.inner.ListDir(ctx, p)
}
func (d *metaDirGateFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return d.inner.Stat(ctx, p)
}
func (d *metaDirGateFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return d.inner.OpenRead(ctx, p)
}
func (d *metaDirGateFS) Rename(ctx context.Context, from, to string) error {
	return d.inner.Rename(ctx, from, to)
}
func (d *metaDirGateFS) Delete(ctx context.Context, p string) error { return d.inner.Delete(ctx, p) }
func (d *metaDirGateFS) MakeDir(_ context.Context, p string) error {
	d.made[p] = true
	return nil
}
func (d *metaDirGateFS) WriteFile(_ context.Context, key syncpkg.LogicalKey, p string, _ io.Reader, _ int64, _ int64) error {
	if !d.made[path.Dir(p)] {
		return errors.New("meta 目录未建: " + path.Dir(p))
	}
	return nil
}

// TestUpdateMetaExtra_EnsuresMetaDir 回归（MINOR 批）：UpdateMetaExtra 落 sidecar 前必须
// 先建目标父目录（与 persistCalcMeta/writeMetaAfter/Rename/Move 一致）——否则 `meta/` 目录
// 不存在的场景下写 sidecar 必失败被吞（Extra 更新静默丢失）。
// 变异：去掉 UpdateMetaExtra 里的 ensureMetaDir → WriteFile 报「meta 目录未建」→ 红。
func TestUpdateMetaExtra_EnsuresMetaDir(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	ctx := context.Background()
	// 预置合法 sidecar（FileMeta 前置校验需读到它）。
	sidecar := `{"version":1,"size":0,"total_sha256":"` + strings.Repeat("a", 64) + `","chunk_size":1,"chunks":null}`
	if err := inner.WriteFile(ctx, syncpkg.LogicalKey{}, "meta/x.bin.meta", strings.NewReader(sidecar), int64(len(sidecar)), 0); err != nil {
		t.Fatalf("预置 sidecar: %v", err)
	}
	gate := &metaDirGateFS{inner: inner, made: map[string]bool{}}
	tv := Wrap(gate, Options{}).(*TrustedVolumeFS)
	if err := tv.UpdateMetaExtra(ctx, "user/x.bin", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("UpdateMetaExtra 应先建 meta 目录再落盘: %v", err)
	}
	if !gate.made["meta"] {
		t.Fatalf("应显式建 meta 目录，实际 made=%v", gate.made)
	}
}

// TestCloneExtra_DeepCopiesNested 回归（MINOR 批）：Extra 必须**深**拷贝——maps.Clone 只
// 浅拷贝，嵌套 map/slice 与源共享底层数组，任一侧就地修改会互相污染（已落盘 meta 的并发
// 读者会看到撕裂数据）。变异：cloneExtra 改回 maps.Clone → 嵌套修改互相穿透 → 红。
func TestCloneExtra_DeepCopiesNested(t *testing.T) {
	t.Parallel()
	src := map[string]any{
		"creator": "alice",
		"nested":  map[string]any{"tags": []any{"a", "b"}},
		"list":    []string{"x"},
	}
	cp := cloneExtra(src)
	// 写在副本上：源不得受影响。
	cp["nested"].(map[string]any)["tags"].([]any)[0] = "changed"
	cp["list"].([]string)[0] = "changed"
	if got := src["nested"].(map[string]any)["tags"].([]any)[0]; got != "a" {
		t.Fatalf("副本嵌套修改穿透到源（浅拷贝）: tags[0]=%v", got)
	}
	if got := src["list"].([]string)[0]; got != "x" {
		t.Fatalf("副本切片修改穿透到源（浅拷贝）: list[0]=%v", got)
	}
	// 反向：源后续修改不得影响副本。
	src["nested"].(map[string]any)["tags"].([]any)[0] = "src-changed"
	if got := cp["nested"].(map[string]any)["tags"].([]any)[0]; got == "src-changed" {
		t.Fatalf("源修改穿透到副本（浅拷贝）: tags[0]=%v", got)
	}
	// nil 保真（nil in → nil out，与 maps.Clone 语义一致）。
	if cloneExtra(nil) != nil {
		t.Fatal("nil Extra 应保持 nil")
	}
}

// TestWrite_SizeDeclaredWrong_SelfHeals m1 修复：调用方 size 声明失真（小于实写）→
// 装饰器按实写分块覆盖和重设 Size，产出正确 meta 落盘（而非报错留半途文件）。
func TestWrite_SizeDeclaredWrong_SelfHeals(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{})
	ctx := context.Background()
	data := bytes.Repeat([]byte("m1-selfheal "), 100) // ~1.1KB
	// 声明 size 远小于实写（如 Content-Length 撒谎/传输截断误报）——WriteFile 应自愈
	// 产出基于实写的正确 meta，而非返回错误。
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/s.bin", bytes.NewReader(data), 10, 0); err != nil {
		t.Fatalf("size 声明失真应自愈（产出实写 meta 而非报错）: %v", err)
	}
	// 主文件实写完整。
	if e, err := inner.Stat(ctx, "user/s.bin"); err != nil || e == nil || e.Size != int64(len(data)) {
		t.Fatalf("主文件应完整落盘: %+v %v", e, err)
	}
	// meta 以实写 Size 落盘（分块覆盖和，非声明值）。
	rc, rerr := inner.OpenRead(ctx, "meta/s.bin.meta")
	if rerr != nil {
		t.Fatalf("meta 应落盘: %v", rerr)
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("meta 反序列化: %v", uerr)
	}
	if fm.Size != int64(len(data)) {
		t.Fatalf("meta Size 应修正为实写 %d, got %d", len(data), fm.Size)
	}
	if fm.TotalSHA256 != refSHA(data) {
		t.Fatalf("meta TotalSHA256 应为实写内容哈希: got %s", fm.TotalSHA256)
	}
	if err := meta.Validate(fm); err != nil {
		t.Fatalf("自愈后的 meta 应通过 Validate: %v", err)
	}
}

// TestWrite_ChunkSizeFloor A-MAJOR-5 回归：配置极小 chunk_size（1B）→ 钳到下界 1MiB，
// 大文件按 1B 分块会产生数百万 ChunkMeta（meta JSON GB 级 DoS）——防分块数爆炸。
func TestWrite_ChunkSizeFloor(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{ChunkSize: 1}) // 1B 分块（恶意配置）
	ctx := context.Background()
	data := bytes.Repeat([]byte("floor-chunk-"), 1000) // ~12KB
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/f.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, rerr := inner.OpenRead(ctx, "meta/f.bin.meta")
	if rerr != nil {
		t.Fatalf("meta 应落盘: %v", rerr)
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("meta 解析: %v", uerr)
	}
	// 钳到 1MiB 后 ~12KB 文件仅 1 块（非 12k 块）。
	if len(fm.Chunks) > 4 {
		t.Fatalf("1B 配置应钳到 1MiB 分块（~12KB 应 ≤4 块）, got %d", len(fm.Chunks))
	}
	if fm.Size != int64(len(data)) {
		t.Fatalf("meta Size = %d, want %d", fm.Size, len(data))
	}
}

// TestUpdateMetaExtra_C5 回归：UpdateMetaExtra 先读回校验主文件与旧 meta 一致才落盘
// （C5/E-MAJOR-2 修复——原先写旧哈希 meta 再读回比对，顺序反：并发覆盖时旧 meta
// 已覆盖新 meta 且不回滚）。测试：主文件与 meta 一致 → Extra 更新成功；主文件被
// 篡改（与 meta 不一致）→ 更新跳过（不覆盖 meta，无污染）。
func TestUpdateMetaExtra_C5(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv, ok := Wrap(inner, Options{}).(*TrustedVolumeFS)
	if !ok {
		t.Fatalf("Wrap(LocalFS) 应返回装饰器, got %T", tv)
	}
	ctx := context.Background()
	data := []byte("update-meta-extra-c5")
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/f.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 一致 → 更新成功，Extra 写入。
	if err := tv.UpdateMetaExtra(ctx, "user/f.bin", map[string]any{"source_integrity": "damaged"}); err != nil {
		t.Fatalf("UpdateMetaExtra: %v", err)
	}
	fm, ferr := tv.FileMeta(ctx, "user/f.bin")
	if ferr != nil {
		t.Fatalf("FileMeta: %v", ferr)
	}
	if got, ok := fm.Extra["source_integrity"]; !ok || got != "damaged" {
		t.Fatalf("Extra 应有 source_integrity=damaged, got %v", fm.Extra)
	}
	// 篡改主文件（内容变、meta 未变）→ 更新跳过（不覆盖 meta）。
	if err := inner.WriteFile(ctx, syncpkg.LogicalKey{}, "user/f.bin", bytes.NewReader(append([]byte("tampered-"), data...)), int64(len(data))+8, 0); err != nil {
		t.Fatalf("篡改写入: %v", err)
	}
	if err := tv.UpdateMetaExtra(ctx, "user/f.bin", map[string]any{"k": "v"}); err == nil {
		t.Fatal("篡改时更新应报错（可观测，不静默），而非 nil")
	}
	fm2, _ := tv.FileMeta(ctx, "user/f.bin")
	if _, has := fm2.Extra["k"]; has {
		t.Fatal("篡改时 Extra 更新应被跳过（不污染 meta）")
	}
}

// countingReadFS 记录 OpenRead 次数（断言 WriteIfAbsent 不再整文件回读算 meta）。
type countingReadFS struct {
	syncpkg.FS
	reads int
}

func (c *countingReadFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	c.reads++
	return c.FS.OpenRead(ctx, path)
}

func (c *countingReadFS) WriteIfAbsent(ctx context.Context, relPath string, r io.Reader, size, mtime int64) (bool, error) {
	w, ok := c.FS.(syncpkg.WriteIfAbsent)
	if !ok {
		return false, syncpkg.ErrUnsupported
	}
	return w.WriteIfAbsent(ctx, relPath, r, size, mtime)
}

// TestTrustedVolumeFS_WriteIfAbsent_NoReadBack 第 4 轮对抗评审 P1：WriteIfAbsent 必须
// 在写入流上流式算 meta（TeeReader），**不得**在写成功后整文件 OpenRead 回读——远端卷
// 回读=一次全量下载（6GiB → +6GiB，违背设计 §8）。
func TestTrustedVolumeFS_WriteIfAbsent_NoReadBack(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	cr := &countingReadFS{FS: inner}
	tv := Wrap(cr, Options{}).(*TrustedVolumeFS)
	ctx := context.Background()
	data := bytes.Repeat([]byte("stream-meta-"), 100)
	written, err := tv.WriteIfAbsent(ctx, "a.bin", bytes.NewReader(data), int64(len(data)), 0)
	if err != nil || !written {
		t.Fatalf("WriteIfAbsent: written=%v err=%v", written, err)
	}
	if cr.reads != 0 {
		t.Fatalf("WriteIfAbsent 不得回读主文件算 meta，OpenRead 次数=%d", cr.reads)
	}
	// meta 必须存在且可反序列化、TotalSHA256 与独立重算一致（真副作用断言）。
	rc, rerr := inner.OpenRead(ctx, meta.MetaPath("a.bin"))
	if rerr != nil {
		t.Fatalf("WriteIfAbsent 应流式落 meta: %v", rerr)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("meta 反序列化: %v", uerr)
	}
	if fm.TotalSHA256 != refSHA(data) {
		t.Fatalf("TotalSHA256=%s want %s", fm.TotalSHA256, refSHA(data))
	}
}

// TestTrustedVolumeFS_RenameStaleTargetMetaInvalidated R1-FS-2 回归：源无 sidecar 时
// rename 覆盖既有目标，目标旧 sidecar 必须失效（否则读路径按旧 meta 校验新内容恒失配）。
func TestTrustedVolumeFS_RenameStaleTargetMetaInvalidated(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{}).(*TrustedVolumeFS)
	ctx := context.Background()
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/t.bin", strings.NewReader("old-target"), 10, 0); err != nil {
		t.Fatal(err)
	}
	// 前置：目标应有 sidecar（**必须关闭句柄**：Windows 上未关句柄会让后续
	// Rename/Delete 因 sharing violation 失败 → 陈旧 sidecar 残留 → 断言假红）。
	if rc, rerr := inner.OpenRead(ctx, meta.MetaPath("user/t.bin")); rerr != nil {
		t.Fatalf("前置：目标应有 sidecar: %v", rerr)
	} else {
		_ = rc.Close()
	}
	// 源**无 sidecar**（直接经 inner 写，模拟旁路写/存量数据）。
	if err := inner.WriteFile(ctx, syncpkg.LogicalKey{}, "user/s.bin", strings.NewReader("new"), 3, 0); err != nil {
		t.Fatal(err)
	}
	if err := tv.Rename(ctx, "user/s.bin", "user/t.bin"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if rc, rerr := inner.OpenRead(ctx, meta.MetaPath("user/t.bin")); rerr == nil {
		_ = rc.Close()
		t.Fatal("源无 sidecar 时目标陈旧 sidecar 必须被失效（否则文件固化不可读）")
	}
}

// TestTrustedVolumeFS_CopyStaleTargetMetaInvalidated 同 R1-FS-2（Copy 路径）。
func TestTrustedVolumeFS_CopyStaleTargetMetaInvalidated(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{}).(*TrustedVolumeFS)
	ctx := context.Background()
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/t.bin", strings.NewReader("old-target"), 10, 0); err != nil {
		t.Fatal(err)
	}
	if err := inner.WriteFile(ctx, syncpkg.LogicalKey{}, "user/s.bin", strings.NewReader("new"), 3, 0); err != nil {
		t.Fatal(err)
	}
	if err := tv.Copy(ctx, "user/s.bin", "user/t.bin"); err != nil {
		if errors.Is(err, syncpkg.ErrUnsupported) {
			t.Skip("inner 未实现 Copy")
		}
		t.Fatalf("Copy: %v", err)
	}
	if rc, rerr := inner.OpenRead(ctx, meta.MetaPath("user/t.bin")); rerr == nil {
		_ = rc.Close() // Windows：未关句柄会让断言分支后的 TempDir 清理失败（sharing violation）
		t.Fatal("Copy 源无 sidecar 时目标陈旧 sidecar 必须被失效")
	}
}

// failMetaFS 主文件写成功、meta 路径写失败（注入 meta 落盘故障）。
type failMetaFS struct{ *syncpkg.LocalFS }

func (f *failMetaFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, rel string, r io.Reader, size, mtime int64) error {
	if rel == "meta" || strings.HasPrefix(rel, "meta/") || strings.Contains(rel, "/meta/") {
		return errors.New("injected: meta write failure")
	}
	return f.LocalFS.WriteFile(ctx, key, rel, r, size, mtime)
}

// TestTrustedVolumeFS_MetaWriteFailureKeepsMainFile F3：meta 落盘失败不得失败主写，
// 且必须失效旧 sidecar（退化为 missing=读侧不校验，而非陈旧 meta 固化）。
func TestTrustedVolumeFS_MetaWriteFailureKeepsMainFile(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{}).(*TrustedVolumeFS)
	ctx := context.Background()
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/a.bin", strings.NewReader("v1-content"), 10, 0); err != nil {
		t.Fatal(err)
	}
	stale := strings.Repeat("s", 64)
	if err := inner.WriteFile(ctx, syncpkg.LogicalKey{}, meta.MetaPath("user/a.bin"), strings.NewReader(stale), int64(len(stale)), 0); err != nil {
		t.Fatal(err)
	}
	fail := &failMetaFS{LocalFS: inner}
	tvF := Wrap(fail, Options{}).(*TrustedVolumeFS)
	if err := tvF.WriteFile(ctx, syncpkg.LogicalKey{}, "user/a.bin", strings.NewReader("v2-content"), 10, 0); err != nil {
		t.Fatalf("meta 失败不得失败主写: %v", err)
	}
	// 主文件已是新内容。
	rc, err := inner.OpenRead(ctx, "user/a.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "v2-content" {
		t.Fatalf("主文件 = %q", got)
	}
	if _, err := inner.OpenRead(ctx, meta.MetaPath("user/a.bin")); err == nil {
		t.Fatal("meta 落盘失败时旧陈旧 sidecar 必须被失效")
	}
}

// notLocalFS 自述非本地（IsLocalVolume=false）——覆盖 TrustedVolumeFS.isLocal() 的
// 「远端卷」分支（正例若用 LocalFS 会恒判本地，遮蔽该分支；F4 测试盲区）。
type notLocalFS struct{ *syncpkg.LocalFS }

func (n *notLocalFS) IsLocalVolume() bool { return false }

// TestTrustedVolumeFS_IsLocalBranch_RemoteSkipsReadback F4：非本地卷不执行整文件读回
// 交叉校验（verifyWriteMeta 早退），且写后 sidecar 仍正确生成。
func TestTrustedVolumeFS_IsLocalBranch_RemoteSkipsReadback(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	nl := &notLocalFS{LocalFS: inner}
	tv := Wrap(nl, Options{}).(*TrustedVolumeFS)
	if tv.isLocal() {
		t.Fatal("notLocalFS 应判为非本地（远端分支）")
	}
	ctx := context.Background()
	data := []byte("remote-content")
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/r.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatal(err)
	}
	rc, err := inner.OpenRead(ctx, meta.MetaPath("user/r.bin"))
	if err != nil {
		t.Fatalf("远端卷写后仍应生成 sidecar: %v", err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("sidecar 反序列化: %v", uerr)
	}
	if fm.TotalSHA256 != refSHA(data) {
		t.Fatalf("TotalSHA256=%s want %s", fm.TotalSHA256, refSHA(data))
	}
}

// remoteVerifyFS 模拟**远端卷**（不实现 LocalVolume → isLocal()==false），可注入 Stat size
// 偏差与末块内容篡改，用于验证 verifyWriteMeta 的远端轻量分支（M9c）。
type remoteVerifyFS struct {
	*syncpkg.LocalFS
	sizeDelta   int64
	corruptLast bool
}

// IsLocalVolume 覆盖提升自 *LocalFS 的实现：本替身模拟**远端**卷（isLocal()==false）。
func (r *remoteVerifyFS) IsLocalVolume() bool { return false }

func (r *remoteVerifyFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	e, err := r.LocalFS.Stat(ctx, p)
	if err == nil && e != nil && !e.IsDir && r.sizeDelta != 0 {
		e.Size += r.sizeDelta
	}
	return e, err
}

func (r *remoteVerifyFS) OpenRangeRead(ctx context.Context, p string, offset, size int64) (io.ReadCloser, error) {
	rc, err := r.LocalFS.OpenRangeRead(ctx, p, offset, size)
	if err != nil || !r.corruptLast {
		return rc, err
	}
	b, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		return nil, rerr
	}
	if len(b) > 0 {
		b[0] ^= 0xFF // 末块内容篡改（长度不变）
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// TestTrustedVolumeFS_VerifyWriteMetaRemote 回归（M9c，第 7 轮 MAJOR）：远端卷写后必须做
// **轻量**交叉校验（size + 末块），不得完全跳过——否则远端静默截断/并发覆盖错位无任何检测
// （同一写路径本地卷有校验、远端卷没有 = 承诺不一致）。
// 变异：verifyWriteMeta 的远端分支改回直接 return → 两个不一致用例的 sidecar 不被失效 → 红。
func TestTrustedVolumeFS_VerifyWriteMetaRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	body := bytes.Repeat([]byte("remote-verify-"), 300) // 3900B（单块）

	// ① size 不一致（远端静默截断/写错对象）→ sidecar 必须被失效。
	inner1 := newInner(t)
	tv1 := Wrap(&remoteVerifyFS{LocalFS: inner1, sizeDelta: -7}, Options{}).(*TrustedVolumeFS)
	if err := tv1.WriteFile(ctx, syncpkg.LogicalKey{}, "user/a.bin", bytes.NewReader(body), int64(len(body)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if e, err := inner1.Stat(ctx, meta.MetaPath("user/a.bin")); err == nil && e != nil {
		t.Fatal("远端 size 与 meta.Size 不一致时应失效 sidecar（M9c）")
	}

	// ② 末块内容不一致（尾部静默损坏）→ sidecar 必须被失效。
	inner2 := newInner(t)
	tv2 := Wrap(&remoteVerifyFS{LocalFS: inner2, corruptLast: true}, Options{}).(*TrustedVolumeFS)
	if err := tv2.WriteFile(ctx, syncpkg.LogicalKey{}, "user/b.bin", bytes.NewReader(body), int64(len(body)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if e, err := inner2.Stat(ctx, meta.MetaPath("user/b.bin")); err == nil && e != nil {
		t.Fatal("远端末块 sha256 不一致时应失效 sidecar（M9c）")
	}

	// ③ 一致（正常远端）→ sidecar 必须保留（不误伤）。
	inner3 := newInner(t)
	tv3 := Wrap(&remoteVerifyFS{LocalFS: inner3}, Options{}).(*TrustedVolumeFS)
	if err := tv3.WriteFile(ctx, syncpkg.LogicalKey{}, "user/c.bin", bytes.NewReader(body), int64(len(body)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if e, err := inner3.Stat(ctx, meta.MetaPath("user/c.bin")); err != nil || e == nil {
		t.Fatalf("一致时 sidecar 应保留（M9c 不误伤正常写）: e=%v err=%v", e, err)
	}
}

// TestTrustedVolumeFS_VerifyWriteMetaLocalLightweight（m9）：本地卷写后交叉校验与远端**统一**
// 为有界轻量判据（Stat size + 末块 sha256），不再整文件读回（6GiB 级写原每次付一次全量读）。
//
//	① 同 size + 末块相同、仅**中段**被并发覆盖 → 轻量判据**不检出**（m9 如实声明的放弃项，
//	   由读路径逐分块校验兜底）→ sidecar 保留；
//	② 末块篡改 → sidecar 失效；③ size 不一致 → sidecar 失效。
//
// 变异：去掉末块判据（只留 size）→ ② 红；改回整文件读回重算 TotalSHA256 → ① 红。
func TestTrustedVolumeFS_VerifyWriteMetaLocalLightweight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	body := bytes.Repeat([]byte("abcd"), 1024) // 4096B → 4 块（ChunkSize 1024）
	inner := newInner(t)
	tv := Wrap(inner, Options{}).(*TrustedVolumeFS)
	calc, cerr := meta.NewCalculator(int64(len(body)), 1024)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if _, rerr := calc.ReadFrom(bytes.NewReader(body)); rerr != nil {
		t.Fatal(rerr)
	}
	fm := calc.Finish()
	const rel = "user/m.bin"
	mrel := meta.MetaPath(rel)
	writeMain := func(data []byte) {
		t.Helper()
		if err := inner.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(data), int64(len(data)), 0); err != nil {
			t.Fatal(err)
		}
	}
	writeSidecar := func() {
		t.Helper()
		_ = inner.Delete(ctx, mrel)
		if err := inner.WriteFile(ctx, syncpkg.LogicalKey{}, mrel, bytes.NewReader([]byte(`{"version":1}`)), 14, 0); err != nil {
			t.Fatal(err)
		}
	}
	sidecarExists := func() bool {
		e, err := inner.Stat(ctx, mrel)
		return err == nil && e != nil
	}

	// ① 中段篡改（同 size、末块不变）：轻量判据不检出 → sidecar 保留。
	writeMain(body)
	writeSidecar()
	mid := append([]byte(nil), body...)
	mid[100] ^= 0xFF
	writeMain(mid)
	tv.verifyWriteMeta(ctx, rel, fm)
	if !sidecarExists() {
		t.Fatal("中段篡改（末块不变）在轻量判据下不应失效 sidecar（红 = 已改回整文件读回）")
	}

	// ② 末块篡改 → 失效。
	writeSidecar()
	tail := append([]byte(nil), body...)
	tail[len(tail)-1] ^= 0xFF
	writeMain(tail)
	tv.verifyWriteMeta(ctx, rel, fm)
	if sidecarExists() {
		t.Fatal("末块 sha256 不一致时应失效 sidecar（末块判据丢失 → 红）")
	}

	// ③ size 不一致 → 失效。
	writeSidecar()
	writeMain(body[:len(body)-7])
	tv.verifyWriteMeta(ctx, rel, fm)
	if sidecarExists() {
		t.Fatal("size 不一致时应失效 sidecar")
	}
}

// seekSpyFS 模拟「只经 ReadAt 多遍随机读」的 inner（如 secretdata 流式写路径）：
// 记录传入 WriteFile 的 reader 是否可 seek，并按 ReadAt 读两遍重建内容——若 trusted 仍
// 用 io.TeeReader（既剥 seek 又让多遍读下 meta 计算器收不到字节），meta 将错误/缺失。
type seekSpyFS struct {
	*syncpkg.LocalFS
	seekable atomic.Bool
	sawMain  atomic.Bool
}

func (s *seekSpyFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	if !strings.HasSuffix(p, ".meta") {
		_, ok := r.(io.Seeker)
		s.seekable.Store(ok)
		s.sawMain.Store(true)
		if ra, rok := r.(io.ReaderAt); rok && size > 0 {
			buf := make([]byte, size)
			_, _ = ra.ReadAt(buf, 0) // 第一遍（模拟哈希 pass）
			_, _ = ra.ReadAt(buf, 0) // 第二遍（模拟分块 pass）
			return s.LocalFS.WriteFile(ctx, key, p, bytes.NewReader(buf), size, mtime)
		}
	}
	return s.LocalFS.WriteFile(ctx, key, p, r, size, mtime)
}

// TestWrite_SeekableSourcePreservesSeekAndMeta：可 seek 源 → inner 收到**可 seek** reader
// （流式生效）+ meta 正确（meta 计算器由独立一遍喂满，不受 inner 多遍 ReadAt 影响）。
func TestWrite_SeekableSourcePreservesSeekAndMeta(t *testing.T) {
	t.Parallel()
	inner := &seekSpyFS{LocalFS: newInner(t)}
	tv := Wrap(inner, Options{})
	ctx := context.Background()
	data := bytes.Repeat([]byte("seekable content 可流式 "), 300)
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/s.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !inner.sawMain.Load() {
		t.Fatal("inner 未收到主文件写入")
	}
	if !inner.seekable.Load() {
		t.Fatal("可 seek 源应把可 seek reader 传给 inner（tee 会剥 seek → 流式失效）")
	}
	// meta 完整性（多遍 ReadAt inner 下 tee 会失效，独立一遍喂满则应正确）。
	raw, rerr := readMetaFile(t, inner, "meta/s.bin.meta")
	if rerr != nil {
		t.Fatalf("读 meta: %v", rerr)
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("meta 反序列化: %v", uerr)
	}
	if fm.TotalSHA256 != refSHA(data) {
		t.Fatalf("meta TotalSHA256 错误（meta 计算器未收满字节）: got %s want %s", fm.TotalSHA256, refSHA(data))
	}
}

// TestWrite_NonSeekableSourceFallsBackToTee：不可 seek 源 → inner 收到非 seek reader（回退
// 单遍 tee），meta 仍正确。
func TestWrite_NonSeekableSourceFallsBackToTee(t *testing.T) {
	t.Parallel()
	inner := &seekSpyFS{LocalFS: newInner(t)}
	tv := Wrap(inner, Options{})
	ctx := context.Background()
	data := bytes.Repeat([]byte("nonseek 纯流 "), 300)
	plain := struct{ io.Reader }{bytes.NewReader(data)}
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/n.bin", plain, int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if inner.seekable.Load() {
		t.Fatal("不可 seek 源不应被标记可 seek")
	}
	raw, rerr := readMetaFile(t, inner, "meta/n.bin.meta")
	if rerr != nil {
		t.Fatalf("读 meta: %v", rerr)
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("meta 反序列化: %v", uerr)
	}
	if fm.TotalSHA256 != refSHA(data) {
		t.Fatalf("meta TotalSHA256 错误: got %s", fm.TotalSHA256)
	}
}

// readMetaFile 读 inner 上 meta sidecar 内容。
func readMetaFile(t *testing.T, inner syncpkg.FS, rel string) ([]byte, error) {
	t.Helper()
	rc, err := inner.OpenRead(context.Background(), rel)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// TestUpdateMetaExtra_RemoteBoundedPrecheck 回归（第 7 轮评审 ⑤ M9c 残余）：远端卷的
// UpdateMetaExtra 此前**完全跳过**前置交叉校验（整文件回读被 isLocal 门挡住），并发覆盖
// 只能靠**写后**的有界判据发现——而那时 meta 已被污染写入。改为远端也用**有界轻量**前置判据
// （size + 末块 sha256，与 verifyWriteMeta 同口径）：确认不一致 → 拒绝更新、**根本不动 meta**。
//
// 变异：远端分支跳过前置校验 → 不一致时 UpdateMetaExtra 返回 nil（仅写后失效 meta）→ 红。
func TestUpdateMetaExtra_RemoteBoundedPrecheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	body := bytes.Repeat([]byte("remote-extra-"), 300)

	// ① 一致 → 更新成功（远端有界前置判据不得误杀正常路径）。
	inner1 := newInner(t)
	tv1 := Wrap(&remoteVerifyFS{LocalFS: inner1}, Options{}).(*TrustedVolumeFS)
	if err := tv1.WriteFile(ctx, syncpkg.LogicalKey{}, "user/a.bin", bytes.NewReader(body), int64(len(body)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := tv1.UpdateMetaExtra(ctx, "user/a.bin", map[string]any{"source_integrity": "damaged"}); err != nil {
		t.Fatalf("远端一致时更新应成功: %v", err)
	}

	// ② size 错位（并发覆盖/截断）→ 拒绝更新，且**未动 meta**（区分于写后失效）。
	inner2 := newInner(t)
	tv2 := Wrap(&remoteVerifyFS{LocalFS: inner2}, Options{}).(*TrustedVolumeFS)
	if err := tv2.WriteFile(ctx, syncpkg.LogicalKey{}, "user/b.bin", bytes.NewReader(body), int64(len(body)), 0); err != nil {
		t.Fatal(err)
	}
	truncated := body[:len(body)-5]
	if err := inner2.WriteFile(ctx, syncpkg.LogicalKey{}, "user/b.bin", bytes.NewReader(truncated), int64(len(truncated)), 0); err != nil {
		t.Fatal(err)
	}
	if err := tv2.UpdateMetaExtra(ctx, "user/b.bin", map[string]any{"k": "v"}); err == nil {
		t.Fatal("远端 size 错位时更新应报错（有界前置校验）")
	}
	if _, serr := inner2.Stat(ctx, meta.MetaPath("user/b.bin")); serr != nil {
		t.Fatal("前置校验拒绝时不得动 meta（sidecar 应原样保留）")
	}

	// ③ 同长度、末块被改写（size 判据不可见）→ 末块 sha256 判据必须拒绝。
	inner3 := newInner(t)
	tv3 := Wrap(&remoteVerifyFS{LocalFS: inner3}, Options{}).(*TrustedVolumeFS)
	if err := tv3.WriteFile(ctx, syncpkg.LogicalKey{}, "user/c.bin", bytes.NewReader(body), int64(len(body)), 0); err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-1] ^= 0xFF
	if err := inner3.WriteFile(ctx, syncpkg.LogicalKey{}, "user/c.bin", bytes.NewReader(tampered), int64(len(tampered)), 0); err != nil {
		t.Fatal(err)
	}
	if err := tv3.UpdateMetaExtra(ctx, "user/c.bin", map[string]any{"k": "v"}); err == nil {
		t.Fatal("远端末块不一致时更新应报错（有界前置校验）")
	}
	if _, serr := inner3.Stat(ctx, meta.MetaPath("user/c.bin")); serr != nil {
		t.Fatal("前置校验拒绝时不得动 meta（sidecar 应原样保留）")
	}
}

// TestUpdateMetaExtra_ModeReplaceDelete 钉住补录的 merge/replace/delete 三模式
// （用户裁定 2026-10-26：默认新增非覆盖；replace 整表替换；delete 删键）。
// 变异：把 replace 分支改回 merge → 本用例红。
func TestUpdateMetaExtra_ModeReplaceDelete(t *testing.T) {
	t.Parallel()
	// 用既有 TestUpdateMetaExtra 的装配模式（直接构造最小 TrustedVolumeFS 不可行——
	// 走真实装配：见 trusted_fs_test.go 既有用例的 fake 方式）。
	// 此处改为纯函数级验证 applyMetaUpdateMode（同包私有函数）。
	fm := &meta.FileMeta{Extra: map[string]any{"a": "1", "b": "2"}}
	// merge：同名更新、其余保留
	applyMetaUpdateMode(syncpkg.WithMetaUpdateMode(context.Background(), syncpkg.MetaUpdateMerge), fm, map[string]any{"a": "9", "c": "3"})
	if fm.Extra["a"] != "9" || fm.Extra["b"] != "2" || fm.Extra["c"] != "3" {
		t.Fatalf("merge 语义错: %v", fm.Extra)
	}
	// replace：整表替换
	applyMetaUpdateMode(syncpkg.WithMetaUpdateMode(context.Background(), syncpkg.MetaUpdateReplace), fm, map[string]any{"z": "only"})
	if len(fm.Extra) != 1 || fm.Extra["z"] != "only" {
		t.Fatalf("replace 语义错: %v", fm.Extra)
	}
	// delete：删指定键
	applyMetaUpdateMode(syncpkg.WithMetaUpdateMode(context.Background(), syncpkg.MetaUpdateDelete), fm, map[string]any{"z": nil})
	if len(fm.Extra) != 0 {
		t.Fatalf("delete 语义错: %v", fm.Extra)
	}
	// 默认（无 ctx 模式）= merge
	applyMetaUpdateMode(context.Background(), fm, map[string]any{"k": "v"})
	if fm.Extra["k"] != "v" {
		t.Fatalf("默认应 merge: %v", fm.Extra)
	}
}
