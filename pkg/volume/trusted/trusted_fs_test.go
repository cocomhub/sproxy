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
	"strings"
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
func (p *providerFS) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	return p.inner.WriteFile(ctx, path, r, size, mtime)
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
	if err := tv.WriteFile(ctx, "user/dir/a.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
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
	if err := tv.WriteFile(ctx, "e.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, _ := inner.OpenRead(ctx, "e.bin.meta")
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
	if err := tv.WriteFile(ctx, "d.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := tv.Delete(ctx, "d.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if e, err := inner.Stat(ctx, "d.bin.meta"); err != nil || e != nil {
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
	if err := tv.WriteFile(ctx, "r.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := tv.Rename(ctx, "r.bin", "sub/r2.bin"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if e, err := inner.Stat(ctx, "r.bin.meta"); err == nil && e != nil {
		t.Fatal("旧 meta 应已移走")
	}
	if e, err := inner.Stat(ctx, "sub/r2.bin.meta"); err != nil || e == nil {
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
	if err := tv.WriteFile(ctx, "n.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if e, err := inner.Stat(ctx, "n.bin.meta"); err != nil || e == nil {
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
	if err := tv.WriteFile(ctx, "empty.bin", bytes.NewReader(nil), 0, 0); err != nil {
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

func (*noCapsFS) ListDir(context.Context, string) ([]syncpkg.Entry, error)         { return nil, nil }
func (*noCapsFS) Stat(context.Context, string) (*syncpkg.Entry, error)             { return nil, nil }
func (*noCapsFS) OpenRead(context.Context, string) (io.ReadCloser, error)          { return nil, nil }
func (*noCapsFS) WriteFile(context.Context, string, io.Reader, int64, int64) error { return nil }
func (*noCapsFS) Rename(context.Context, string, string) error                     { return nil }
func (*noCapsFS) Delete(context.Context, string) error                             { return nil }
func (*noCapsFS) MakeDir(context.Context, string) error                            { return nil }

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
	if err := tv.WriteFile(ctx, "user/f.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
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
	if err := tv.WriteFile(ctx, "user/s.bin", bytes.NewReader(data), 10, 0); err != nil {
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
	if err := tv.WriteFile(ctx, "user/f.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
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
	if err := tv.WriteFile(ctx, "user/f.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
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
	if err := inner.WriteFile(ctx, "user/f.bin", bytes.NewReader(append([]byte("tampered-"), data...)), int64(len(data))+8, 0); err != nil {
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
	if err := tv.WriteFile(ctx, "user/t.bin", strings.NewReader("old-target"), 10, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := inner.OpenRead(ctx, meta.MetaPath("user/t.bin")); err != nil {
		t.Fatalf("前置：目标应有 sidecar: %v", err)
	}
	// 源**无 sidecar**（直接经 inner 写，模拟旁路写/存量数据）。
	if err := inner.WriteFile(ctx, "user/s.bin", strings.NewReader("new"), 3, 0); err != nil {
		t.Fatal(err)
	}
	if err := tv.Rename(ctx, "user/s.bin", "user/t.bin"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := inner.OpenRead(ctx, meta.MetaPath("user/t.bin")); err == nil {
		t.Fatal("源无 sidecar 时目标陈旧 sidecar 必须被失效（否则文件固化不可读）")
	}
}

// TestTrustedVolumeFS_CopyStaleTargetMetaInvalidated 同 R1-FS-2（Copy 路径）。
func TestTrustedVolumeFS_CopyStaleTargetMetaInvalidated(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{}).(*TrustedVolumeFS)
	ctx := context.Background()
	if err := tv.WriteFile(ctx, "user/t.bin", strings.NewReader("old-target"), 10, 0); err != nil {
		t.Fatal(err)
	}
	if err := inner.WriteFile(ctx, "user/s.bin", strings.NewReader("new"), 3, 0); err != nil {
		t.Fatal(err)
	}
	if err := tv.Copy(ctx, "user/s.bin", "user/t.bin"); err != nil {
		if errors.Is(err, syncpkg.ErrUnsupported) {
			t.Skip("inner 未实现 Copy")
		}
		t.Fatalf("Copy: %v", err)
	}
	if _, err := inner.OpenRead(ctx, meta.MetaPath("user/t.bin")); err == nil {
		t.Fatal("Copy 源无 sidecar 时目标陈旧 sidecar 必须被失效")
	}
}

// failMetaFS 主文件写成功、meta 路径写失败（注入 meta 落盘故障）。
type failMetaFS struct{ *syncpkg.LocalFS }

func (f *failMetaFS) WriteFile(ctx context.Context, rel string, r io.Reader, size, mtime int64) error {
	if rel == "meta" || strings.HasPrefix(rel, "meta/") || strings.Contains(rel, "/meta/") {
		return errors.New("injected: meta write failure")
	}
	return f.LocalFS.WriteFile(ctx, rel, r, size, mtime)
}

// TestTrustedVolumeFS_MetaWriteFailureKeepsMainFile F3：meta 落盘失败不得失败主写，
// 且必须失效旧 sidecar（退化 missing 直算，而非陈旧 meta 固化）。
func TestTrustedVolumeFS_MetaWriteFailureKeepsMainFile(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{}).(*TrustedVolumeFS)
	ctx := context.Background()
	if err := tv.WriteFile(ctx, "user/a.bin", strings.NewReader("v1-content"), 10, 0); err != nil {
		t.Fatal(err)
	}
	stale := strings.Repeat("s", 64)
	if err := inner.WriteFile(ctx, meta.MetaPath("user/a.bin"), strings.NewReader(stale), int64(len(stale)), 0); err != nil {
		t.Fatal(err)
	}
	fail := &failMetaFS{LocalFS: inner}
	tvF := Wrap(fail, Options{}).(*TrustedVolumeFS)
	if err := tvF.WriteFile(ctx, "user/a.bin", strings.NewReader("v2-content"), 10, 0); err != nil {
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
	if err := tv.WriteFile(ctx, "user/r.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
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
