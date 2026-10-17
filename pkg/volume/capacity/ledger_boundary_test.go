// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// ledger_boundary_test.go —— 账本核心正确性的**确定性回归**（2026-10-18 并行审查
// 修复固化）。每条用例「未修必红/修后必绿」：
//
//   - A1 并发 TryAdd「检查-再追加」非原子 ⇒ 双过双入账可超限（探针实测 552/3000）；
//   - A2 fsStore 降级清零 ⇒ 已记账占用从 Total 视角消失（探针实测 4096→0）；
//   - A3 读失败与「不存在」混同 ⇒ 瞬态错误把分片当 0（探针实测 Total=0 ok=true）；
//   - A4 索引文件丢失 ⇒ Total 静默归零（探针实测分片仍在盘上但读不到）；
//   - A7 Release 无条件扣减 ⇒ 账本拖负 ⇒ 凭负基线放行更多。

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestLedgerCounter_ConcurrentTryAddNeverOverruns 并发 TryAdd 下账本总和**恒不超 max**
// （进程内「检查-再追加」必须原子；无 pool 兜底 = 用户卷场景）。
func TestLedgerCounter_ConcurrentTryAddNeverOverruns(t *testing.T) {
	t.Parallel()
	const max = int64(100)
	led, err := OpenLedger(t.TempDir(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	c := NewLedgerCounter(led, max) // 无 WithPool：用户卷无池兜底

	const workers = 8
	const iters = 300
	var wg sync.WaitGroup
	var overrun atomic.Bool
	for range workers {
		wg.Go(func() {
			for range iters {
				if err := c.TryAdd(60); err == nil {
					// 成功一次即 +60；并发判定若非原子，总和可冲到 120>max。
					if got, _ := c.l.Total(); got > max {
						overrun.Store(true)
						return
					}
				}
			}
		})
	}
	wg.Wait()
	if overrun.Load() {
		t.Fatalf("并发 TryAdd 后账本总和 > max=%d：进程内判定非原子（未修必红）", max)
	}
	if got, _ := c.l.Total(); got > max {
		t.Fatalf("最终总和 %d > max %d", got, max)
	}
}

// TestFSStore_DegradeSeedsVolumeTotal 降级到内存账本时**保留已记账占用**。
func TestFSStore_DegradeSeedsVolumeTotal(t *testing.T) {
	t.Parallel()
	fsys := syncpkg.NewLocalFS(t.TempDir(), nil)
	led, err := OpenLedgerFS(fsys, LedgerDirName, "seed-node")
	if err != nil {
		t.Fatal(err)
	}
	if aerr := led.Add(4096); aerr != nil {
		t.Fatal(aerr)
	}
	// 触发降级（写路径任一失败即 degrade）。
	store, ok := led.store.(*fsStore)
	if !ok {
		t.Fatalf("store 类型 %T, want *fsStore", led.store)
	}
	store.degrade()
	total, ok := led.Total()
	if !ok || total != 4096 {
		t.Fatalf("降级后 Total=%d ok=%v, want 4096/true（已记账占用不得清零，未修必红）", total, ok)
	}
	// 降级后继续追加：总和递增（内存账本可写）。
	if aerr := led.Add(2048); aerr != nil {
		t.Fatal(aerr)
	}
	if got, _ := led.Total(); got != 4096+2048 {
		t.Fatalf("降级后追加 Total=%d, want %d", got, 4096+2048)
	}
}

// flakyReadFS 包装本地 FS：fail=true 时 OpenRead 恒返回**非 not-exist 真错误**
// （模拟远端瞬态抖动）。MakeDir/Stat/WriteFile 委托 inner（账本可构造、可写）。
type flakyReadFS struct {
	syncpkg.FS
	fail atomic.Bool
}

func (f *flakyReadFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	if f.fail.Load() {
		return nil, fmt.Errorf("flaky: transient read failure")
	}
	return f.FS.OpenRead(ctx, p)
}

// TestLedger_ReadErrorIsUndetermined 读错误（非 not-exist）⇒ Total 不可确定（ok=false，
// fail-closed 拒写）；不得当 0（fail-open）。
func TestLedger_ReadErrorIsUndetermined(t *testing.T) {
	t.Parallel()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	fsys := &flakyReadFS{FS: inner}
	led, err := OpenLedgerFS(fsys, LedgerDirName, "node")
	if err != nil {
		t.Fatal(err)
	}
	if aerr := led.Add(100); aerr != nil {
		t.Fatal(aerr)
	}
	fsys.fail.Store(true)
	if _, ok := led.Total(); ok {
		t.Fatal("读错误必须不可确定（ok=false fail-closed；未修必红：分片当 0 fail-open）")
	}
	// 恢复后自愈（瞬态错误不永久 corrupt）。
	fsys.fail.Store(false)
	if got, ok := led.Total(); !ok || got != 100 {
		t.Fatalf("读错误恢复后 Total=%d ok=%v, want 100/true（瞬态自愈）", got, ok)
	}
}

// TestLedger_IndexLostAfterRegisterFailsClosed 索引文件丢失（且本进程已登记）⇒ corrupt
// fail-closed；不得静默归零。
func TestLedger_IndexLostAfterRegisterFailsClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, nil)
	led, err := OpenLedgerFS(fsys, LedgerDirName, "node")
	if err != nil {
		t.Fatal(err)
	}
	if aerr := led.Add(100); aerr != nil {
		t.Fatal(aerr) // 首次 Add 触发 registerShardLocked（registered=true）
	}
	if err := os.Remove(filepath.Join(dir, "meta/quota", ledgerIndexName)); err != nil {
		t.Fatal(err)
	}
	if _, ok := led.Total(); ok {
		t.Fatal("索引丢失且已登记必须 fail-closed（未修必红：静默归零 ok=true）")
	}
}

