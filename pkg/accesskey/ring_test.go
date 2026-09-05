// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"encoding/hex"
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
	ks, ok := r.Lookup("ak-whatever")
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
	ak := "ak-1234567890abcdef"
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
	ak := "ak-abcdef1234567890"
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
	_, err := r.AddKey("ak-9999999999999999", must32BHex(t, 0xAA))
	if err == nil {
		t.Fatalf("AddKey 对不存在 AK 应返回错误")
	}
}

// TestRing_ExpireKey 注入时钟前进后 Lookup 剔除过期、CoreEntry 仍返回未过期者、
// GetEntry 对已过期条目返回错误。
func TestRing_ExpireKey(t *testing.T) {
	clk := &mutableClock{}
	r := NewRing(clk.Now)
	ak := "ak-mesh-1234567890abcdef"
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
	ak := "ak-z-1234567890abcdef"
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
	ak := "ak-d-1234567890abcdef"
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
	ak := "ak-a-1234567890abcdef"
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
		ak := "ak-b-1234567890abcdef"
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
		ak := "ak-c-1234567890abcdef"
		if err := r.UpsertAK(ak, "o"); err != nil {
			t.Fatalf("UpsertAK: %v", err)
		}
		id, err := r.AddKey(ak, must32BHex(t, 5), WithID(""))
		if err != nil {
			t.Fatalf("AddKey(empty id): %v", err)
		}
		if len(id) != len("skey-")+EntryIDLen {
			t.Fatalf("自动生成 ID 长度应为 skey-<12hex>, got %q (%d)", id, len(id))
		}
	})
	t.Run("AddKey duplicate ID", func(t *testing.T) {
		r := NewRing()
		ak := "ak-d2-1234567890abcdef"
		if err := r.UpsertAK(ak, "o"); err != nil {
			t.Fatalf("UpsertAK: %v", err)
		}
		if _, err := r.AddKey(ak, must32BHex(t, 6), WithID("skey-000000000001")); err != nil {
			t.Fatalf("首次 AddKey: %v", err)
		}
		if _, err := r.AddKey(ak, must32BHex(t, 7), WithID("skey-000000000001")); err != ErrDuplicate {
			t.Fatalf("重复 ID 应 ErrDuplicate, got %v", err)
		}
	})
}

