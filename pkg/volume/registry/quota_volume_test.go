// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

// quota_volume_test.go —— 配额层（M4a）回归：**每用户 + 整卷**双账本 + 足迹委托。
//
// 设计：`docs/designs/2026-10-16-quota-volume-layer.md` §1.3。本片**不挂载**（零行为变更 ✓），
// 故测试直接构造 `QuotaVolume` + 替身卷。
//
// 覆盖：
//  1. 委托（Name/Kind/FS/Close）与「不加变换」（FS 恒等 ✓）；
//  2. 新文件：`Reserve → Commit(0, written)` ⇒ **两侧**（每用户 Scope + 整卷池）均 = written ✓；
//  3. **覆盖写**：`Commit(prev, written)` ⇒ 两侧均 = written（**旧字节被释放，不双计** ✗✓）；
//  4. `ReleaseUsage` ⇒ 两侧归零 ✓；
//  5. **预留回滚**：池拒绝 ⇒ Scope 侧预留已归还（不半入账 ✓）；
//  6. 足迹/上界**委托**替身卷；能力缺失 ⇒ **显式不支持**（ok=false / 保守下界 ✓）。
//
// 变异：`Reservation.Commit` 覆盖写分支改成只 `Commit(written)`（不释放 prev）⇒ 覆盖写断言红。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// fakeQuotaInner 是配额层测试用的替身存储卷（可选能力可开关）。
type fakeQuotaInner struct {
	name      string
	fs        syncpkg.FS
	footprint int64
	fpOK      bool
	maxPhys   int64
}

func (f fakeQuotaInner) Name() string      { return f.name }
func (f fakeQuotaInner) Kind() volume.Kind { return volume.KindStorage }
func (f fakeQuotaInner) FS() syncpkg.FS    { return f.fs }
func (f fakeQuotaInner) Close() error      { return nil }

func (f fakeQuotaInner) Footprint(string, string) (int64, bool)        { return f.footprint, f.fpOK }
func (f fakeQuotaInner) MaxPhysical(_ string, _ string, _ int64) int64 { return f.maxPhys }

// bareQuotaInner 是**不实现任何可选能力**的替身（验证「显式不支持」路径）。
//
// ⚠️ 不能内嵌 fakeQuotaInner（那会**继承** Footprint/MaxPhysical ⇒ 夹具不再是「裸卷」✗，
// 测试会假绿/假红）。故显式只实现 StorageVolume 四个必需方法。
type bareQuotaInner struct{ name string }

func (b bareQuotaInner) Name() string      { return b.name }
func (b bareQuotaInner) Kind() volume.Kind { return volume.KindStorage }
func (b bareQuotaInner) FS() syncpkg.FS    { return nil }
func (b bareQuotaInner) Close() error      { return nil }

func TestQuotaVolume_TwoLedgersAndNoDoubleCount(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(1 << 20)
	userPool := quota.NewPool(1 << 20)
	scope := userPool.Scope("/tenant/alice/user", 1<<20)
	qv := NewQuotaVolume(fakeQuotaInner{name: "v1", fs: syncpkg.NewLocalFS(t.TempDir(), nil)}, func(string) *quota.Scope { return scope }, pool, nil)

	// ① 委托 + 「不加变换」。
	if qv.Name() != "v1" || qv.Kind() != volume.KindStorage || qv.FS() == nil {
		t.Fatalf("委托异常: name=%q kind=%v", qv.Name(), qv.Kind())
	}

	// ② 新文件：Reserve → Commit(0, 8192) ⇒ 两侧 = 8192。
	res, err := qv.Reserve("alice", 8192)
	if err != nil || res == nil {
		t.Fatalf("Reserve: res=%v err=%v", res, err)
	}
	res.Commit(0, 8192)
	if scope.Usage() != 8192 || pool.Usage() != 8192 {
		t.Fatalf("新文件结算：scope=%d pool=%d，want 8192/8192", scope.Usage(), pool.Usage())
	}
	if scope.Reserved() != 0 || pool.Reserved() != 0 {
		t.Fatalf("预留应已清空：scopeReserved=%d poolReserved=%d", scope.Reserved(), pool.Reserved())
	}

	// ③ **覆盖写**：Reserve → Commit(8192, 20480) ⇒ 两侧 = 20480（旧值释放，不双计）。
	res2, err := qv.Reserve("alice", 20480)
	if err != nil || res2 == nil {
		t.Fatalf("Reserve(覆盖): res=%v err=%v", res2, err)
	}
	res2.Commit(8192, 20480)
	if scope.Usage() != 20480 || pool.Usage() != 20480 {
		t.Fatalf("覆盖写结算：scope=%d pool=%d，want 20480/20480（旧值须释放，禁双计）", scope.Usage(), pool.Usage())
	}

	// ④ 删除释放 ⇒ 两侧归零。
	qv.ReleaseUsage("alice", 20480)
	if scope.Usage() != 0 || pool.Usage() != 0 {
		t.Fatalf("删除释放后：scope=%d pool=%d，want 0/0", scope.Usage(), pool.Usage())
	}
}

