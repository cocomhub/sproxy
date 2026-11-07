// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"bytes"
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ledger_no_selfaccount_test.go —— 钉住「**账本自身不自记账**」这条不变式。
//
// 为什么必须钉：账本必须与卷**同处**（跨进程可见），而它又写在**同一个卷 FS** 上；
// 若账本写入被同一个 `CapacityFS` 计数，则每追加一行就抬高 used ⇒ **自记账正反馈
// （runaway）** ⇒ 卷很快被自己的账本填满 ✗。实现上靠「账本建在**原始** FS（绕过
// `CapacityFS`）」保证 ⇒ 本用例把这个保证变成可判定的断言。
//
// 变异：把账本改建在 `CapacityFS` 之上（或让 counter 计到账本写入）⇒ 红。

// TestLedger_DoesNotCountItsOwnWrites 账本写入**不得**抬高卷级已用。
func TestLedger_DoesNotCountItsOwnWrites(t *testing.T) {
	t.Parallel()
	// 同一份底层 FS：`CapacityFS` 包在外层计数；账本建在**原始** FS 上（生产同构）。
	raw := syncpkg.NewLocalFS(t.TempDir(), nil)
	led, err := OpenLedgerFS(raw, LedgerDirName, "self-node")
	if err != nil {
		t.Fatalf("OpenLedgerFS: %v", err)
	}
	counter := NewLedgerCounter(led, 0)
	fs := Wrap(raw, counter)

	const payload = 4096
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "data.bin",
		bytes.NewReader(make([]byte, payload)), payload, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	// 已用必须**恰好**等于数据字节：账本自身的 shard/index 写入不得被计入。
	if got := counter.Used(); got != payload {
		t.Fatalf("Used=%d want %d（账本自身写入不得被计入 ⇒ 否则自记账正反馈）", got, payload)
	}
}
