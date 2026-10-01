// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"crypto/rand"
	"fmt"
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

// randomChunkSize 在 [min, max] 间均匀随机，且不超过 remain；剩余不足 min 时取 remain。
func randomChunkSize(min, max, remain int64) int64 {
	if remain <= min {
		return remain
	}
	hi := max
	if remain < hi {
		hi = remain
	}
	return min + cryptoRandN(hi-min+1)
}
