// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"sort"
	"sync"
	"testing"
	"time"
)

// fixedNow 是测试用固定时间基准。
var fixedNow = time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)

// mutableClock 是可前进的测试时钟：注入 Ring.now 后通过 Advance 推进时间，
// 用于验证"注入 now 前进"后 alive 判定（含 ExpiresAt 已到）。
type mutableClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fixedNow.Add(c.offset)
}

func (c *mutableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// must32BHex 生成内容全为 n 的 32 字节密钥供测试使用。
func must32BHex(t *testing.T, n byte) []byte {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = n
	}
	return b
}

// TestRing_Lookup_Empty 空 ring Lookup 返回 (nil, false)。
func TestRing_Lookup_Empty(t *testing.T) {
	r := NewRing()
	ks, ok := r.Lookup("sk-whatever")
	if ok {
		t.Fatalf("Lookup 空 ring 应返回 ok=false")
	}
	if ks != nil {
		t.Fatalf("Lookup 空 ring 应返回 nil 切片")
	}
}

// TestRing_UpsertAK_ThenLookup UpsertAK 后 Lookup 可查到，CoreEntry 返回最新加入条目。
func TestRing_UpsertAK_ThenLookup(t *testing.T) {
	r := NewRing()
	ak := "sk-1234567890abcdef"
	if err := r.UpsertAK(ak, "owner-1"); err != nil {
		t.Fatalf("UpsertAK 失败: %v", err)
	}
	ks, ok := r.Lookup(ak)
	if !ok {
		t.Fatalf("UpsertAK 后 Lookup 应 ok=true")
	}
	if len(ks) != 0 {
		t.Fatalf("UpsertAK 后不应有条目，实际 %d", len(ks))
	}

	if _, err := r.AddKey(ak, must32BHex(t, 0x11)); err != nil {
		t.Fatalf("AddKey #1 失败: %v", err)
	}
	if _, err := r.AddKey(ak, must32BHex(t, 0x22)); err != nil {
		t.Fatalf("AddKey #2 失败: %v", err)
	}
	ce := r.CoreEntry(ak)
	if ce == nil {
		t.Fatalf("CoreEntry 应为非 nil")
	}
	if !bytes.Equal(ce.SK, must32BHex(t, 0x22)) {
		t.Fatalf("CoreEntry 应返回最新加入的 SK")
	}
}

