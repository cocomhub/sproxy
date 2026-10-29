// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// list_checksum_allocs_test.go 是目录列举的**性能回归门禁**：List 的 checksum 取用必须
// 逐条 O(1)（只看本目录条目），不得随台账规模增长而拷贝全量快照。
//
// 背景：`fileInfosFromViewEntries` / `listVolumeDirEntries` 曾每卷调用
// `checksumSnapshotFor`（`GetAll()` + 按卷拆分 = 3 份全量 map）——大库下每次浏览目录都会
// 分配 O(全库) 内存。改用 `checksumGetter` 后分配与台账规模无关。

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

// TestList_ChecksumAllocsFlatWithLedgerSize：台账从 1 条增到 2 万条，单次 List 的分配次数
// 不应显著变化（阈值宽松，只拦「O(全库) 拷贝回归」这一量级）。
//
// 变异：`checksumGetter` 内先 `cs.GetAll()` 再查（回退全量快照）⇒ 分配随台账线性增长 ⇒ 红。
// sproxy:serial: AllocsPerRun 与并行测试不兼容（testing 包显式 panic：called during parallel test）。
func TestList_ChecksumAllocsFlatWithLedgerSize(t *testing.T) {
	// sproxy:serial: 性能门禁以 MemStats 度量分配，与并行测试互相干扰（测量需独占）。
	env := viewEnv(t, false)
	userDir := filepath.Join(env.volDirs["default"], "alice", "user")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	env.rebuild()
	st := env.svc.rt.checksumStore("alice")
	if st == nil {
		t.Fatal("checksum store 未装配")
	}
	checksumSet(st, "", "user/a.txt", "1111111111111111111111111111111111111111111111111111111111111111")

	small := allocBytesPerRun(func() { _ = listFileInfos(t, env, "") })
	for i := range 500 {
		checksumSet(st, "", "user/noise-"+itoa(i)+".txt", "2222222222222222222222222222222222222222222222222222222222222222")
	}
	large := allocBytesPerRun(func() { _ = listFileInfos(t, env, "") })
	// 阈值按「每 500 条台账约 40KB+」量级取 8KB（惰性实现差异应接近 0）。
	if large > small+8<<10 {
		t.Fatalf("List 的分配随台账规模增长（small=%dB large=%dB per run）：checksum 全量快照拷贝回归", small, large)
	}
	if got := listFileInfos(t, env, ""); len(got) != 1 || got[0].Checksum == "" {
		t.Fatalf("列表 checksum 丢失: %+v", got)
	}
}

// allocBytesPerRun 返回单次调用的平均分配字节数（TotalAlloc 单调，不受 GC 影响）。
func allocBytesPerRun(fn func()) uint64 {
	const rounds = 20
	var m1, m2 goruntime.MemStats
	goruntime.GC()
	goruntime.ReadMemStats(&m1)
	for range rounds {
		fn()
	}
	goruntime.ReadMemStats(&m2)
	return (m2.TotalAlloc - m1.TotalAlloc) / rounds
}

// itoa 避免 import strconv（测试内只用十进制小整数）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
