// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestLedger_OwnShardReapedThenAddStillCounted 钉住「本进程分片被回收（含被**他进程**回收）后，
// 本进程后续增量仍必须计入 Total()」。
//
// 缺陷（独立审核 P0，2026-10-18）：`reapEmptyShards` 回收索引内**全部**空分片（**不排除本进程**
// ——它自己刚 `Compact()` 截断为空的那一个也在内）；而 `registerShardLocked` 的 `registered`
// 是**粘性**标志（「进程内只做一次」）⇒ 此后 `Add` 只往 `shard.<node>.log` 追加、**不再登记索引**，
// 而 `shardNamesLocked` **只从索引枚举** ⇒ 本进程全部增量对 `Total()` 不可见：
//   - 卷容量上限静默放宽（TryAdd 基于被冻结的旧总和）；
//   - 若期间重启，未登记分片里的增量永久不参与求和（丢账）。
//
// 触发极日常：任何「上传后又删除」使本进程净贡献为 0 ⇒ 下一次周期对账即回收其空分片。
//
// 本用例是**确定性**的（无时序依赖）：Add(100) → Compact()（分片空、基线 100）→ 回收（本进程
// 分片被摘除+删除）→ Add(50) ⇒ Total 必须是 150；未修必得 100（红）。
func TestLedger_OwnShardReapedThenAddStillCounted(t *testing.T) {
	t.Parallel()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	led, err := OpenLedgerFS(inner, LedgerDirName, "node")
	if err != nil {
		t.Fatal(err)
	}
	if aerr := led.Add(100); aerr != nil {
		t.Fatal(aerr)
	}
	// 折叠进基线并截断分片（本进程分片此刻为空）。
	if cerr := led.Compact(); cerr != nil {
		t.Fatal(cerr)
	}
	if got, ok := led.Total(); !ok || got != 100 {
		t.Fatalf("Compact 后 Total=%d ok=%v, want 100/true", got, ok)
	}
	// 回收空分片（不排除本进程 ⇒ 本进程分片被摘除索引 + 删除文件）。
	if rerr := led.reapEmptyShards(); rerr != nil {
		t.Fatal(rerr)
	}
	if got, ok := led.Total(); !ok || got != 100 {
		t.Fatalf("回收后 Total=%d ok=%v, want 100/true（基线须保留）", got, ok)
	}
	// 本进程继续写入：该增量必须计入（未修必红：分片不在索引 ⇒ 只得到 100）。
	if aerr := led.Add(50); aerr != nil {
		t.Fatal(aerr)
	}
	if got, ok := led.Total(); !ok || got != 150 {
		t.Fatalf("回收后再 Add ⇒ Total=%d ok=%v, want 150/true"+
			"（未修必红：本进程分片被摘索引且 registered 粘性 ⇒ 增量对 Total 不可见）", got, ok)
	}
}

// TestLedger_OwnShardReapedZeroBaselineStillCounted 同型但基线为 0（从未折叠过非零值）：
// `unindexShardsLocked` 连 snap 行都不保留 ⇒ 本进程分片/基线在索引中**全部消失**，
// 此后增量同样必须计入。
func TestLedger_OwnShardReapedZeroBaselineStillCounted(t *testing.T) {
	t.Parallel()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	led, err := OpenLedgerFS(inner, LedgerDirName, "node")
	if err != nil {
		t.Fatal(err)
	}
	// 先写再抵消 ⇒ 分片为空且基线为 0（净贡献恰好 0）。
	if aerr := led.Add(100); aerr != nil {
		t.Fatal(aerr)
	}
	if rerr := led.Add(-100); rerr != nil {
		t.Fatal(rerr)
	}
	if cerr := led.Compact(); cerr != nil {
		t.Fatal(cerr)
	}
	if rerr := led.reapEmptyShards(); rerr != nil {
		t.Fatal(rerr)
	}
	if aerr := led.Add(50); aerr != nil {
		t.Fatal(aerr)
	}
	if got, ok := led.Total(); !ok || got != 50 {
		t.Fatalf("Total=%d ok=%v, want 50/true（未修必红：零基线被摘除后增量不可见）", got, ok)
	}
}
