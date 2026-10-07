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
	if err := tv.WriteFile(ctx, "dir/a.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// .meta 落盘（inner 可见——占配额语义：meta 是底层文件之一）。
	e, err := inner.Stat(ctx, "dir/a.bin.meta")
	if err != nil || e == nil {
		t.Fatalf("meta sidecar 应存在: %v %v", e, err)
	}
	// meta 可读 + 反序列化校验（值独立重算比对）。
	rc, rerr := inner.OpenRead(ctx, "dir/a.bin.meta")
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
	// 隐藏过滤：ListDir 不含 .meta。
	es, lerr := tv.ListDir(ctx, "dir")
	if lerr != nil {
		t.Fatalf("ListDir: %v", lerr)
	}
	if len(es) != 1 || es[0].Name != "a.bin" {
		t.Fatalf("ListDir 应只含 a.bin（meta 隐藏），got %+v", es)
	}
	// Stat 也过滤 meta。
	if e, serr := tv.Stat(ctx, "dir/a.bin.meta"); serr != nil || e != nil {
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

// TestDisableMetaFile 关闭 meta 落盘（纯校验读取可用）。
func TestDisableMetaFile(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{DisableMetaFile: true})
	ctx := context.Background()
	data := []byte("no meta")
	if err := tv.WriteFile(ctx, "n.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if e, err := inner.Stat(ctx, "n.bin.meta"); err != nil || e != nil {
		t.Fatalf("DisableMetaFile 下不应落 meta: %v %v", e, err)
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
