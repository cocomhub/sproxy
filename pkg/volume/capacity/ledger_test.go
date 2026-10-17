// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ledger_test.go —— 共享卷跨进程配额账本（片 7）的 TDD。
//
// 设计文档：`docs/designs/2026-10-17-quota-ledger-persistence.md`。
//
// 测试策略：用**同目录下的两个 Ledger 实例**模拟两个进程（各自 node 不同），
// 因为跨进程语义的全部要素（独立分片、只写自己、读时求和）都在这两个实例之间体现。

const ledgerGiB = int64(1) << 30

// 注：原 S0 反例 `TestLedger_LocalOnlyCounterExceeds`（用**旧单文件计数器**证明
// 「多进程共享同一路径 ⇒ 双双通过 ⇒ 超限」）已随旧计数器删除而移除 —— 其使命已完成：
// 该事实作为**删除旧两套机制的理由**记录在设计文档 §3.3b（唯一实现原则）。

// 注：原 S0 反例（用**旧单文件计数器**证明「多进程共享同一路径 ⇒ 双双通过 ⇒ 总和超限」）
// 已随旧两套计数器删除而移除 —— 其使命已完成：该事实作为**删除旧机制的理由**记录在设计
// 文档 §3.3b（唯一实现原则）。当前只有分片日志账本一套，跨进程正确性由下方用例守护。

// TestLedger_CrossProcessVisibility 是**核心不变量**（S2）：共享卷上，
// 一个进程的追加必须被另一个进程在**判定**时看见 ⇒ 超限被拒。
func TestLedger_CrossProcessVisibility(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "quota")
	const cap10 = 10 * ledgerGiB

	l1, err := OpenLedger(dir, "node-a")
	if err != nil {
		t.Fatalf("OpenLedger(node-a): %v", err)
	}
	defer l1.Close()
	l2, err := OpenLedger(dir, "node-b")
	if err != nil {
		t.Fatalf("OpenLedger(node-b): %v", err)
	}
	defer l2.Close()

	c1 := NewLedgerCounter(l1, cap10)
	c2 := NewLedgerCounter(l2, cap10)

	// 各预置 4.5GiB ⇒ 全局 9GiB。
	for _, c := range []*LedgerCounter{c1, c2} {
		if err := c.TryAdd(9 * ledgerGiB / 2); err != nil {
			t.Fatalf("预置 4.5GiB: %v", err)
		}
	}
	// 全局和必须可见（这是「跨进程」的全部要点）。
	if got, ok := l1.Total(); !ok || got != 9*ledgerGiB {
		t.Fatalf("node-a 看到全局和=%d ok=%v want %d（必须求和所有分片）", got, ok, 9*ledgerGiB)
	}
	// node-a 再要 1.5GiB：全局 9+1.5 > 10 ⇒ **必须拒**（现状：各看各的 ⇒ 会通过）。
	if err := c1.TryAdd(3 * ledgerGiB / 2); !errors.Is(err, ErrVolumeFull) {
		t.Fatalf("超限应返回 ErrVolumeFull，got %v（跨进程不可见 ⇒ 会静默超限）", err)
	}
	// 拒绝必须**零副作用**：账本和不变。
	if got, _ := l1.Total(); got != 9*ledgerGiB {
		t.Fatalf("被拒后全局和=%d want %d（拒绝不得改账本）", got, 9*ledgerGiB)
	}
	// 1GiB 仍在限内 ⇒ 通过，且另一个进程立刻可见。
	if err := c1.TryAdd(ledgerGiB); err != nil {
		t.Fatalf("限内追加应通过: %v", err)
	}
	if got, _ := l2.Total(); got != 10*ledgerGiB {
		t.Fatalf("node-b 看到全局和=%d want %d", got, 10*ledgerGiB)
	}
}

// TestLedger_ReleaseVisibleCrossProcess：释放同样跨进程可见（否则只增不减 ⇒ 假 507）。
func TestLedger_ReleaseVisibleCrossProcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l1, err := OpenLedger(dir, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	l2, err := OpenLedger(dir, "b")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()

	c1 := NewLedgerCounter(l1, 0) // 不限，只看和
	c2 := NewLedgerCounter(l2, 0)
	if err := c1.TryAdd(7); err != nil {
		t.Fatal(err)
	}
	if err := c2.TryAdd(3); err != nil {
		t.Fatal(err)
	}
	c1.Release(5)
	if got, _ := l2.Total(); got != 5 {
		t.Fatalf("跨进程释放后和=%d want 5", got)
	}
}

