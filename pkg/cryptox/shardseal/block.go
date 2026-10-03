// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
)

// cryptoRandN 返回 [0, n) 的均匀随机 int64（crypto/rand 无穷熵，拒绝采样消除偏差）。
func cryptoRandN(n int64) int64 {
	bi, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		// crypto/rand 失败是致命环境问题：回落 min（确定性）让上游错误路径可见，
		// 不静默吞错。
		return 0
	}
	return bi.Int64()
}

// BlockPlanner 是可插拔分块规划接口：把原始文件大小切成不定长的分块序列。
// 默认实现是 RandomPlanner（1MB-200MB 随机）；可扩展 video-keyframe 等
// 按内容语义分片（plugin，见设计 §7）。
type BlockPlanner interface {
	// Plan 按 origSize（字节）返回分块序列。分块 Offset/Size 覆盖 [0, origSize)，
	// 连续不重叠，最后一块为剩余。
	Plan(origSize int64) ([]Block, error)
}

// Block 是一个分块的偏移与大小描述。
type Block struct {
	Offset int64
	Size   int64
}

// RandomPlanner 是默认随机分块规划器：在 [Min, Max] 之间随机取块长，最后一块为剩余。
//
// 注意（设计 §2.2/§4.1）：块大小随机使「每文件分块序列各不相同」，避免同源文件
// 分块结构被观察者依固定边界对齐 → 分块边界不同不泄露原始大小规律。Min==Max
// 时退化为定长块（确定性，便于测试）。
type RandomPlanner struct {
	Min int64
	Max int64
}

// Plan 实现 BlockPlanner。校验 Min/Max > 0 且 Min ≤ Max。
//
// 空文件（origSize ≤ 0）返回错误——**有意取舍（M-2）**：分块存储要求至少 1 个数据
// 分块承载 meta 引用（元数据 + 内容哈希锚定三段 hex 命名），空文件无内容可分块、也无法
// 锚定整文件 SHA-256。对透明文件卷而言空文本文件是常规输入却被拒，属已知边界（已在
// 测试与文档标注，见 block_test.go 与设计文档）；上层 secretdata writeFile 对空文件会
// 经本错误 fail-closed，不静默写半态。
func (p *RandomPlanner) Plan(origSize int64) ([]Block, error) {
	if p.Min <= 0 || p.Max < p.Min {
		return nil, fmt.Errorf("shardseal: 非法分块区间 Min=%d Max=%d（需 0<Min≤Max）", p.Min, p.Max)
	}
	if origSize <= 0 {
		return nil, fmt.Errorf("shardseal: 空文件不可分块（size=%d）", origSize)
	}
	var blocks []Block
	var off int64
	for off < origSize {
		remain := origSize - off
		// 剩余不足最小块 → 末块收尾；否则在 [Min, max(Min, min(Max, remain))] 随机。
		size := randomChunkSize(p.Min, p.Max, remain)
		blocks = append(blocks, Block{Offset: off, Size: size})
		off += size
	}
	return blocks, nil
}

// randomChunkSize 在 [minSize, maxSize] 间均匀随机，且不超过 remain；剩余不足 minSize 时取 remain。
// 形参命名避开预声明的 buildin min/max（Sonar go:S978 遮蔽告警）。
func randomChunkSize(minSize, maxSize, remain int64) int64 {
	if remain <= minSize {
		return remain
	}
	hi := min(remain, maxSize)
	return minSize + cryptoRandN(hi-minSize+1)
}

// BlockletPlanner 规划单个块的 blocklet 序列（关键帧边界 / 固定大小）。
// 视频关键帧随机访问需要「只下载/解密含目标范围的 blocklet 段」，故单块细分为可独立
// 解密的 blocklet（各 blocklet 自描述 offset/len/nonce，见 crypto.go）。
type BlockletPlanner interface {
	// PlanBlocklets 按 data（整文件随机访问）、origSize（整文件大小）、blockOffset（块
	// 在文件中的偏移）、blockSize（块大小）返回 blocklet 序列。data 供 video-keyframe
	// 按内容（GOP/关键帧边界）规划；fixed mode 忽略 data（传 _）。Blocklet.Offset 为块
	// 内 blocklet 在原始文件中的绝对偏移；序列连续覆盖 [blockOffset, blockOffset+blockSize)，
	// 末段为剩余。
	PlanBlocklets(data io.ReaderAt, origSize, blockOffset, blockSize int64) ([]Blocklet, error)
}

// Blocklet 是块内子块描述。
type Blocklet struct {
	Offset int64 // blocklet 在原始文件中的字节偏移（绝对）
	Size   int64 // blocklet 原始明文大小
	// Padding 标记「空闲/未使用」的 blocklet 段（打包替换场景：文件 A 5MB→B 4MB 时余出
	// 的段标记 padding 供后续小内容复用，无需重加密整文件）。加密时 padding 段用随机字节
	// 填充（仍在密文内、GCM 认证）；meta 以 BlockletInfo.Used=false 记录。fixed 规划当前
	// 不产出 padding 段，字段为格式预留。
	Padding bool
	// Type 是可选的显式段类型（0=按 Padding 推断：Data/Padding；非 0 时优先，如 Extra）。
	Type BlockletType
	// Data 是段内容（nil=取自块内 data[Offset-blockOffset : ...]；非 nil=段明文本体按此
	// 提供，供错误段/附加段等自描述段使用。Size 恒为段明文长，Data 长度须 == Size）。
	Data []byte
}

// FixedBlockletPlanner 是默认定长 blocklet 规划器（BlockletMode "fixed"）：把单块切成
// 宽度 ≤BloffsetMax 的连续 blocklet，末块收尾（剩余 <BlockletMin 时收尾块允许更小）。
// Min/Max 可继承 BlockPolicy 默认（64KB-4MB）；对小块测试可缩小。fixed 模式忽略 data。
type FixedBlockletPlanner struct {
	Min int64
	Max int64
}

// PlanBlocklets 实现 BlockletPlanner。data 忽略（fixed 不按内容规划）。校验 Min/Max>0 且
// Min≤Max、块尺寸>0、块不越出文件。
func (p *FixedBlockletPlanner) PlanBlocklets(data io.ReaderAt, origSize, blockOffset, blockSize int64) ([]Blocklet, error) {
	_ = data // fixed 模式不按内容规划（video-keyframe 预留参数）
	if p.Min <= 0 || p.Max < p.Min {
		return nil, fmt.Errorf("shardseal: 非法 blocklet 区间 Min=%d Max=%d（需 0<Min≤Max）", p.Min, p.Max)
	}
	if blockSize <= 0 {
		return nil, fmt.Errorf("shardseal: 空块不可细分 blocklet（size=%d）", blockSize)
	}
	if blockOffset < 0 || origSize < blockOffset+blockSize {
		return nil, fmt.Errorf("shardseal: 块 [%d,%d) 越出文件 [0,%d)", blockOffset, blockOffset+blockSize, origSize)
	}
	var out []Blocklet
	for off := int64(0); off < blockSize; {
		remain := blockSize - off
		size := min(remain,
			// 剩余 <Max → 末块收尾
			p.Max)
		// 剩余 ≥Min 时 size 恒 ≥Min（Max≥Min 且 size=min(Max,remain)≥Min）；剩余<Min 时
		// size=remain（收尾块，允许 <Min）。
		out = append(out, Blocklet{Offset: blockOffset + off, Size: size})
		off += size
	}
	return out, nil
}
