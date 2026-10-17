// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"errors"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// usageFakeFS 是「知道真实占用」的卷层替身（只实现真值报告；FS 方法用嵌入接口占位，
// 测试不调用）。
type usageFakeFS struct {
	syncpkg.FS
	bytes int64
	ok    bool
	rel   map[string]int64
}

func (f *usageFakeFS) TrueUsage() (int64, bool) { return f.bytes, f.ok }

func (f *usageFakeFS) TrueSize(rel string) (int64, bool) {
	if !f.ok {
		return 0, false
	}
	n, found := f.rel[rel]
	return n, found
}

// innerWrapFS 模拟装饰器（暴露 Inner() 以便 Innermost 下探）。
type innerWrapFS struct {
	syncpkg.FS
	inner syncpkg.FS
}

func (f *innerWrapFS) Inner() syncpkg.FS { return f.inner }

// reporterWrapFS 是「既包装又实现真值报告」的层（模拟 secretdata：处理器层在链中间）。
type reporterWrapFS struct {
	syncpkg.FS
	inner syncpkg.FS
	bytes int64
	ok    bool
}

func (f *reporterWrapFS) Inner() syncpkg.FS             { return f.inner }
func (f *reporterWrapFS) TrueUsage() (int64, bool)      { return f.bytes, f.ok }
func (f *reporterWrapFS) TrueSize(string) (int64, bool) { return f.bytes, f.ok }

// TestTrueUsageOf_PrefersOutermostReporter 链上有多个实现时取**最外层**实现者：掌握物理布局的
// 是处理器层（内容分块 + pad + parity），最内层只是被包装的底层存储（不知道上层写了几个 blob）。
func TestTrueUsageOf_PrefersOutermostReporter(t *testing.T) {
	t.Parallel()
	innermost := &usageFakeFS{bytes: 7, ok: true}
	processor := &reporterWrapFS{inner: innermost, bytes: 999, ok: true}
	decorated := &innerWrapFS{inner: processor}

	r, ok := TrueUsageOf(decorated)
	if !ok || r == nil {
		t.Fatalf("应找到链上的真值报告者, ok=%v", ok)
	}
	if got, gok := r.TrueUsage(); !gok || got != 999 {
		t.Fatalf("应取最外层报告者（999）, got %d ok=%v", got, gok)
	}
	// ok=false 的报告者仍可被返回（调用方据 ok 回落），不得被当作 0。
	half := &reporterWrapFS{inner: innermost, ok: false}
	if r2, ok2 := TrueUsageOf(&innerWrapFS{inner: half}); !ok2 {
		t.Fatal("实现了接口就应返回（由调用方看 ok）")
	} else if _, tok := r2.TrueUsage(); tok {
		t.Fatal("报告者 ok=false 时不得冒称掌握")
	}
}

// TestTrueUsageOf_UnwrapsDecorators 真值报告者必须能穿过装饰器链被找到（否则上层永远
// 只能拿到估算 → 失真不可修正）。
func TestTrueUsageOf_UnwrapsDecorators(t *testing.T) {
	t.Parallel()
	inner := &usageFakeFS{bytes: 4096, ok: true}
	wrapped := &innerWrapFS{inner: &innerWrapFS{inner: inner}}

	r, ok := TrueUsageOf(wrapped)
	if !ok || r == nil {
		t.Fatalf("应穿过装饰器链找到真值报告者, ok=%v r=%v", ok, r)
	}
	if got, ok := r.TrueUsage(); !ok || got != 4096 {
		t.Fatalf("真值应为 4096, got %d ok=%v", got, ok)
	}

	// 不实现该能力 → 必须报「不掌握」，不得当作 0。
	if _, ok := TrueUsageOf(&innerWrapFS{inner: &innerWrapFS{}}); ok {
		t.Fatal("未实现真值报告者时应返回 ok=false")
	}
	if _, ok := TrueUsageOf(nil); ok {
		t.Fatal("nil FS 应返回 ok=false")
	}
}

