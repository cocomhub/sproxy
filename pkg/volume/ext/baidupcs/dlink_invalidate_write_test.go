// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// dlink_invalidate_write_test.go —— 钉住「上传/复制/移动后目标直链必须失效」（2026-10-27，
// S1 的 Delete/Move 只覆盖删除与源 key；写面（Put/Copy/Move 目标）覆盖同名旧对象后旧直链
// 仍在缓存 ⇒ 命中旧 fs_id ⇒ 内容错。这是「对象内容可变化」场景的失效纪律补全）。
//
// 测试模型：fake adapter 的 DirectLink 返回 `ad.dlink`（可切换的服务器 URL），Range GET
// 打到该服务器。每个用例：先用「旧内容服务器」首读（缓存旧直链）→ 切「新内容服务器」+
// 执行覆盖写 → 断言缓存已清 + 再读重签（sign+1）且读到新内容。

import (
	"context"
	"strings"
	"testing"
)

// newDlinkSwitchableEnv 建 Storage + 可切换直链的计数 adapter + 初始内容服务器。
func newDlinkSwitchableEnv(t *testing.T, content string) (*Storage, *dlinkCountingAdapter, string) {
	t.Helper()
	st, ad, key := newCountingRangeEnv(t, content)
	return st, ad, key
}

// switchDlink 把直链切到新内容服务器（模拟「覆盖后新对象」的直链 URL）。
func switchDlink(t *testing.T, ad *dlinkCountingAdapter, content string) {
	t.Helper()
	srv := newRangeTestServer(t, content, nil)
	ad.dlink = srv.URL + "/f.bin"
}

// TestStoragePut_InvalidatesDlinkOnOverwrite 上传覆盖同名旧对象后，目标直链必须失效。
func TestStoragePut_InvalidatesDlinkOnOverwrite(t *testing.T) {
	t.Parallel()
	oldContent := strings.Repeat("OLD", 50)
	st, ad, key := newDlinkSwitchableEnv(t, oldContent)

	// ① 首读：签发 #1 并缓存。
	if got := readRange(t, st, key, 0, 6); got != oldContent[:6] {
		t.Fatalf("首段内容错误: %q", got)
	}
	if _, ok := st.dlinkCache.get(key); !ok {
		t.Fatal("首读后 dlink 缓存未建立（用例前提不成立）")
	}
	if n := ad.signs.Load(); n != 1 {
		t.Fatalf("首读签发次数=%d，应为 1", n)
	}
	// ② 上传覆盖同名（内容不同）→ 必须失效目标直链（核心断言）。
	newContent := strings.Repeat("NEW", 50)
	switchDlink(t, ad, newContent)
	if _, perr := st.Put(context.Background(), key, strings.NewReader(newContent)); perr != nil {
		t.Fatalf("覆盖写 Put: %v", perr)
	}
	if _, ok := st.dlinkCache.get(key); ok {
		t.Fatal("覆盖写成功后 dlink 缓存仍命中（未失效 ⇒ 下次命中旧对象直链）")
	}
	// ③ 再读 ⇒ 必须重新签发（sign #2）且读到**新**内容。
	if got := readRange(t, st, key, 0, 6); got != newContent[:6] {
		t.Fatalf("覆盖后读到的内容=%q，应为新内容 %q（命中旧直链 ⇒ 读到旧文件）", got, newContent[:6])
	}
	if n := ad.signs.Load(); n != 2 {
		t.Fatalf("覆盖后再读签发次数=%d，应为 2（覆盖后必须重签）", n)
	}
}