func TestQuotaVolume_ReserveRollbackOnPoolReject(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100) // 整卷池极小 ⇒ 必拒
	userPool := quota.NewPool(1 << 20)
	scope := userPool.Scope("/tenant/alice/user", 1<<20)
	qv := NewQuotaVolume(fakeQuotaInner{name: "v1"}, func(string) *quota.Scope { return scope }, pool, nil)

	if _, err := qv.Reserve("alice", 4096); err == nil {
		t.Fatal("池不足时 Reserve 应报错")
	}
	// **不半入账**：Scope 侧已取得的预留必须归还。
	if scope.Reserved() != 0 {
		t.Fatalf("池拒绝后 Scope 侧预留应归还，got reserved=%d", scope.Reserved())
	}
	if scope.Usage() != 0 {
		t.Fatalf("失败路径不得产生已确认占用，got usage=%d", scope.Usage())
	}
}

func TestQuotaVolume_FootprintDelegationAndExplicitUnsupported(t *testing.T) {
	t.Parallel()
	// 实现能力的替身 ⇒ 委托。
	qv := NewQuotaVolume(fakeQuotaInner{name: "v1", footprint: 8244, fpOK: true, maxPhys: 8300}, nil, nil, nil)
	if n, ok := qv.Footprint("alice", "user/a.txt"); !ok || n != 8244 {
		t.Fatalf("Footprint 应委托替身：n=%d ok=%v", n, ok)
	}
	if got := qv.MaxPhysical("alice", "user/a.txt", 8192); got != 8300 {
		t.Fatalf("MaxPhysical 应委托替身：got=%d", got)
	}
	// 能力缺失 ⇒ **显式不支持**（不静默回落成逻辑值）。
	bare := NewQuotaVolume(bareQuotaInner{name: "v2"}, nil, nil, nil)
	if n, ok := bare.Footprint("alice", "user/a.txt"); ok || n != 0 {
		t.Fatalf("能力缺失时 Footprint 必须显式 ok=false，got n=%d ok=%v", n, ok)
	}
	if got := bare.MaxPhysical("alice", "user/a.txt", 8192); got != 8192 {
		t.Fatalf("能力缺失时 MaxPhysical 应为保守下界（logical），got=%d", got)
	}
}

func TestQuotaVolume_WholeVolumeCapacity(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(4096)
	qv := NewQuotaVolume(fakeQuotaInner{name: "v1"}, nil, pool, nil)
	if got := qv.Capacity(); got != 4096 {
		t.Fatalf("Capacity=%d want 4096", got)
	}
	qv.SetCapacity(8192)
	if got := qv.Capacity(); got != 8192 {
		t.Fatalf("SetCapacity 后 Capacity=%d want 8192", got)
	}
	if used, total, ok := qv.VolumeStats(); !ok || total != 8192 || used != 0 {
		t.Fatalf("VolumeStats=(%d,%d,%v) want (0,8192,true)", used, total, ok)
	}
}