// TestRing_AddKey_CopiesSecret 修复轮 1#2：AddKey 必须复制入参 SK 切片，调用方随后
// 改写缓冲区不影响 ring 内部凭据。
func TestRing_AddKey_CopiesSecret(t *testing.T) {
	r := NewRing()
	ak := "ak-cp-1234567890abcdef"
	if err := r.UpsertAK(ak, "o"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	sk := must32BHex(t, 0x55)
	if _, err := r.AddKey(ak, sk); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	// 调用方改写入参缓冲
	for i := range sk {
		sk[i] = 0xEE
	}
	ce := r.CoreEntry(ak)
	if ce == nil || !bytes.Equal(ce.SK, must32BHex(t, 0x55)) {
		t.Fatalf("AddKey 后改写入参缓冲污染了 ring 内 SK")
	}
}

// TestRing_Replace 修复轮 1#3：Replace 原子全量替换（store 装载 / 快照还原用），
// 空 AK 校验失败且替换不生效；入参被深拷贝。
func TestRing_Replace(t *testing.T) {
	r := NewRing()
	// 先放旧数据
	oldAK := "ak-old-1234567890abcdef"
	if err := r.UpsertAK(oldAK, "old"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	if _, err := r.AddKey(oldAK, must32BHex(t, 1)); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if r.Len() != 1 {
		t.Fatalf("初始 Len 应为 1")
	}

	// 替换为新集合
	newAKs := []Key{
		{AK: "ak-b-1234567890abcdef", Owner: "o2", Entries: []SKEntry{
			{ID: "skey-0000000000aa", SK: must32BHex(t, 0xAA), CreatedAt: fixedNow, Status: StatusActive},
		}},
		{AK: "ak-a-1234567890abcdef", Owner: "o1", Entries: []SKEntry{
			{ID: "skey-0000000000bb", SK: must32BHex(t, 0xBB), CreatedAt: fixedNow, Status: StatusActive},
		}},
	}
	if err := r.Replace(newAKs); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if r.Len() != 2 {
		t.Fatalf("Replace 后 Len 应为 2, got %d", r.Len())
	}
	if _, ok := r.Lookup(oldAK); ok {
		t.Fatalf("Replace 后旧 AK 应被移除")
	}
	// 深拷贝：改入参不影响 ring
	newAKs[0].Entries[0].SK[0] ^= 0xff
	ce := r.CoreEntry("ak-b-1234567890abcdef")
	if ce == nil {
		t.Fatalf("CoreEntry(新 AK) 失败")
	}
	if !bytes.Equal(ce.SK, must32BHex(t, 0xAA)) {
		t.Fatalf("Replace 未深拷贝 SK")
	}

	// 非法入参：含空 AK → ErrInvalidAK 且替换不生效（原子性）
	snapshotBefore := r.Snapshot()
	if err := r.Replace([]Key{{AK: "", Owner: "x"}}); err != ErrInvalidAK {
		t.Fatalf("含空 AK 的 Replace 应 ErrInvalidAK, got %v", err)
	}
	snapshotAfter := r.Snapshot()
	if len(snapshotBefore) != len(snapshotAfter) {
		t.Fatalf("非法 Replace 不应部分生效")
	}
}

// TestRing_ExpireKey_StatusRefresh 修复轮 1#7：ExpireKey 设置 until 后同步刷新 Status——
// until 零值恢复永久 → active；until 将来 → active；until 已过去 → expired
// （刷新发生在 ExpireKey 写操作时；纯时间流逝不改写持久化 Status，存活判定独立）。
func TestRing_ExpireKey_StatusRefresh(t *testing.T) {
	clk := &mutableClock{}
	r := NewRing(clk.Now)
	ak := "ak-ref-1234567890abcdef"
	if err := r.UpsertAK(ak, "o"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	id, err := r.AddKey(ak, must32BHex(t, 1))
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	statusOf := func() Status {
		for _, k := range r.Snapshot() {
			for _, en := range k.Entries {
				if en.ID == id {
					return en.Status
				}
			}
		}
		return ""
	}

	// until 将来 → active
	if err := r.ExpireKey(ak, id, fixedNow.Add(time.Hour)); err != nil {
		t.Fatalf("ExpireKey(future): %v", err)
	}
	if got := statusOf(); got != StatusActive {
		t.Fatalf("until 将来 Status 应为 active, got %q", got)
	}

	// until 已过去（相对当前 now 已是过去）→ expired
	if err := r.ExpireKey(ak, id, fixedNow.Add(-time.Second)); err != nil {
		t.Fatalf("ExpireKey(past): %v", err)
	}
	if got := statusOf(); got != StatusExpired {
		t.Fatalf("until 已过去 Status 应为 expired, got %q", got)
	}
	if ks, ok := r.Lookup(ak); !ok || len(ks) != 0 {
		t.Fatalf("until 已过去条目应不可用")
	}

	// until 零值（恢复永久）→ active，且 Lookup 重新返回
	if err := r.ExpireKey(ak, id, time.Time{}); err != nil {
		t.Fatalf("ExpireKey(zero): %v", err)
	}
	if got := statusOf(); got != StatusActive {
		t.Fatalf("until 零值恢复永久 Status 应为 active, got %q", got)
	}
	if ks, ok := r.Lookup(ak); !ok || len(ks) != 1 {
		t.Fatalf("恢复永久后 Lookup 应返回条目")
	}
}

// TestRing_Snapshot_SortedAndDeepCopy Snapshot 按 AK 排序、深拷贝（改返回切片不影响内部）。
func TestRing_Snapshot_SortedAndDeepCopy(t *testing.T) {
	r := NewRing()
	aks := []string{
		"ak-3333333333333333",
		"ak-1111111111111111",
		"ak-2222222222222222",
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
	snap[0].AK = "ak-hacked"
	orig := r.Snapshot()[0]
	if orig.AK == "ak-hacked" {
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
		return "ak-cn-" + string(rune('a'+i)) + "1234567890abcd"
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

// TestNewRingFromKeyPairs 验证导出的装配工厂：合法条目入 ring、非法 SK 被跳过、
// 条目为 plain alive 且 Meta.Type="initial"、空输入得空 ring。
func TestNewRingFromKeyPairs(t *testing.T) {
	hex32 := hex.EncodeToString(must32BHex(t, 0xaa))
	// 合法 AK/SK + 非法 SK（非 32 字节）→ 只有合法条目存活。
	ring := NewRingFromKeyPairs([]KeyPair{
		{Key: "ak-kp-test-1234567890ab", Secret: hex32},
		{Key: "sk-kp-bad-secret", Secret: "deadbeef"},
		{Key: "", Secret: hex32},
	})
	if entry := ring.CoreEntry("ak-kp-test-1234567890ab"); entry == nil {
		t.Fatal("合法 AK/SK 应有存活条目")
	} else {
		if entry.Kind != KindPlain {
			t.Fatalf("条目形态应为 plain, got %q", entry.Kind)
		}
		if entry.Status != StatusActive {
			t.Fatalf("条目状态应为 active, got %q", entry.Status)
		}
		if entry.Meta.Type != "initial" {
			t.Fatalf("条目 Meta.Type 应为 initial, got %q", entry.Meta.Type)
		}
		if entry.ExpiresAt.IsZero() == false {
			t.Fatal("初始条目应永久有效（ExpiresAt 零值）")
		}
		if got := hex.EncodeToString(entry.SK); got != hex32 {
			t.Fatalf("条目 SK 应与入参一致, got %q", got)
		}
	}
	if ring.CoreEntry("ak-kp-bad-secret") != nil {
		t.Fatal("非法 SK 条目不应进入 ring")
	}
	if ring.CoreEntry("") != nil {
		t.Fatal("空 AK 条目不应进入 ring")
	}
	if got := ring.Snapshot(); len(got) != 1 {
		t.Fatalf("Snapshot 应有 1 条，got %d", len(got))
	}
	// 空输入 → 空 ring。
	if got := NewRingFromKeyPairs(nil).Len(); got != 0 {
		t.Fatalf("空输入应为空 ring, got Len=%d", got)
	}
}
