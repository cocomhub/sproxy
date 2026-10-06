// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity

import (
	//nolint:gosec // G505: GCID 是 PikPak 官方 hash 算法（sha1(concat(sha1(分块)))，算法定义
	// 必须用 SHA-1，非安全用途——复算权威 hash 需与官方一致，不可替换。
	"crypto/sha1"
	"encoding/hex"
	"io"
	"os"
)

// GCIDCandidates 是 PikPak GCID 复算的候选分块集合（字节），供外部（如 pikpak 下载器）
// 直接复用：256KB / 512KB / 1MB / 2MB / 4MB。
var GCIDCandidates = []int64{262144, 524288, 1048576, 2097152, 4194304}

// gcidDefaultCandidates 是 ComputeGCID 单分块模式下的默认分块大小（blockSize<=0 时用
// 最大候选 4MB——单分块复算语义，供外部已知分块的调用方）。
var gcidDefaultCandidates = GCIDCandidates

// ComputeGCID 计算 PikPak GCID：sha1(concat(sha1(每个 blockSize 分块)))。
// blockSize <= 0 时退回 gcidDefaultCandidates（取最大候选）；空数据返回空串（调用方
// 判 ok 前先验 size）。
func ComputeGCID(data []byte, blockSize int64) string {
	if blockSize <= 0 {
		blockSize = gcidDefaultCandidates[len(gcidDefaultCandidates)-1]
	}
	if len(data) == 0 {
		return ""
	}
	h := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
	for off := int64(0); off < int64(len(data)); off += blockSize {
		end := off + blockSize
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		chunkHash := sha1.Sum(data[off:end]) //nolint:gosec // G401: GCID 算法必须 sha1
		h.Write(chunkHash[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RecomputeGCID 对内存数据尝试候选分块集合复算 GCID：返回**首个整除候选**的命中值
// + ok（兼容测试/外部调用；文件流式用 RecomputeGCIDAll 取全候选）。
// RecomputeGCID 对内存数据尝试候选分块集合复算 GCID：返回**首个整除候选**的命中值
// + ok（兼容测试/外部调用；文件流式用 RecomputeGCIDAll 取全候选）。
// 注意：此处保留整除限制是「候选预筛」（与 RecomputeGCIDAll 尾块语义的差异）——调用方
// 应优先用 RecomputeGCIDAll（文件流式，尾块正确计入）。本函数为测试基准/小数据入口。
func RecomputeGCID(data []byte, candidates []int64) (string, bool) {
	for _, bs := range candidates {
		if bs <= 0 || len(data) == 0 || int64(len(data))%bs != 0 {
			continue
		}
		if g, ok := indexedBlockGCID(data, bs); ok {
			return g, true
		}
	}
	return "", false
}

// indexedBlockGCID 按固定分块复算 GCID（测试内部基准用，块大小校验）。
func indexedBlockGCID(data []byte, blockSize int64) (string, bool) {
	if blockSize <= 0 || len(data) == 0 {
		return "", false
	}
	h := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
	for off := int64(0); off < int64(len(data)); off += blockSize {
		end := off + blockSize
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		chunkHash := sha1.Sum(data[off:end]) //nolint:gosec // G401: GCID 算法必须 sha1
		h.Write(chunkHash[:])
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// RecomputeGCIDFile 对文件流式复算 GCID（避免整文件读入内存，适配多 GB 视频）：
// 返回第一个整除候选的 GCID（兼容旧签名）+ ok；无候选整除 → ok=false。
func RecomputeGCIDFile(path string, candidates []int64) (string, bool, error) {
	all, err := RecomputeGCIDAll(path, candidates)
	if err != nil {
		return "", false, err
	}
	if len(all) == 0 {
		return "", false, nil
	}
	return all[0], true, nil
}

// RecomputeGCIDAll 返回**全部整除候选**的 GCID 值（R3-I1：官方分块粒度未知，调用方
// 逐一与官方 hash 比对，任一命中即权威——提升命中率，非首整除即返）。
// I/O 成本说明（对抗评审 R3-Minor + R2 尾块修正）：**每个候选都需全文件扫描一次**——
// 尾块修复后不再要求整除（真实文件几乎不可能整除，整除跳过使权威匹配恒失效），故
// 对**每个文件**恒 5× 全读（不再依赖「size 同时整除全部候选」）。PikPak 官方分块粒度
// 未实证（大文件非固定 256KB），全候选是正确性优先（小候选不命中不代表大候选不命中）；
// 视频下载后核对属低频路径，5×GB I/O 相对下载带宽可接受。需要严格省 I/O 时可改为
// 「逐候选算一比对一、命中即返」。
func RecomputeGCIDAll(path string, candidates []int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	var out []string
	for _, bs := range candidates {
		if bs <= 0 {
			continue
		}
		// 尾块语义与内存版 ComputeGCID 对齐（end=min，余量尾块参与计算）——不再要求
		// size%bs==0（真实文件尺寸几乎不可能恰为候选整数倍，整除限制使权威复算对多数
		// 文件失效，恒回落 ModeLocalOnly，权威匹配形同虚设）。官方 GCID 对小文件按
		// 256KB 分块，余量尾块必须计入。
		outer := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
		block := make([]byte, bs)
		inner := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
		for off := int64(0); off < size; off += bs {
			end := off + bs
			if end > size {
				end = size
			}
			n, rerr := f.ReadAt(block[:end-off], off)
			if rerr != nil && rerr != io.EOF {
				return nil, rerr
			}
			// R7-F1：短读（Stat 与 Read 间被并发截断/IO 异常）时只写实际读到的 n 字节
			// （block 尾部残留旧数据/零填充会产出脏 GCID——命中率极低但属正确性隐患）。
			// 尾块（EOF）也是合法输入：n 即尾块实际长度，参与计算（不再有"EOF 后继续
			// 空块累积 sha1('')"的问题——循环按 off<size 推进，尾块即最后一块）。
			inner.Reset()
			inner.Write(block[:n])
			outer.Write(inner.Sum(nil))
		}
		out = append(out, hex.EncodeToString(outer.Sum(nil)))
	}
	return out, nil
}
