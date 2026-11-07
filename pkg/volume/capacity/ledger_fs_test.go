// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ledger_fs_test.go —— **卷内账本**（`fsStore`，生产路径）的回归。
//
// 为什么单独钉住：生产上账本必须落在**卷内 FS**（跨进程可见）；若该路径静默降级为内存，
// 跨进程正确性就**无声失效**（只留指标）。本用例直接断言「一个节点写入、另一个节点读到」。

// TestLedgerFS_SeesOtherNodeShard 卷内账本：跨节点（不同分片）**互相可见**。
func TestLedgerFS_SeesOtherNodeShard(t *testing.T) {
	t.Parallel()
	fsys := syncpkg.NewLocalFS(t.TempDir(), nil)

	seed, err := OpenLedgerFS(fsys, LedgerDirName, "seed-node")
	if err != nil {
		t.Fatalf("OpenLedgerFS(seed): %v", err)
	}
	if aerr := seed.Add(4096); aerr != nil {
		t.Fatalf("seed Add: %v", aerr)
	}

	other, err := OpenLedgerFS(fsys, LedgerDirName, "other-node")
	if err != nil {
		t.Fatalf("OpenLedgerFS(other): %v", err)
	}
	if other.Degraded() {
		t.Fatal("卷内账本不得降级（卷 FS 应能承载账本目录）")
	}
	got, ok := other.Total()
	if !ok || got != 4096 {
		t.Fatalf("Total=%d ok=%v，want 4096/true（卷内账本必须看到其他节点的分片）", got, ok)
	}
}
