// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// bypass_dirstate_test.go 锁定 R16 缺陷族修复：旁路写（cloud 转存 / 归档转存 / 跨卷 move）
// 成功后必须登记**派生状态**（checksum 台账 + 搜索索引），否则该卷 list 缺条目且 checksum 空。
//
// 变异（去掉 IndexMove/RecordBypassWrite 的 checksum store 写入或索引 upsert）⇒ 断言红。

import (
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestIndexMove_MigratesChecksumAndEventDrivesIndex 钉住 V4 阶段二口径：
//   - `IndexMove` 只迁移 **checksum 台账**（物理搬移的索引维护已由 Root 事件承担）；
//   - 事件（这里用目标卷的 `MutationWrite` 等价模拟）负责把条目落到**目标卷**并带台账 checksum。
//
// 变异：① 去掉 IndexMove 的台账写入 ⇒ checksum 断言红；② 去掉事件 ⇒ 条目缺失/卷归属陈旧 ⇒ 红。
func TestIndexMove_MigratesChecksumAndEventDrivesIndex(t *testing.T) {
	t.Parallel()
	dir, derr := os.MkdirTemp("", "idxmove-*")
	if derr != nil {
		t.Fatal(derr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	h := newAssemblyTestHandlers(t, dir)
	svc := h.fileService()
	if svc == nil {
		t.Fatal("fileService 不可用")
	}
	tnt := h.tenantFor("alice")
	if tnt == nil {
		t.Fatal("tenant 不可用")
	}
	if err := tnt.Root().MkdirAll("user/docs", 0o755); err != nil {
		t.Fatal(err)
	}
	fh, ferr := tnt.Root().OpenFileEncrypted("user/docs/a.bin", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if ferr != nil {
		t.Fatal(ferr)
	}
	_, _ = fh.Write([]byte("hello-bypass"))
	_ = fh.Close()

	const owner, rel, cs = "alice", "user/docs/a.bin", "abc123"
	// 先触发一次 list（构建 owner 索引——upsert 仅在已构建时生效）。
	if _, lerr := svc.List(t.Context(), files.ListQuery{Owner: owner, Subdir: "docs"}); lerr != nil {
		t.Fatalf("预热 list: %v", lerr)
	}
	// ① 台账迁移（IndexMove 唯一职责）。
	svc.IndexMove(owner, rel, 42, cs, "vol2", nil)
	if got, ok := h.checksumStoreFor(owner).Get(rel); !ok || got != cs {
		t.Fatalf("move 后 checksum 台账应写入 %q，got %q ok=%v（未写 ⇒ 目标卷 list checksum 空）", cs, got, ok)
	}
	// ② 物理搬移造成的索引更新由事件承担：目标卷内容写事件（Tag=vol2）刷新条目与卷归属。
	// 事件口径：Root = **卷根**（这里是 globalRoot），Rel = 卷根命名空间 `<owner>/user/<rel>`。
	svc.ApplyRootMutation(storage.Mutation{
		Op: storage.MutationWrite, Tag: "vol2", Root: h.globalRoot, Rel: owner + "/" + rel,
	})
	res, lerr := svc.List(t.Context(), files.ListQuery{Owner: owner, Subdir: "docs"})
	if lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	found := false
	for _, f := range res.Files {
		if f.Name == "a.bin" {
			found = true
			if f.Checksum != cs {
				t.Fatalf("list 条目 checksum = %q, want %q", f.Checksum, cs)
			}
			if f.Volume != "vol2" {
				t.Fatalf("条目卷归属 = %q，want vol2（事件应携带目标卷 Tag）", f.Volume)
			}
		}
	}
	if !found {
		t.Fatalf("move 后 list 应包含 a.bin（got %d 条）", len(res.Files))
	}
}