// TestLedger_CompactPreservesTotal（S3）：压缩把自身分片折进自身基线，**和不改变**。
func TestLedger_CompactPreservesTotal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := OpenLedger(dir, "n1")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for range 5 {
		if err := l.Add(100); err != nil {
			t.Fatal(err)
		}
	}
	before, ok := l.Total()
	if !ok || before != 500 {
		t.Fatalf("压缩前和=%d ok=%v want 500", before, ok)
	}
	if err := l.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after, ok := l.Total()
	if !ok || after != before {
		t.Fatalf("压缩后和=%d ok=%v want %d（压缩不得改变和）", after, ok, before)
	}
	// 压缩必须真的折叠：分片应为空、基线存在。
	if _, err := os.Stat(filepath.Join(dir, ledgerSnapPrefix+"n1")); err != nil {
		t.Fatalf("压缩后应存在基线文件: %v", err)
	}
	// 压缩后继续追加仍正确（基线 + 新增量）。
	if err := l.Add(7); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Total(); got != 507 {
		t.Fatalf("压缩后追加和=%d want 507", got)
	}
}

// TestLedger_CorruptShardFailClosed（S5/§3.2）：分片不可解析 ⇒ **拒绝写入**（不 fail-open），
// 且计入可观测计数。
func TestLedger_CorruptShardFailClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := OpenLedger(dir, "me")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Add(10); err != nil {
		t.Fatal(err)
	}
	// 另一个进程的分片损坏（不可解析）。
	if err := os.WriteFile(filepath.Join(dir, ledgerShardPrefix+"other"+ledgerShardSuffix), []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 让坏分片**进入索引**：索引是账本内唯一枚举来源（不依赖 ListDir ⇒ 见 ledgerIndexName 说明）
	// ⇒ 不登记就不会被扫描，用例将失去意义。
	if ierr := os.WriteFile(filepath.Join(dir, ledgerIndexName),
		[]byte(ledgerShardPrefix+"other"+ledgerShardSuffix+"\n"), 0o644); ierr != nil {
		t.Fatal(ierr)
	}
	before := CorruptLedgerCount()
	c := NewLedgerCounter(l, 1000)
	if err := c.TryAdd(1); !errors.Is(err, ErrLedgerDegraded) {
		t.Fatalf("账本损坏必须 fail-closed（ErrLedgerDegraded），got %v", err)
	}
	if !c.Degraded() {
		t.Fatal("损坏后 Degraded() 应为 true")
	}
	if got := CorruptLedgerCount(); got <= before {
		t.Fatalf("损坏必须计入 CorruptLedgerCount（before=%d after=%d）", before, got)
	}
	// 损坏分片被**跳过**（不当作 0 混入和）：Total 返回 ok=false。
	if _, ok := l.Total(); ok {
		t.Fatal("损坏分片存在时 Total 必须返回 ok=false")
	}
}

// TestLedger_NodeSanitized：节点标识含路径分隔符时不得跨目录写（文件名安全）。
func TestLedger_NodeSanitized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := OpenLedger(dir, "host/1:2")
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	defer l.Close()
	if l.Node() == "host/1:2" {
		t.Fatalf("节点标识必须归一为文件名安全片段，got %q", l.Node())
	}
	// 分片**惰性创建**（首次追加才落文件）：只读场景不留空文件。
	if aerr := l.Add(1); aerr != nil {
		t.Fatal(aerr)
	}
	shards, err := ListLedgerShards(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 1 {
		t.Fatalf("应恰有 1 个分片文件，got %v", shards)
	}
}

// TestOpenLedger_RejectsEmpty：空目录/空节点 ⇒ 错误（调用方据此退回纯内存路径）。
func TestOpenLedger_RejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := OpenLedger("", "n"); err == nil {
		t.Fatal("空目录应报错")
	}
	if _, err := OpenLedger(t.TempDir(), ""); err == nil {
		t.Fatal("空节点应报错")
	}
}