// TestLedgerCounter_ReleaseClampedToZero Release 超出已记账占用 ⇒ 钳制到 0（防负漂移）。
func TestLedgerCounter_ReleaseClampedToZero(t *testing.T) {
	t.Parallel()
	led, err := OpenLedger(t.TempDir(), "a7")
	if err != nil {
		t.Fatal(err)
	}
	c := NewLedgerCounter(led, 100)
	if err := c.TryAdd(50); err != nil {
		t.Fatal(err)
	}
	c.Release(80) // 释放超出入账：钳到 0，不得拖负
	if got := c.Used(); got != 0 {
		t.Fatalf("Release 钳制后 Used=%d, want 0（未修必红：负漂移 %d）", got, got)
	}
}

// TestLedgerCounter_CompactKeepsTotalAndReapsEmpty 周期压缩接线：Compact 折叠分片进基线
// （Total 不变）+ ReapEmptyShards 回收空分片并从索引摘除（评审 S1/S3）。
func TestLedgerCounter_CompactKeepsTotalAndReapsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, nil)
	led, err := OpenLedgerFS(fsys, LedgerDirName, "compact-node")
	if err != nil {
		t.Fatal(err)
	}
	c := NewLedgerCounter(led, 1000)
	if err := c.TryAdd(400); err != nil {
		t.Fatal(err)
	}
	if err := c.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	// 压缩后 Total 不变（增量已折入基线，分片被截断为空）。
	if got := c.Used(); got != 400 {
		t.Fatalf("Compact 后 Used=%d, want 400", got)
	}
	shardPath := filepath.Join(dir, "meta/quota", led.shardName())
	shardData, serr := os.ReadFile(shardPath)
	if serr != nil {
		t.Fatalf("分片文件应存在（空内容）: %v", serr)
	}
	if strings.TrimSpace(string(shardData)) != "" {
		t.Fatalf("Compact 后分片应为空, got %q", shardData)
	}
	// 再写入：新增量进分片（基线 400 + 增量 100）。
	if err := c.TryAdd(100); err != nil {
		t.Fatal(err)
	}
	if got := c.Used(); got != 500 {
		t.Fatalf("压缩后再写 Used=%d, want 500", got)
	}
	// 回收空分片：分片非空 ⇒ 不得回收；Compact 后再回收 ⇒ 删除 + 摘索引。
	if err := c.ReapEmptyShards(); err != nil {
		t.Fatalf("ReapEmptyShards: %v", err)
	}
	if _, err := os.Stat(shardPath); err != nil {
		t.Fatalf("非空分片不得被回收: %v", err)
	}
	if err := c.Compact(); err != nil {
		t.Fatalf("第二次 Compact: %v", err)
	}
	if err := c.ReapEmptyShards(); err != nil {
		t.Fatalf("第二次 ReapEmptyShards: %v", err)
	}
	if _, err := os.Stat(shardPath); !os.IsNotExist(err) {
		t.Fatalf("空分片应被回收删除: %v", err)
	}
	// 回收后 Total 不变（增量已折入基线）。
	if got := c.Used(); got != 500 {
		t.Fatalf("回收后 Used=%d, want 500", got)
	}
}

// TestWriteVolumeState_AtomicWriteNoTmpResidue 状态文件原子写：读回完整两行、无 .tmp 残留
// （评审 S4——直接写崩溃会残缺 ⇒ LoadVolumeLimit 返回 0 ⇒ 上限被抹成「不限」）。
func TestWriteVolumeState_AtomicWriteNoTmpResidue(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "v.limit")
	if err := WriteVolumeState(path, 100, 60); err != nil {
		t.Fatalf("WriteVolumeState: %v", err)
	}
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || lines[0] != "100" || lines[1] != "60" {
		t.Fatalf("状态文件内容 %q, want 两行 100/60", string(b))
	}
	// LoadVolumeLimit/LoadVolumeUsed 读回一致。
	if got := LoadVolumeLimit(path, 0, false); got != 100 {
		t.Fatalf("LoadVolumeLimit=%d, want 100", got)
	}
	if got := LoadVolumeUsed(path); got != 60 {
		t.Fatalf("LoadVolumeUsed=%d, want 60", got)
	}
	// 无 .tmp 残留（原子 rename 成功）。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("原子写后不得残留 .tmp: %v", err)
	}
}
