// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// viewBackend 把源视图原样作为后端 FS（用于端到端验证「封装卷可写、源直连被拒」）。
type viewBackend struct{ fs syncpkg.FS }

func (b *viewBackend) FS() syncpkg.FS { return b.fs }
func (b *viewBackend) Close() error   { return nil }

// viewWrapper 返回持有源视图的后端。
type viewWrapper struct{}

func (viewWrapper) Name() string { return "attachview" }

func (viewWrapper) Build(_ context.Context, _ volume.Volume, src syncpkg.FS, _ map[string]any) (registry.ExternalBackend, error) {
	return &viewBackend{fs: src}, nil
}

var attachViewOnce sync.Once

func registerViewWrapper() {
	attachViewOnce.Do(func() { enclosure.RegisterWrapper(viewWrapper{}) })
}

// newAttachedEnclosureSet 构造「源卷已封印 + 封装卷已补装」的卷集合。
func newAttachedEnclosureSet(t *testing.T, sealPath string) (*registry.Set, *storage.Root, string) {
	t.Helper()
	registerViewWrapper()
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	vols := []volume.Volume{
		{Name: "main", Type: volume.TypeLocal, RootDir: dir},
		{Name: "vault", Type: "attachview", Extra: map[string]any{
			"source": "main:" + sealPath,
		}},
	}
	// 按**生产装配序**：先就地累积（pools + volumes）→ applyEnclosureSeals（注入封印 + 配额父链
	// 继承）→ NewSet → AttachAll（推迟装配补装）。
	acc := &volumeAccum{
		roots:       map[string]*storage.Root{"main": rt},
		pools:       map[string]*quota.Pool{"main": quota.NewPool(0), "vault": quota.NewPool(0)},
		volumes:     vols,
		defaultName: "main",
	}
	if serr := applyEnclosureSeals(nil, acc); serr != nil {
		t.Fatalf("applyEnclosureSeals: %v", serr)
	}
	set := registry.NewSet(acc.volumes, acc.roots, nil, acc.pools, "main")
	if aerr := enclosure.AttachAll(context.Background(), set, nil); aerr != nil {
		t.Fatalf("AttachAll: %v", aerr)
	}
	return set, rt, dir
}

// TestEnclosureAttach_UnsealedWriteOK_SourceDirectDenied 设计核心承诺（评审「tests」角度点名
// 无测试）：**封装卷自身不受封印影响**（经服务器特权源视图读写成功），而同路径**源卷直连**被拒。
// 变异：把 attach.sourceFS 改成封印感知的 NewRootFS → 封装写入被拒 → 红；删掉 AttachAll 调用 → 后端 nil → 红。
func TestEnclosureAttach_UnsealedWriteOK_SourceDirectDenied(t *testing.T) {
	t.Parallel()
	const sealPath = "private/vault"
	set, rt, _ := newAttachedEnclosureSet(t, sealPath)

	be := set.External("vault")
	if be == nil {
		t.Fatal("封装卷后端应已补装")
	}
	// ① 封装卷写成功（不受封印影响）。
	if werr := be.FS().WriteFile(context.Background(), syncpkg.LogicalKey{}, "a.bin", bytes.NewReader([]byte("plain-data")), 9, 0); werr != nil {
		t.Fatalf("封装卷写入不应被封印影响: %v", werr)
	}
	// ② 数据确实落在源卷的封装区间（前缀映射生效）。
	raw, rerr := os.ReadFile(filepath.Join(rt.AbsPath(), sealPath, "a.bin"))
	if rerr != nil {
		t.Fatalf("封装数据应落在 %s 下: %v", sealPath, rerr)
	}
	if string(raw) != "plain-data" {
		t.Fatalf("落盘内容应为 plain-data, got %q", raw)
	}
	// ③ 源卷直连同路径仍被封印（写拒 / 读 404）。
	if werr := rt.Remove(filepath.Join(sealPath, "a.bin")); !errors.Is(werr, storage.ErrSealed) {
		t.Fatalf("源卷直连删除必须被拒(ErrSealed), got %v", werr)
	}
	if _, serr := rt.Open(filepath.Join(sealPath, "a.bin")); !errors.Is(serr, storage.ErrSealed) {
		t.Fatalf("源卷直连读取必须被拒(ErrSealed), got %v", serr)
	}
	// ④ 封装卷读回（经服务器处理）不受影响。
	rc, oerr := be.FS().OpenRead(context.Background(), "a.bin")
	if oerr != nil {
		t.Fatalf("封装卷读回: %v", oerr)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "plain-data" {
		t.Fatalf("封装卷读回应为 plain-data, got %q", got)
	}
}

// TestEnclosureAttach_InheritsSourcePool 评审 P1/P2 点名无测试：封装卷容量池必须**挂到源卷池**
// （默认继承、父链聚合不双计），而非独立计账。
// 变异：inheritPools 去掉（或装配后替换池）→ Parent()==nil → 红。
func TestEnclosureAttach_InheritsSourcePool(t *testing.T) {
	t.Parallel()
	set, _, _ := newAttachedEnclosureSet(t, "private/vault")
	vaultPool := set.Pool("vault")
	mainPool := set.Pool("main")
	if vaultPool == nil || mainPool == nil {
		t.Fatal("两卷池应存在")
	}
	if vaultPool.Parent() != mainPool {
		t.Fatalf("封装卷池必须挂到源卷池之下（继承语义），got parent=%v", vaultPool.Parent())
	}
	// 经封装卷写入 → 占用沿父链聚合（同一字节只计一次：父池 == 子池占用，不翻倍）。
	be := set.External("vault")
	if werr := be.FS().WriteFile(context.Background(), syncpkg.LogicalKey{}, "b.bin", bytes.NewReader(make([]byte, 10)), 10, 0); werr != nil {
		t.Fatalf("写入: %v", werr)
	}
	if got := vaultPool.Usage(); got != 10 {
		t.Fatalf("封装卷池应记 10, got %d", got)
	}
	if got := mainPool.Usage(); got != 10 {
		t.Fatalf("源卷池应聚合同 10（不双计）, got %d", got)
	}
}