// TestStorageMove_InvalidatesDstDlinkOnOverwrite Move 到**已存在同名目标**（覆盖语义）后，
// 目标旧直链必须失效（S1 原先只清源 key）。
func TestStorageMove_InvalidatesDstDlinkOnOverwrite(t *testing.T) {
	t.Parallel()
	st, ad, srcKey := newDlinkSwitchableEnv(t, strings.Repeat("S", 50))
	dstKey := "dir/dst.bin"

	// 先给目标写入旧内容（fake adapter 落盘），首读目标**用旧内容服务器**（缓存目标直链 #1）。
	oldDst := strings.Repeat("D", 50)
	switchDlink(t, ad, oldDst)
	if _, perr := st.Put(context.Background(), dstKey, strings.NewReader(oldDst)); perr != nil {
		t.Fatalf("Put 目标: %v", perr)
	}
	if got := readRange(t, st, dstKey, 0, 4); got != oldDst[:4] {
		t.Fatalf("目标首段内容错误: %q", got)
	}
	if _, ok := st.dlinkCache.get(dstKey); !ok {
		t.Fatal("目标首读后 dlink 缓存未建立（用例前提不成立）")
	}
	if n := ad.signs.Load(); n != 1 {
		t.Fatalf("目标首读签发次数=%d，应为 1", n)
	}
	// 切到「源内容」服务器（模拟 Move 后目标新对象的直链 URL），执行 Move 覆盖。
	srcContent := strings.Repeat("S", 50)
	switchDlink(t, ad, srcContent)
	if _, merr := st.Move(context.Background(), srcKey, dstKey); merr != nil {
		t.Fatalf("Move: %v", merr)
	}
	if _, ok := st.dlinkCache.get(dstKey); ok {
		t.Fatal("Move 覆盖目标后目标 dlink 缓存仍命中（未失效 ⇒ 命中被覆盖旧对象直链）")
	}
	// 再读目标 ⇒ 必须重签（sign #2）且读到**源**内容（覆盖后的新对象）。
	if got := readRange(t, st, dstKey, 0, 4); got != srcContent[:4] {
		t.Fatalf("Move 覆盖后目标内容=%q，应为源内容 %q", got, srcContent[:4])
	}
	if n := ad.signs.Load(); n != 2 {
		t.Fatalf("Move 覆盖后再读签发次数=%d，应为 2", n)
	}
}

// TestStorageCopy_InvalidatesDstDlinkOnOverwrite Copy 到**已存在同名目标**（覆盖语义）后，
// 目标旧直链必须失效。
func TestStorageCopy_InvalidatesDstDlinkOnOverwrite(t *testing.T) {
	t.Parallel()
	st, ad, srcKey := newDlinkSwitchableEnv(t, strings.Repeat("C", 50))
	dstKey := "dir/copy-dst.bin"

	oldDst := strings.Repeat("D", 50)
	switchDlink(t, ad, oldDst)
	if _, perr := st.Put(context.Background(), dstKey, strings.NewReader(oldDst)); perr != nil {
		t.Fatalf("Put 目标: %v", perr)
	}
	if got := readRange(t, st, dstKey, 0, 4); got != oldDst[:4] {
		t.Fatalf("目标首段内容错误: %q", got)
	}
	if _, ok := st.dlinkCache.get(dstKey); !ok {
		t.Fatal("目标首读后 dlink 缓存未建立（用例前提不成立）")
	}
	if n := ad.signs.Load(); n != 1 {
		t.Fatalf("目标首读签发次数=%d，应为 1", n)
	}
	srcContent := strings.Repeat("C", 50)
	switchDlink(t, ad, srcContent)
	if _, cerr := st.Copy(context.Background(), srcKey, dstKey); cerr != nil {
		t.Fatalf("Copy: %v", cerr)
	}
	if _, ok := st.dlinkCache.get(dstKey); ok {
		t.Fatal("Copy 覆盖目标后目标 dlink 缓存仍命中（未失效）")
	}
	if got := readRange(t, st, dstKey, 0, 4); got != srcContent[:4] {
		t.Fatalf("Copy 覆盖后目标内容=%q，应为源内容 %q", got, srcContent[:4])
	}
	if n := ad.signs.Load(); n != 2 {
		t.Fatalf("Copy 覆盖后再读签发次数=%d，应为 2", n)
	}
}
