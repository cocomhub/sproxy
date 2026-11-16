// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// 本文件实现「收敛加密（确定性密文）」的随机源（设计
// docs/designs/2026-11-16-deterministic-blob-convergent-encryption.md §3/§10）。
//
// v1（AlgoV1GCM*）保持 crypto/rand 随机（语义**不变**）；v2（AlgoV2GCM）把一次加密里
// 所有「随机」字节改为按 (卷密钥, 明文内容标识 CID, 标签) **确定性派生** ⇒ 同一明文在同一
// 卷上两次加密产出逐字节相同的密文 ⇒ 相同 blob 名 ⇒ 网盘秒传可命中 / 跨路径去重。
//
// 派生必须绑定**卷密钥**（§10）：CID 相同而卷密钥不同 ⇒ 结果必不同（跨卷不可关联）。
// CID = SHA-256(明文整文件) 只在派生输入中使用，**绝不落盘/外传**（§10.3）。

// sealSource 提供一次加密所需的「随机」字节来源。
//   - Bytes：n 字节（v1 = crypto/rand；v2 = 确定性派生）；
//   - N：    [0,max) 的 int64（块长规划 / 段间间隙用）。
//
// 标签（label）编码用途与序号（如 "blk3/b5"、"meta/hdr"），**绝不包含文件名/路径/mtime**
// （设计 §3.1：派生源只用内容标识与序号）。
type sealSource interface {
	Bytes(label string, n int) ([]byte, error)
	N(label string, max int64) int64
}

// cryptoSealSource 是 v1 随机源（crypto/rand，与历史行为同分布；label 忽略）。
type cryptoSealSource struct{}

func (cryptoSealSource) Bytes(_ string, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("shardseal: 随机字节失败: %w", err)
	}
	return b, nil
}

func (cryptoSealSource) N(_ string, max int64) int64 { return cryptoRandN(max) }

// convSealSource 是 v2 收敛随机源：按 (卷密钥, CID, 标签) 确定性派生。
type convSealSource struct {
	secret []byte // 卷密钥（HMAC key；每卷独立 ⇒ 跨卷结果不同）
	cid    []byte // 明文内容标识（SHA-256(明文)），仅作派生输入、不落盘
}

func (d convSealSource) Bytes(label string, n int) ([]byte, error) {
	if n == 0 {
		return nil, nil // 空请求（如零长间隙/填充）无需派生
	}
	return deriveBytesN(d.secret, label, d.cid, n)
}

func (d convSealSource) N(label string, max int64) int64 {
	if max <= 1 {
		return 0
	}
	b, err := d.Bytes(label, 8)
	if err != nil {
		return 0 // 派生失败属环境级错误（空密钥在 sealSourceFor 已 fail-closed），回落 0 让上游错误路径可见
	}
	return int64(binary.BigEndian.Uint64(b) % uint64(max))
}

// sealSourceFor 返回该算法版本所用的随机源；第二个返回值报告是否为**确定性**（v2）。
// 空密钥在 convergent 分支 fail-closed（convSealSource 的 DeriveBytes 会拒绝空密钥），
// 杜绝「退化为跨卷可关联的常量派生」。
func sealSourceFor(v AlgoVersion, secret, cid []byte) (sealSource, bool) {
	if a, ok := AlgoByVersion(v); ok && a.Convergent {
		return convSealSource{secret: secret, cid: cid}, true
	}
	return cryptoSealSource{}, false
}

// deriveBytesN 组合既有 DeriveBytes（HMAC-SHA256，单次输出上限 32B）生成 n 字节：
// 第 i 个 32B 块 = DeriveBytes(secret, label||"#"||i, content, 32)。复用同一 HMAC 原语，
// 不另立派生函数（设计 §3/§10）。
func deriveBytesN(secret []byte, label string, content []byte, n int) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("shardseal: 派生长度非法: %d", n)
	}
	out := make([]byte, 0, n+sha256.Size)
	for i := 0; len(out) < n; i++ {
		chunk, err := DeriveBytes(secret, fmt.Sprintf("%s#%d", label, i), content, sha256.Size)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	return out[:n], nil
}

// convBlockSize 返回第 idx 块的确定性长度（设计 §3.2：blockLen = min + derive(...) mod
// (max-min+1)，保留「变长」但去掉随机）；末块收尾语义与 randomChunkSize 一致。
func convBlockSize(ss sealSource, idx, minSize, maxSize, remain int64) int64 {
	if remain <= minSize {
		return remain
	}
	hi := min(remain, maxSize)
	span := hi - minSize + 1
	if span <= 1 {
		return minSize
	}
	return minSize + ss.N(fmt.Sprintf("blockplan/%d", idx), span)
}

// injectConvBlockSizes 若规划器是 RandomPlanner 则注入确定性块长来源（v2）；其它规划器
// （fixed/video-keyframe）本就确定，无需注入。
func injectConvBlockSizes(planner BlockPlanner, ss sealSource) {
	rp, ok := planner.(*RandomPlanner)
	if !ok {
		return
	}
	mn, mx := rp.Min, rp.Max
	rp.SizeAt = func(idx, remain int64) int64 { return convBlockSize(ss, idx, mn, mx, remain) }
}