// TestRing_AddKey_Multiple 追加多条后 Lookup 返回全部未过期条目；CoreEntry 返回最新。
func TestRing_AddKey_Multiple(t *testing.T) {
	clk := &mutableClock{}
	r := NewRing(clk.Now)
	ak := "sk-abcdef1234567890"
	if err := r.UpsertAK(ak, "o"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	if _, err := r.AddKey(ak, must32BHex(t, 1)); err != nil {
		t.Fatalf("AddKey #1: %v", err)
	}
	if _, err := r.AddKey(ak, must32BHex(t, 2)); err != nil {
		t.Fatalf("AddKey #2: %v", err)
	}
	if _, err := r.AddKey(ak, must32BHex(t, 3)); err != nil {
		t.Fatalf("AddKey #3: %v", err)
	}
	ks, ok := r.Lookup(ak)
	if !ok {
		t.Fatalf("Lookup 应 ok=true")
	}
	if len(ks) != 3 {
		t.Fatalf("Lookup 应返回 3 条，实际 %d", len(ks))
	}
	ce := r.CoreEntry(ak)
	if ce == nil {
		t.Fatalf("CoreEntry 应为非 nil")
	}
	if !bytes.Equal(ce.SK, must32BHex(t, 3)) {
		t.Fatalf("CoreEntry 应最新（sk=0x03）")
	}
}

// TestRing_AddKey_UnknownAK AddKey 对不存在 AK 返回错误。
func TestRing_AddKey_UnknownAK(t *testing.T) {
	r := NewRing()
	_, err := r.AddKey("sk-9999999999999999", must32BHex(t, 0xAA))
	if err == nil {
		t.Fatalf("AddKey 对不存在 AK 应返回错误")
	}
}

// TestRing_ExpireKey 注入时钟前进后 Lookup 剔除过期、CoreEntry 仍返回未过期者、
// GetEntry 对已过期条目返回错误。
func TestRing_ExpireKey(t *testing.T) {
	clk := &mutableClock{}
	r := NewRing(clk.Now)
	ak := "sk-mesh-1234567890abcdef"
	if err := r.UpsertAK(ak, "o"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	if _, err := r.AddKey(ak, must32BHex(t, 1)); err != nil {
		t.Fatalf("AddKey #1: %v", err)
	}
	id2, err := r.AddKey(ak, must32BHex(t, 2))
	if err != nil {
		t.Fatalf("AddKey #2: %v", err)
	}
	// 设第二条过期（until=now-1s → 立即过期）
	until := fixedNow.Add(-time.Second)
	if err := r.ExpireKey(ak, id2, until); err != nil {
		t.Fatalf("ExpireKey: %v", err)
	}
	// GetEntry 对已过期条目返回错误
	if _, _, err := r.GetEntry(ak, id2); err != ErrExpired {
		t.Fatalf("GetEntry 已过期条目应返回 ErrExpired, got %v", err)
	}
	ks, ok := r.Lookup(ak)
	if !ok {
		t.Fatalf("Lookup 应 ok=true")
	}
	if len(ks) != 1 {
		t.Fatalf("Lookup 应剔除过期条目，剩余 %d", len(ks))
	}
	ce := r.CoreEntry(ak)
	if ce == nil || !bytes.Equal(ce.SK, must32BHex(t, 1)) {
		t.Fatalf("CoreEntry 应返回未过期者")
	}
}

// TestRing_ExpireKey_UntilZero 传零值 until 清空过期时间，条目恢复永久有效。
func TestRing_ExpireKey_UntilZero(t *testing.T) {
	clk := &mutableClock{}
	r := NewRing(clk.Now)
	ak := "sk-z-1234567890abcdef"
	if err := r.UpsertAK(ak, "o"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	id, err := r.AddKey(ak, must32BHex(t, 1))
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	// 先设一个过期时间（现在+1h），再前进到将来使其过期
	until := fixedNow.Add(time.Hour)
	if err := r.ExpireKey(ak, id, until); err != nil {
		t.Fatalf("ExpireKey(set): %v", err)
	}
	clk.Advance(31 * 24 * time.Hour)
	ks, ok := r.Lookup(ak)
	if !ok || len(ks) != 0 {
		t.Fatalf("条目尚未过期?")
	}
	// 清空过期时间 → 永久有效（注入时钟仍在未来，但零值=永久，应恢复存活）
	if err := r.ExpireKey(ak, id, time.Time{}); err != nil {
		t.Fatalf("ExpireKey(until=zero): %v", err)
	}
	ks, ok = r.Lookup(ak)
	if !ok || len(ks) != 1 {
		t.Fatalf("清空过期时间后 Lookup 应返回条目")
	}
}

// TestRing_DeleteKey 删除某条后 Lookup 不含它、再删同条返回错误（404 语义）。
func TestRing_DeleteKey(t *testing.T) {
	r := NewRing()
	ak := "sk-d-1234567890abcdef"
	if err := r.UpsertAK(ak, "o"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	id1, err := r.AddKey(ak, must32BHex(t, 1))
	if err != nil {
		t.Fatalf("AddKey #1: %v", err)
	}
	if _, err := r.AddKey(ak, must32BHex(t, 2)); err != nil {
		t.Fatalf("AddKey #2: %v", err)
	}
	if err := r.DeleteKey(ak, id1); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	ks, _ := r.Lookup(ak)
	if len(ks) != 1 {
		t.Fatalf("删除后应剩 1 条，实际 %d", len(ks))
	}
	if err := r.DeleteKey(ak, id1); err != ErrNotFound {
		t.Fatalf("再删同条应 ErrNotFound, got %v", err)
	}
}

// TestRing_DeleteAK 删除整个 AK → Lookup false。
func TestRing_DeleteAK(t *testing.T) {
	r := NewRing()
	ak := "sk-a-1234567890abcdef"
	if err := r.UpsertAK(ak, "o"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	if _, err := r.AddKey(ak, must32BHex(t, 1)); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if err := r.DeleteAK(ak); err != nil {
		t.Fatalf("DeleteAK: %v", err)
	}
	if _, ok := r.Lookup(ak); ok {
		t.Fatalf("DeleteAK 后 Lookup 应 ok=false")
	}
	if err := r.DeleteAK(ak); err != ErrNotFound {
		t.Fatalf("再删同 AK 应 ErrNotFound, got %v", err)
	}
}

// TestRing_InvalidArgs 非法入参校验。
func TestRing_InvalidArgs(t *testing.T) {
	t.Run("UpsertAK empty AK", func(t *testing.T) {
		r := NewRing()
		if err := r.UpsertAK("", "o"); err != ErrInvalidAK {
			t.Fatalf("UpsertAK 空 AK 应 ErrInvalidAK, got %v", err)
		}
	})
	t.Run("AddKey bad SK len", func(t *testing.T) {
		r := NewRing()
		ak := "sk-b-1234567890abcdef"
		if err := r.UpsertAK(ak, "o"); err != nil {
			t.Fatalf("UpsertAK: %v", err)
		}
		_, err := r.AddKey(ak, []byte("too-short"))
		if err != ErrInvalidSecret {
			t.Fatalf("非 32B SK 应 ErrInvalidSecret, got %v", err)
		}
	})
	t.Run("AddKey empty ID auto-generate", func(t *testing.T) {
		r := NewRing()
		ak := "sk-c-1234567890abcdef"
		if err := r.UpsertAK(ak, "o"); err != nil {
			t.Fatalf("UpsertAK: %v", err)
		}
		id, err := r.AddKey(ak, must32BHex(t, 5), WithID(""))
		if err != nil {
			t.Fatalf("AddKey(empty id): %v", err)
		}
		if len(id) != len("sk-")+EntryIDLen {
			t.Fatalf("自动生成 ID 长度应为 sk-<12hex>, got %q (%d)", id, len(id))
		}
	})
	t.Run("AddKey duplicate ID", func(t *testing.T) {
		r := NewRing()
		ak := "sk-d2-1234567890abcdef"
		if err := r.UpsertAK(ak, "o"); err != nil {
			t.Fatalf("UpsertAK: %v", err)
		}
		if _, err := r.AddKey(ak, must32BHex(t, 6), WithID("sk-000000000001")); err != nil {
			t.Fatalf("首次 AddKey: %v", err)
		}
		if _, err := r.AddKey(ak, must32BHex(t, 7), WithID("sk-000000000001")); err != ErrDuplicate {
			t.Fatalf("重复 ID 应 ErrDuplicate, got %v", err)
		}
	})
}

// TestRing_Snapshot_SortedAndDeepCopy Snapshot 按 AK 排序、深拷贝（改返回切片不影响内部）。
func TestRing_Snapshot_SortedAndDeepCopy(t *testing.T) {
	r := NewRing()
	aks := []string{
		"sk-3333333333333333",
		"sk-1111111111111111",
		"sk-2222222222222222",
	}
	for _, ak := range aks {
		if err := r.UpsertAK(ak, "o"); err != nil {
			t.Fatalf("UpsertAK: %v", err)
		}
		if _, err := r.AddKey(ak, must32BHex(t, byte(ak[3]))); err != nil {
			t.Fatalf("AddKey: %v", err)
		}
	}
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot 长度应为 3, got %d", len(snap))
	}
	// 按 AK 排序断言
	sortedAks := make([]string, 3)
	for i, k := range snap {
		sortedAks[i] = k.AK
	}
	if !sort.StringsAreSorted(sortedAks) {
		t.Fatalf("Snapshot 应按 AK 排序，got %v", sortedAks)
	}
	// 深拷贝：修改返回切片内部不影响 ring
	snap[0].Entries[0].SK[0] ^= 0xff
	snap[0].AK = "sk-hacked"
	orig := r.Snapshot()[0]
	if orig.AK == "sk-hacked" {
		t.Fatalf("深拷贝失败：修改 Snapshot 的 AK 影响了内部")
	}
	if bytes.Equal(orig.Entries[0].SK, snap[0].Entries[0].SK) {
		t.Fatalf("深拷贝失败：修改 Snapshot 的 SK 影响了内部")
	}
}

// TestRing_Concurrent 并发 AddKey/Lookup/Expire/Delete 跑 200 轮，-race 下无竞态。
func TestRing_Concurrent(t *testing.T) {
	r := NewRing()
	const nAK = 8
	const rounds = 200
	akName := func(i int) string {
		return "sk-cn-" + string(rune('a'+i)) + "1234567890abcd"
	}
	for i := range nAK {
		if err := r.UpsertAK(akName(i), "o"); err != nil {
			t.Fatalf("UpsertAK: %v", err)
		}
	}
	var wg sync.WaitGroup
	for i := range nAK {
		ak := akName(i)
		seed := byte(i)
		wg.Go(func() {
			for j := range rounds {
				id, err := r.AddKey(ak, must32BHex(t, seed))
				if err != nil {
					t.Errorf("AddKey: %v", err)
					return
				}
				_ = r.CoreEntry(ak)
				_, _ = r.Lookup(ak)
				_, _, _ = r.GetEntry(ak, id)
				_ = r.Snapshot()
				if j%10 == 0 {
					_ = r.ExpireKey(ak, id, time.Time{}) // 清空过期（幂等）
					_ = r.DeleteKey(ak, id)
				}
			}
		})
	}
	wg.Wait()
	// 全部结束后 ring 状态仍一致
	_ = r.Snapshot()
}
