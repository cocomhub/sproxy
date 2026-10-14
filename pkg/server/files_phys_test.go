// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

// TestFilesPhysFootprinter_SecretdataVolume 钉住**卷物理足迹能力真的被解析到**（P0 回归）。
//
// 为什么需要本测：适配器（`filesPhysFootprinter`）是「卷自报物理足迹」与「配额账本」之间**唯一**
// 的桥——桥断了不会报错，只会**静默回落到逻辑口径**（实测曾出现「递归 mkdir 配额增量为 0」，根因
// 是配额键用了用户可见路径 ⇒ `quotaScopeFor` 返回 nil）。故这里直接钉住「能力必须真的解析到 +
// 目录链足迹随 mkdir 逐级增长 + 小文件物理 ≥ meta pad」。
//
// 变异：把 physFS 换成 `syncpkg.Innermost`（当前等价，防未来回归）或让 physFS 恒 false ⇒ 本测红。
func TestFilesPhysFootprinter_SecretdataVolume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	be, err := secretdata.NewBackend(ctx, volume.Volume{Name: "vault", Type: "secretdata"}, inner,
		secretdata.Options{Secret: []byte("phys-adapter-key-0123"), MetaPadBytes: 4096, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("secretdata.NewBackend: %v", err)
	}
	// 卷集合先放一个本地卷（默认卷），再把 secretdata 作为**外部卷**加入（与生产装配同形）。
	mainDir := t.TempDir()
	mainRoot, rerr := storage.OpenRoot(mainDir)
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	vs := registry.NewSet([]volume.Volume{{Name: "main", Type: volume.TypeLocal, RootDir: mainDir}},
		map[string]*storage.Root{"main": mainRoot}, nil, nil, "main")
	t.Cleanup(func() { _ = vs.Close() })
	if aerr := vs.AddExternalVolume(volume.Volume{Name: "vault", Type: "secretdata"}, be); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	p := filesPhysFootprinter{h: &Handlers{volSet: vs}}

	// ① 文件足迹能力（PhysBound / PhysFootprint）必须可用。
	if got := p.PhysBound("vault", 4096); got <= 0 {
		t.Fatalf("PhysBound=%d：secretdata 文件足迹能力未被解析（Innermost 穿透过头？）", got)
	}
	// ② 目录足迹能力必须可用且随 mkdir 增长（每级一个 dir meta，pad ∈ [4096, 8192)）。
	before, okB := p.DirChainFootprint("vault", "alice", "a/b/c")
	if !okB {
		t.Fatal("DirChainFootprint 不可用：secretdata 目录足迹能力未被解析")
	}
	if merr := be.FS().MakeDir(ctx, "alice/user/a/b/c"); merr != nil {
		t.Fatalf("底层 MakeDir: %v", merr)
	}
	after, okA := p.DirChainFootprint("vault", "alice", "a/b/c")
	if !okA {
		t.Fatal("mkdir 后 DirChainFootprint 不可用")
	}
	if after-before < 3*4096 {
		t.Fatalf("mkdir a/b/c 后目录链足迹增量=%d，应 ≥ 3 级 dir meta（每级都要计入）", after-before)
	}
	// ③ 文件足迹实测：小文件的物理 ≥ meta pad（4KiB）。
	payload := bytes.Repeat([]byte("p"), 64)
	if werr := be.FS().WriteFile(ctx, "alice/user/a/b/c/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("底层 WriteFile: %v", werr)
	}
	fp, okF := p.PhysFootprint("vault", "alice", "a/b/c/f.bin")
	if !okF {
		t.Fatal("PhysFootprint 不可用：secretdata 文件足迹能力未被解析")
	}
	if fp < 4096 {
		t.Fatalf("64B 文件物理足迹=%d，应 ≥ meta pad 下限 4096（按逻辑记账会低估）", fp)
	}
}