// TestPoolCounter_CorrectTo_TrueValueWins 真值修正：可增可减、防下溢归零、不动 reserved、
// 修正后立即持久化（快照可读回）。
func TestPoolCounter_CorrectTo_TrueValueWins(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(1000)
	pc := newTestLedgerCounter(t, pool)
	if err := pc.TryAdd(400); err != nil {
		t.Fatalf("TryAdd: %v", err)
	}
	if pc.Used() != 400 {
		t.Fatalf("初始占用应为 400, got %d", pc.Used())
	}

	// 真值更大（逻辑记账低估，如加密卷 pad 膨胀）→ 上调到真值。
	pc.CorrectTo(1500)
	if pc.Used() != 1500 {
		t.Fatalf("修正后应为真值 1500, got %d", pc.Used())
	}
	// 真值优先于上限：修正暴露超限，但不得被截断（否则失真被掩盖）。
	if pool.MaxBytes() != 1000 {
		t.Fatalf("上限应保持 1000, got %d", pool.MaxBytes())
	}
	// 超限后再写入必须被拒（上限按真值判定，不能因逻辑记账而放行）。
	if err := pc.TryAdd(1); !errors.Is(err, ErrVolumeFull) {
		t.Fatalf("真值超限后写入应被拒, got %v", err)
	}

	// 真值更小（逻辑记账高估，如去重/纠删码）→ 下调，且不会低于 0。
	pc.CorrectTo(300)
	if pc.Used() != 300 {
		t.Fatalf("修正后应为 300, got %d", pc.Used())
	}
	pc.CorrectTo(-5)
	if pc.Used() != 0 {
		t.Fatalf("负真值应按 0 处理, got %d", pc.Used())
	}

	// 在途预留不被修正误伤：预留后 CorrectTo 只动 committed。
	res, err := pool.TryReserve(100)
	if err != nil {
		t.Fatalf("TryReserve: %v", err)
	}
	pc.CorrectTo(500)
	if pool.Reserved() != 100 {
		t.Fatalf("修正不得触碰 reserved, got %d", pool.Reserved())
	}
	if pc.Used() != 500 {
		t.Fatalf("committed 应为 500, got %d", pc.Used())
	}
	res.Release()
}

// TestPoolCounter_CorrectFrom_FallsBackWhenUnknown 「不掌握真值」必须回落到既有记账，
// 绝不静默降级为 0（否则会凭空释放配额 = 上限失效）。
func TestPoolCounter_CorrectFrom_FallsBackWhenUnknown(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(1000)
	pc := newTestLedgerCounter(t, pool)
	if err := pc.TryAdd(250); err != nil {
		t.Fatal(err)
	}

	if got, ok := pc.CorrectFrom(nil); ok || got != 250 {
		t.Fatalf("nil reporter 应不修正且保持当前占用, got=%d ok=%v", got, ok)
	}
	if got, ok := pc.CorrectFrom(&usageFakeFS{ok: false}); ok || got != 250 {
		t.Fatalf("ok=false 应不修正且保持当前占用, got=%d ok=%v", got, ok)
	}
	if pc.Used() != 250 {
		t.Fatalf("回落场景不得改动账本, got %d", pc.Used())
	}

	got, ok := pc.CorrectFrom(&usageFakeFS{bytes: 900, ok: true})
	if !ok || got != 900 || pc.Used() != 900 {
		t.Fatalf("真值可用时应修正到 900, got=%d ok=%v usage=%d", got, ok, pc.Used())
	}
}

// TestNewPoolWithParent_EncapsulationInheritsSource 封装卷配额继承源卷池：
// 默认（无自身上限）只受父池约束且**不双计**（子池占用是父池的子集，聚合一次）；
// 配自身上限时双重约束。
func TestNewPoolWithParent_EncapsulationInheritsSource(t *testing.T) {
	t.Parallel()
	parent := quota.NewPool(1000)
	child := quota.NewPoolWithParent(parent, 0)
	if child.Parent() != parent {
		t.Fatal("子池应挂到父池")
	}

	// 子池写入 → 沿父链聚合（父池计一次，不因两池而双计）。
	res, err := child.TryReserve(600)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res.Commit(600)
	if parent.Usage() != 600 {
		t.Fatalf("父池应聚合子池已确认占用 600（单次）, got %d", parent.Usage())
	}
	// 父池上限对子池生效：再要 500 → 超父池 1000 → 拒绝。
	if _, err := child.TryReserve(500); !errors.Is(err, quota.ErrStorageFull) {
		t.Fatalf("应受父池上限约束, got %v", err)
	}

	// 自身上限：独立子池配 100 → 请求 150 直接拒绝。
	limited := quota.NewPoolWithParent(quota.NewPool(0), 100)
	if _, err := limited.TryReserve(150); !errors.Is(err, quota.ErrStorageFull) {
		t.Fatalf("应受自身上限约束, got %v", err)
	}
}

// newTestLedgerCounter 是测试用账本计数器（唯一实现）：上限取自传入 pool（保持用例语义），
// 账本落在临时目录。断言须用 `pc.Used()`（账本即权威）——旧 `pc.Used()` 属已删的
// PoolCounter 适配（它把 pool 当存储），账本不再借用 pool 的计数器。
func newTestLedgerCounter(t *testing.T, pool *quota.Pool) *LedgerCounter {
	t.Helper()
	led, err := OpenLedger(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	return NewLedgerCounter(led, pool.MaxBytes())
}
