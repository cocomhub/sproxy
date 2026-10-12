// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// trueusage.go 实现 capacity.TrueUsageReporter：让**掌握物理布局**的这一层（透明加解密 +
// 分块 + meta pad + XOR parity）向容量账本报告**本卷真实落盘字节**，供
// pkg/server/quota_reconcile.go 优先采信（与磁盘扫描交叉校验），消除
// 「逻辑记账 ≠ 物理占用」的失真（评审实证：加密卷 1GiB 限额实际可落 ~1.5TiB）。
//
// 口径（用户裁定 2026-10-12）：**本卷落盘字节，不含副本** ——
//   - 内容分块：`ChunkInfo.EncSize` 之和（密文长度，含每块头/长度头）；
//   - XOR parity：`ParityInfo.EncSize`（Erasure 且 ≥2 分块时存在）；
//   - meta blob：**落盘长度**（含 meta pad 随机化膨胀，不是逻辑 JSON 长度）。
//
// 副本（NewFSMultiplicas 的 replicas）物理落在**其它卷**，由那个卷自己的账本与对账负责；
// 在本卷计副本会造成跨卷双计与卷池虚高，故明确不计。
//
// 不计入项（文档化，避免与磁盘扫描的差异被误判为漏记）：目录 meta blob（每目录数百字节
// 量级）、GC 尚未回收的孤儿 blob、并发未命中去重时留下的冗余分块；这些由对账的磁盘扫描
// 口径覆盖，交叉校验用容差判定（见 quota_reconcile.go volumeTruthTolerance）。
//
// 实现成本 O(索引)——接口契约允许（「O(1) 或 O(索引)」，不得全卷扫描）；只在周期对账调用。

// physBytesOf 计算单个条目在**本卷**的物理落盘字节。
// metaBlobLen 是 meta blob 落盘长度（写路径 = len(metaBlob)，加载路径 = 读到的 blob 长度）。
// dedup=true 表示内容分块归**共享去重容器**（由 dedupBlob 单独计一次，见 contentPhysBytes），
// 本条目只计 meta blob，避免多引用重复计数。
func physBytesOf(mm *shardseal.Meta, metaBlobLen int64, dedup bool) int64 {
	if metaBlobLen < 0 {
		metaBlobLen = 0
	}
	if mm == nil {
		return metaBlobLen
	}
	n := metaBlobLen
	if dedup {
		return n
	}
	for _, c := range mm.Chunks {
		n += c.EncSize
	}
	if mm.Parity != nil {
		n += mm.Parity.EncSize
	}
	return n
}

// contentPhysBytes 计算**共享去重内容 blob** 的物理字节（内容分块 + parity；meta 归各引用
// 条目自己计），只按池条目计一次。
func contentPhysBytes(mm *shardseal.Meta) int64 {
	if mm == nil {
		return 0
	}
	var n int64
	for _, c := range mm.Chunks {
		n += c.EncSize
	}
	if mm.Parity != nil {
		n += mm.Parity.EncSize
	}
	return n
}

// TrueUsage 返回本卷**物理落盘字节**：索引内全部条目的 `phys`（内容分块 + meta blob + parity）
// 之和，加去重容器共享内容 blob（每 blob 只计一次）。ok 恒为 true——本层掌握自身布局，
// 「不掌握」在该层不成立；调用方仍必须按 ok=false 分支处理其他卷层。
func (s *SecretdataFS) TrueUsage() (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, e := range s.index {
		total += e.phys
	}
	for _, b := range s.dedupPool {
		total += b.phys
	}
	return total, true
}

// TrueSize 返回单个 rel 的物理占用（内容分块 + meta blob + parity）。去重引用条目的**共享
// 内容**不摊派到单文件（多引用重复计数会虚高），只含本条目自身的 meta blob；共享内容在
// TrueUsage 里按池条目计一次。rel 不在索引 → ok=false（不推 0）。
func (s *SecretdataFS) TrueSize(rel string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.index[rel]
	if !ok || e == nil {
		return 0, false
	}
	return e.phys, true
}
