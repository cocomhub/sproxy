// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func newNormalizeFS(t *testing.T, root string) *SecretdataFS {
	t.Helper()
	s, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block: shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return s
}

// TestNormalizeContainerNames_MigratesAndIdempotent 钉住**容器名规范化**：
// 历史随机名容器逐个移动到 `DirNameFor` 确定性名下；dry-run 不移动；幂等（重跑 moved=0）；
// 迁移后**重启仍可读**（逻辑解析不依赖物理段名）。
func TestNormalizeContainerNames_MigratesAndIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	payload := bytes.Repeat([]byte("normalize-container-names-"), 8*1024)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "jav/makers/NHDTB-999/f.bin",
		bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}

	// dry-run：只统计不移动
	// 新建容器已是**确定性名**（机制 1）⇒ 为验证迁移，先把叶子容器人为改成「随机名」
	// （模拟历史遗留容器）。
	if merr := sfs.MakeDir(ctx, "rndlegacy"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	sfs.mu.RLock()
	legacy := sfs.dirSegs["rndlegacy"]
	sfs.mu.RUnlock()
	legacyNew := path.Join(path.Dir(legacy), "Rand0mLegacyName")
	if rerr := os.Rename(filepath.Join(root, filepath.FromSlash(legacy)), filepath.Join(root, filepath.FromSlash(legacyNew))); rerr != nil {
		t.Fatalf("改名为随机名: %v", rerr)
	}
	sfs.mu.Lock()
	sfs.dirSegs["rndlegacy"] = legacyNew
	sfs.mu.Unlock()

	sfs.mu.RLock()
	before := map[string]string{}
	for rel, c := range sfs.dirSegs {
		before[rel] = c
	}
	sfs.mu.RUnlock()
	movedDry, _, derr := sfs.NormalizeContainerNames(ctx, true)
	if derr != nil {
		t.Fatalf("dry-run: %v", derr)
	}
	if movedDry == 0 {
		t.Fatal("dry-run 应报告待迁移数量（随机名 ≠ 确定性名）")
	}
	sfs.mu.RLock()
	for rel, c := range sfs.dirSegs {
		if before[rel] != c {
			t.Fatalf("dry-run 不得改动 dirSegs[%s]: %s → %s", rel, before[rel], c)
		}
	}
	sfs.mu.RUnlock()

	// 实迁
	moved, _, merr := sfs.NormalizeContainerNames(ctx, false)
	if merr != nil {
		t.Fatalf("NormalizeContainerNames: %v", merr)
	}
	if moved == 0 {
		t.Fatal("应有容器被迁移")
	}
	// 断言：每个 rel 的段名 == DirNameFor(secret, 父段, rel)
	sfs.mu.RLock()
	segs := map[string]string{}
	for rel, c := range sfs.dirSegs {
		segs[rel] = c
	}
	sfs.mu.RUnlock()
	for rel, c := range segs {
		if rel == "" {
			continue
		}
		pc := segs[parentDirOf(rel)]
		want, werr := sfs.targetContainerPath(pc, rel)
		if werr != nil {
			t.Fatalf("targetContainerPath: %v", werr)
		}
		if c != want {
			t.Fatalf("rel=%s 应已规范化为 %s，got %s", rel, want, c)
		}
	}
	// 幂等：再跑应 0 迁移
	moved2, _, merr2 := sfs.NormalizeContainerNames(ctx, false)
	if merr2 != nil {
		t.Fatalf("第二次规范化: %v", merr2)
	}
	if moved2 != 0 {
		t.Fatalf("规范化必须幂等，第二次 moved=%d", moved2)
	}

	// 重启后仍可读（逻辑解析靠 DirID/扫描，不靠段名）
	sfs2 := newNormalizeFS(t, root)
	rc, rerr := sfs2.OpenRead(ctx, "jav/makers/NHDTB-999/f.bin")
	if rerr != nil {
		t.Fatalf("迁移后重启读取失败: %v", rerr)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("迁移后内容不一致: %d vs %d", len(got), len(payload))
	}
}

// TestAuditDuplicateContainers_DetectsConflict 钉住**迁移前置审计**：
// 落选容器含权威容器没有的条目 ⇒ Conflicted=true（禁止自动迁移，等人工确认）。
func TestAuditDuplicateContainers_DetectsConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	if merr := sfs.MakeDir(ctx, "a"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	sfs.mu.RLock()
	c1 := sfs.dirSegs["a"]
	sfs.mu.RUnlock()
	// 复制 C1 的 meta ⇒ 第二个容器声明同一逻辑目录（无子容器）
	dup := path.Join(path.Dir(c1), "DUPcontainerX")
	if cerr := copyContainerMetaOnly(t, root, c1, dup); cerr != nil {
		t.Fatalf("复制容器 meta: %v", cerr)
	}
	// 先挂载一次，取得该组的**落选容器**（权威选择依赖子容器数/mtime/DirID，不能预设）
	probe := newNormalizeFS(t, root)
	groups := probe.dupGroupsSnapshot()
	if len(groups) == 0 || len(groups[0].others) == 0 {
		t.Fatalf("应登记重复容器组，got %+v", groups)
	}
	victim := groups[0].others[0]
	// 在**落选容器**里放一个权威容器没有的条目 ⇒ 制造争议（可能丢数据的形态）
	if werr := os.WriteFile(filepath.Join(root, filepath.FromSlash(victim), "EXTRA_entry"), []byte("x"), 0o600); werr != nil {
		t.Fatalf("写争议条目: %v", werr)
	}

	sfs2 := newNormalizeFS(t, root)
	conflicts, err := sfs2.AuditDuplicateContainers(ctx)
	if err != nil {
		t.Fatalf("AuditDuplicateContainers: %v", err)
	}
	if len(conflicts) == 0 {
		t.Fatal("应发现重复容器组")
	}
	var found bool
	for _, c := range conflicts {
		if c.Conflicted {
			found = true
			if len(c.Details) == 0 {
				t.Fatal("争议组必须给出差异说明")
			}
		}
	}
	if !found {
		t.Fatalf("应标记为争议（落选容器含额外条目），got %+v", conflicts)
	}

	// 有争议 ⇒ 规范化必须**跳过该组**（不迁移）
	moved, _, merr := sfs2.NormalizeContainerNames(ctx, false)
	if merr != nil {
		t.Fatalf("NormalizeContainerNames: %v", merr)
	}
	_ = moved
	sfs2.mu.RLock()
	after := sfs2.dirSegs["a"]
	sfs2.mu.RUnlock()
	if after != groups[0].canonical {
		t.Fatalf("争议组不得被迁移（等人工确认），got %s（原 %s）", after, groups[0].canonical)
	}
}
