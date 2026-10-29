// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// search_checksum_lazy_test.go 钉住搜索的 checksum 取用口径（2026-10-30 性能收敛）：
// `Search` 不再每请求 `GetAll()` 全量拷贝台账，改为按 (卷, 路径) **逐条惰性**查（O(1)）；
// 卷维度键隔离（§15.1.1）必须保持——同名文件跨卷不得串值。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSearch_ChecksumPerVolumeNoCrossTalk：两卷文件（索引按 rel 唯一 ⇒ 用不同名），
// 台账写入不同 checksum ⇒ 搜索返回各自 checksum（惰性查找按卷隔离）。
//
// 变异：searchEntryToFileInfo 忽略 e.volume（cs("", name)）⇒ 跨卷串值 ⇒ 红。
func TestSearch_ChecksumPerVolumeNoCrossTalk(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableVolumes(t, "default", "disk2")
	for _, vol := range []string{"default", "disk2"} {
		tnt := env.volumeTenant(vol, "alice")
		if tnt == nil || tnt.Root() == nil {
			t.Fatalf("%s 租户不可用", vol)
		}
		userRel := tnt.UserRoot() + "/x-" + vol + ".txt"
		if err := tnt.Root().MkdirAll(tnt.UserRoot(), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", vol, err)
		}
		f, err := tnt.Root().OpenFile(userRel, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("create %s: %v", vol, err)
		}
		if _, werr := f.WriteString(vol); werr != nil {
			t.Fatalf("write %s: %v", vol, werr)
		}
		_ = f.Close()
	}
	env.rebuild()

	cs := env.svc.rt.checksumStore("alice")
	if cs == nil {
		t.Fatal("checksum store 未装配")
	}
	checksumSet(cs, "default", "user/x-default.txt", "1111111111111111111111111111111111111111111111111111111111111111")
	checksumSet(cs, "disk2", "user/x-disk2.txt", "2222222222222222222222222222222222222222222222222222222222222222")

	res, err := env.svc.Search(context.Background(), SearchQuery{Owner: "alice", Query: "x-"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("应命中两卷各一条，got %+v", res.Files)
	}
	for _, f := range res.Files {
		want := map[string]string{"default": "1111", "disk2": "2222"}[f.Volume]
		if want == "" || f.Checksum[:4] != want {
			t.Fatalf("卷 %q checksum=%q，跨卷串值或缺失（got %+v）", f.Volume, f.Checksum, res.Files)
		}
	}
}

// TestSearch_ChecksumLedgerRebuildsOnEmpty：台账为空时搜索路径仍触发一次自愈重建
// （与既有全量入口同语义），checksum 不因惰性化而丢失。
func TestSearch_ChecksumLedgerRebuildsOnEmpty(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	env.rebuild()
	// 不写台账（模拟丢失/新租户）⇒ 搜索触发重建（从对象 meta / 实时计算）。
	res, err := env.svc.Search(context.Background(), SearchQuery{Owner: "alice", Query: "a.txt"})
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("search: n=%d err=%v", len(res.Files), err)
	}
}
