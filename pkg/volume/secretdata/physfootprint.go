// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"path"
	"strings"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// physfootprint.go 向**配额账本**报告本卷的「真实物理落盘字节」与「逻辑 → 物理上界」
// （配额模型 v3，用户裁定 2026-10-14：**只按物理占用记账**）。
//
// 为什么必须由本层报告（而不是让写内核按逻辑长度折算）：
//   - 内容密文：每 blocklet 多 12B nonce + 16B GCM tag；
//   - XOR parity（`extra.erasure`）：明文 = **最长数据分块**（≤ Block.Max）⇒ 大文件可达逻辑的 2×；
//   - **meta blob：pad 到 [base, 2·base)（默认 base = Block.Min = 1MB）**——尺寸混淆设计让 meta blob
//     与底层分块分布重叠，其量级与文件**逻辑大小无关**。
//
// ⇒ 按逻辑长度记账在小文件 / 多目录场景下，物理可达逻辑的**千倍**（10k 个小文件的 meta pad 即
// 10–20GB，而账本只记 ~10MB）⇒ 严重违反「绝不超真实配额 / 绝不打满磁盘」。
//
// 口径与 `trueusage.go` 同源（`physBytesOf`：内容分块 EncSize + parity + **meta blob 落盘长度**），
// 区别是本文件提供**单对象**（含目录）与**上界**：
//   - `PhysFootprint` = 已落盘对象的实测（= `TrueSize`，索引内 `e.phys`）；
//   - `PhysBound` = 预占用上界（写入前不知随机分块/meta pad ⇒ 必须取上界，结算时按实测收敛）。
//
// 目录 meta：`dirs` 只记集合（无尺寸）⇒ 本文件新增 `dirPhys`（逻辑目录 → meta blob 落盘长度），
// 在写入与加载两处捕获；缺失（旧索引未跟踪）⇒ ok=false，调用方按上界处理并记日志。

const (
	// blockletOverhead 是每 blocklet 的固定密文开销（nonce + GCM tag）。
	blockletOverhead = int64(shardseal.NonceLen + 16)
	// blobFormatSlack 是单 blob 的格式余量（长度头/内嵌盐/魔数/命名等）的保守估计。
	blobFormatSlack = int64(4 << 10)
)

