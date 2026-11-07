// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestDlinkLRU_BasicGetPutDelete：基本 get/put/delete。
func TestDlinkLRU_BasicGetPutDelete(t *testing.T) {
	t.Parallel()
	c := newDlinkLRU(4)
	c.put("k1", "u1", time.Hour)
	if u, ok := c.get("k1"); !ok || u != "u1" {
		t.Fatalf("get 应命中 u1，got %q ok=%v", u, ok)
	}
	c.delete("k1")
	if _, ok := c.get("k1"); ok {
		t.Fatal("delete 后不应命中")
	}
}

// TestDlinkLRU_EvictsOldest：超容量淘汰最久未用。
func TestDlinkLRU_EvictsOldest(t *testing.T) {
	t.Parallel()
	c := newDlinkLRU(3)
	c.put("a", "ua", time.Hour)
	c.put("b", "ub", time.Hour)
	c.put("c", "uc", time.Hour)
	c.get("b") // 命中 b ⇒ b 变最新
	c.put("d", "ud", time.Hour)
	if _, ok := c.get("a"); ok {
		t.Fatal("a 应被 LRU 淘汰")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := c.get(k); !ok {
			t.Fatalf("%s 不应被淘汰", k)
		}
	}
}

// TestDlinkLRU_ExpiredRemoved：过期条目按 TTL 失效并从索引移除。
// 直接把条目 exp 改到过去（不 Sleep——R14 睡眠棘轮约束，且消除真实时钟 flake）。
func TestDlinkLRU_ExpiredRemoved(t *testing.T) {
	t.Parallel()
	c := newDlinkLRU(4)
	c.put("k", "u", time.Hour)
	c.mu.Lock()
	c.idx["k"].Value.(*dlinkEntry).exp = time.Now().Add(-time.Second)
	c.mu.Unlock()
	if _, ok := c.get("k"); ok {
		t.Fatal("过期条目不应命中")
	}
	if c.len() != 0 {
		t.Fatalf("过期条目应从索引移除，len=%d", c.len())
	}
}

// TestDlinkLRU_UpdateRefreshes：同 key 更新刷新过期时间与位次。
func TestDlinkLRU_UpdateRefreshes(t *testing.T) {
	t.Parallel()
	c := newDlinkLRU(4)
	c.put("k", "u1", time.Hour)
	c.mu.Lock()
	c.idx["k"].Value.(*dlinkEntry).exp = time.Now().Add(-time.Second) // 人为置过期
	c.mu.Unlock()
	c.put("k", "u2", time.Hour) // 同 key 更新应刷新 exp
	if u, ok := c.get("k"); !ok || u != "u2" {
		t.Fatalf("刷新后应命中 u2，got %q ok=%v", u, ok)
	}
}

// TestStorageDelete_InvalidatesDlink —— **删除后直链缓存必须失效**（S1，2026-10-27）。
//
// 背景（设计文档 2026-10-27-remote-derived-state-cache §3.3）：直链缓存描述「远端对象当前
// 可用性」；对象被删后缓存里的 URL 即使未过期也是死链，且用户**同名重建**新文件时命中旧
// 缓存会拿到指向旧文件的 URL ⇒ 404 / 取到错误内容。此前 `Storage.Delete` 成功后完全不清
// 缓存（依赖 TTL 1h 兜底 = 幽灵窗口）。
//
// 断言：读一次（签发并缓存）→ Delete 成功 → 同名重建 → 再读 ⇒ 必须**重新签发**（sign 次数
// +1）且内容为重建后的新内容（不是旧直链）。
func TestStorageDelete_InvalidatesDlink(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("Q", 100)
	st, ad, key := newCountingRangeEnv(t, content)

	// ① 首读：签发 #1 并缓存。
	if got := readRange(t, st, key, 0, 10); got != content[:10] {
		t.Fatalf("首段内容错误: %q", got)
	}
	if n := ad.signs.Load(); n != 1 {
		t.Fatalf("首读签发次数=%d，应为 1", n)
	}

	// ② 删除成功 ⇒ 直链缓存必须被清除（这是本用例的核心断言：缓存里不得再有该 key）。
	if err := st.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := st.dlinkCache.get(key); ok {
		t.Fatal("删除成功后 dlink 缓存仍命中（S1 失效未生效 ⇒ 同名重建会复用旧直链）")
	}

	// ③ 同名重建（内容不同）→ 再读 ⇒ 必须重新签发（sign #2）且读到**新**内容。
	// 注：fake adapter 的 DirectLink 返回固定 URL；这里用新 content server 换掉 dlink 指向，
	// 验证「重建后不命中旧缓存、重新走 DirectLink」——签发次数就是证据。
	newContent := strings.Repeat("Z", 100)
	srv2 := newRangeTestServer(t, newContent, nil)
	ad.dlink = srv2.URL + "/f.bin"
	if _, perr := st.Put(context.Background(), key, strings.NewReader(newContent)); perr != nil {
		t.Fatalf("同名重建 Put: %v", perr)
	}
	if got := readRange(t, st, key, 0, 10); got != newContent[:10] {
		t.Fatalf("重建后读到的内容=%q，应为新内容 %q（命中旧直链 ⇒ 读到旧文件）", got, newContent[:10])
	}
	if n := ad.signs.Load(); n != 2 {
		t.Fatalf("重建后再读签发次数=%d，应为 2（删除后必须重签，不得复用旧直链）", n)
	}
}

// TestStorageMove_InvalidatesSourceDlink —— **移动后源直链必须失效**（S1）。
//
// 移动 = 源对象已不存在，但目标新路径的直链与源不同 key ⇒ 源 key 的旧缓存是死链。若不移除，
// 源 key 再次出现（同名重建/回迁）会命中旧直链。断言 Move 后源 key 缓存被清。
func TestStorageMove_InvalidatesSourceDlink(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("M", 80)
	st, _, key := newCountingRangeEnv(t, content)

	if got := readRange(t, st, key, 0, 8); got != content[:8] {
		t.Fatalf("首段内容错误: %q", got)
	}
	if _, err := st.Move(context.Background(), key, "dir/moved.bin"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, ok := st.dlinkCache.get(key); ok {
		t.Fatal("Move 成功后源 key 的 dlink 缓存仍命中（S1 失效未生效 ⇒ 旧直链幽灵）")
	}
}
