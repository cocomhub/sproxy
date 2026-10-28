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
)

// TestIndexMove_WritesChecksumAndIndex 钉住跨卷 move 后目标卷 list 带 checksum：
// 源台账值随 move 迁移（此前 IndexMove 只更新索引、不写 checksum ⇒ list checksum 空）。
func TestIndexMove_WritesChecksumAndIndex(t *testing.T) {
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
	// 落盘源文件（跨卷 move 的真实前提：源文件存在）。
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
	// 源台账**清空**（move 前未上传/未记录场景）⇒ IndexMove 显式传 checksum 写入目标。
	if s := h.checksumStoreFor(owner); s != nil {
		if _, ok := s.Get(rel); ok {
			t.Fatal("前置：源台账应为空")
		}
	}
	// **先触发一次 list**（构建 owner 索引——upsert 仅在已构建时生效；未构建时由下次全量
	// 扫描兜底，这是设计的正确语义）。本用例锁定的是「索引已构建后旁路写不 upsert」的缺陷。
	if _, lerr := svc.List(t.Context(), files.ListQuery{Owner: owner, Subdir: "docs"}); lerr != nil {
		t.Fatalf("预热 list: %v", lerr)
	}
	// move 完成回调（IndexMove 是跨卷 move 的唯一派生状态登记入口）：显式传 checksum。
	svc.IndexMove(owner, rel, 42, cs, "vol2", nil)
	// checksum 台账：目标 rel 应写入（此前 IndexMove 只更新索引 ⇒ list checksum 空）。
	if got, ok := h.checksumStoreFor(owner).Get(rel); !ok || got != cs {
		t.Fatalf("move 后 checksum 台账应写入 %q，got %q ok=%v（未写 ⇒ 目标卷 list checksum 空）", cs, got, ok)
	}
	// 索引：条目应可列出（带 checksum）。
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
		}
	}
	if !found {
		t.Fatalf("move 后 list 应包含 a.bin（got %d 条）", len(res.Files))
	}
}