// ceilDiv 返回 ⌈a/b⌉（b>0；a<0 视为 0）。
func ceilDiv(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// blockMin / blockMax / blockletMax 返回有效分块参数（0/负值 ⇒ 生产默认值，与 NewFS 回落一致）。
func (s *SecretdataFS) blockParams() (blMin, blMax, bltMax int64) {
	blMin, blMax = s.opts.Block.Min, s.opts.Block.Max
	if blMin <= 0 {
		blMin = 1 << 20
	}
	if blMax <= 0 {
		blMax = 200 << 20
	}
	bltMax = s.opts.Block.BlockletMax
	if bltMax <= 0 {
		bltMax = 4 << 20
	}
	return blMin, blMax, bltMax
}

// metaPadBound 返回**单个 meta blob**（文件或目录）的落盘长度上界。
// pad 目标 = max(base + rand(base), 196) ∈ [base, 2·base) ⇒ 上界 = 2·base + 格式余量。
func (s *SecretdataFS) metaPadBound() int64 {
	base := int64(s.opts.MetaPadBytes)
	if base <= 0 {
		base = s.opts.Block.Min
	}
	if base <= 0 {
		base = 196
	}
	return 2*base + blobFormatSlack
}

// PhysFootprint 返回 rel 的**真实物理落盘字节**（内容分块 + parity + meta blob pad；实测）。
func (s *SecretdataFS) PhysFootprint(rel string) (int64, bool) {
	return s.TrueSize(rel)
}

// PhysBound 返回 logical 字节在本卷的**物理上界**（预占用用；结算按 `PhysFootprint` 实测收敛）。
//
// 上界推导（保守、宁大勿小——多预留部分在 Commit(实测) 时自动归还）：
//
//	内容密文 ≤ logical + blockletOverhead × blocklet 数上界
//	blocklet 数 ≤ ⌈logical/bltMax⌉ + ⌈logical/blMin⌉（块内定长 blocklet + 每块至少 1 个）
//	parity（Erasure 且 ≥2 分块）≤ blMax + blockletOverhead × ⌈blMax/bltMax⌉ + 格式余量
//	meta blob ≤ 2·base + 格式余量
func (s *SecretdataFS) PhysBound(logical int64) int64 {
	if logical < 0 {
		logical = 0
	}
	blMin, blMax, bltMax := s.blockParams()
	n := logical + blobFormatSlack + s.metaPadBound()
	if logical > 0 {
		blocklets := ceilDiv(logical, bltMax) + ceilDiv(logical, blMin)
		n += blockletOverhead * blocklets
		if s.opts.Erasure {
			n += blMax + blockletOverhead*ceilDiv(blMax, bltMax) + blobFormatSlack
		}
	}
	return n
}

// DirPhysFootprint 返回目录 rel 的 meta blob **落盘长度**（实测）。
//
// 实现：由 `dirSegs` 取该目录的随机容器名，列容器内条目并取 `KindDirMeta` 条目的 Size。
// 不在索引里另存尺寸——目录 meta 的落盘长度只有**写入那一刻**与**列目录**可见，而 rmdir 是
// 低频操作（每次一次列目录可接受），这样避免为每个目录常驻一份尺寸状态（加载路径零改动）。
func (s *SecretdataFS) DirPhysFootprint(ctx context.Context, rel string) (int64, bool) {
	key := path.Clean(strings.TrimPrefix(rel, "/"))
	if key == "." {
		key = ""
	}
	s.mu.RLock()
	container, ok := s.dirSegs[key]
	s.mu.RUnlock()
	if !ok || container == "" {
		return 0, false
	}
	entries, err := s.inner.ListDir(ctx, container)
	if err != nil {
		return 0, false
	}
	for _, e := range entries {
		if e.IsDir || shardseal.ClassifyName(e.Name) != shardseal.KindDirMeta {
			continue
		}
		if e.Size > 0 {
			return e.Size, true
		}
	}
	return 0, false
}

// DirPhysBound 返回目录 meta blob 的落盘长度上界（预占用用）。
func (s *SecretdataFS) DirPhysBound() int64 { return s.metaPadBound() }

// DirChainFootprint 返回 rel **及其全部祖先**（到卷根）已存在目录的 dir meta blob 落盘长度之和。
//
// 为什么需要「链」：`MakeDir` 会**递归创建整条祖先链**（每级一个随机容器 + 一个 dir meta blob，
// 默认 1–2MB）⇒ 单看叶子会漏记祖先。用「mkdir 前 chain − mkdir 后 chain」做差分即**精确**得到
// 本次新建目录的物理字节（无需知道 FS 内部创建了哪几级）；rmdir 同理（pre − post）。
//
// 不存在的层级贡献 0（`DirPhysFootprint` ok=false）⇒ 差分天然只统计**新增/移除**的层级。
// 恒返回 ok=true（本层支持目录足迹；调用方按能力装配与否决定是否使用）。
func (s *SecretdataFS) DirChainFootprint(ctx context.Context, rel string) (int64, bool) {
	key := path.Clean(strings.TrimPrefix(rel, "/"))
	if key == "." {
		key = ""
	}
	var total int64
	// 从卷根逐级累加（根容器也有 dir meta blob）。
	for _, p := range dirChain(key) {
		if n, ok := s.DirPhysFootprint(ctx, p); ok && n > 0 {
			total += n
		}
	}
	return total, true
}

// dirChain 返回 key 及其祖先（含空串根，从根到叶子顺序）。
func dirChain(key string) []string {
	if key == "" {
		return []string{""}
	}
	segs := strings.Split(key, "/")
	out := make([]string, 0, len(segs)+1)
	out = append(out, "")
	for i := range segs {
		out = append(out, strings.Join(segs[:i+1], "/"))
	}
	return out
}
