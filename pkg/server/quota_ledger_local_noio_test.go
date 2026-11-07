// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// quota_ledger_local_noio_test.go —— 钉住「**本地默认卷零账本 I/O**」。
//
// 为什么必须钉：账本是为**共享卷**（多进程可见同一份存储）而设；本地默认卷各节点自有磁盘
// ⇒ 各自内存账本**本就正确** ⇒ 给它落账本只是白付 I/O ✗（设计 §3.1 P1）。
// 实现上靠「本地默认卷不经 `CapacityFS`」保证 ⇒ 本用例把它变成可判定断言。
//
// 变异：给本地默认卷也挂 `CapacityFS`/账本 ⇒ 红。

// TestLocalVolume_NoLedgerIO 写本地默认卷不得产生任何账本目录/分片。
func TestLocalVolume_NoLedgerIO(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	mux := actorFileWriteMux(h, "alice")

	body := bytes.Repeat([]byte("l"), 512)
	if st, resp := writeMuxUpload(t, mux, "local.txt", "", body); st != http.StatusOK {
		t.Fatalf("写本地默认卷 = %d (body: %s)", st, resp)
	}

	root := h.cfgPtr.Load().StorageRoot
	// ① 不得出现账本目录。
	if _, err := os.Stat(filepath.Join(root, "meta", "quota")); !os.IsNotExist(err) {
		t.Fatalf("本地卷不得产生账本目录 meta/quota（零 I/O），stat err=%v", err)
	}
	// ② 整个存储根下不得出现账本分片/索引/基线（`shard.*.log` / `shards` / `snap.*`）。
	var found []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		n := d.Name()
		if strings.HasPrefix(n, "shard.") || n == "shards" || strings.HasPrefix(n, "snap.") {
			found = append(found, p)
		}
		return nil
	})
	if len(found) > 0 {
		t.Fatalf("本地卷不得产生账本文件（零 I/O），实际: %v", found)
	}
}
