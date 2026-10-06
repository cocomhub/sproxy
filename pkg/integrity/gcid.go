// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity

import (
	//nolint:gosec // G505: GCID 是 PikPak 官方 hash 算法（sha1(concat(sha1(分块)))，算法定义必须
	// 用 SHA-1，非安全用途——复算权威 hash 需与官方一致，不可替换。
	"crypto/sha1"
	"encoding/hex"
	"io"
	"os"
)

// GCIDCandidates 是 PikPak GCID 复算的候选分块集合（字节），供外部（如 pikpak 下载器）
// 直接复用：256KB / 512KB / 1MB / 2MB / 4MB。
var GCIDCandidates = []int64{262144, 524288, 1048576, 2097152, 4194304}

// gcidDefaultCandidates 是 ComputeGCID 的默认分块（内部，向后兼容）。
var gcidDefaultCandidates = GCIDCandidates

// ComputeGCID 计算 PikPak GCID：sha1(concat(sha1(每个 blockSize 分块)))。
// blockSize <= 0 时退回 gcidDefaultCandidates；空数据返回空串（调用方判 ok 前先验 size）。
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
		chunkHash := sha1.Sum(data[off:end])
		h.Write(chunkHash[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// indexedBlockGCID 按候选分块复算 GCID 并校验位数（内部实现，测试用 gcidCompute 独立基准）。
func indexedBlockGCID(data []byte, blockSize int64) (string, bool) {
	if blockSize <= 0 || len(data) == 0 {
		return "", false
	}
	// 余量判定：分块必须整除文件长度（无尾块）。PikPak 大文件分块随上传变化，
	// 候选仅在整除时才有机会命中官方 hash（Review Focus 5：非整除即未命中回落 ② 态）。
	if int64(len(data))%blockSize != 0 {
		return "", false
	}
	return ComputeGCID(data, blockSize), true
}

// RecomputeGCID 在候选分块集合上复算 GCID，逐一与官方权威（无已知值，仅返回首个整除
// 候选的复算值 + ok=true）；调用方用返回值与官方 hash（FileMeta.Hash）比对决定是否权威命中。
// 设计（spec §6）：候选集合自适应，未命中（无候选整除）→ ok=false，回落 ② 态本地自洽。
func RecomputeGCID(data []byte, candidates []int64) (string, bool) {
	for _, bs := range candidates {
		if gcid, ok := indexedBlockGCID(data, bs); ok {
			return gcid, true
		}
	}
	return "", false
}

// RecomputeGCIDFile 对文件流式复算 GCID（避免整文件读入内存，适配多 GB 视频）：
// 首个能整除文件大小的候选即采用（与 RecomputeGCID 语义对齐），返回其 GCID + ok=false
// 为无候选整除。ReadAt 随机读（与候选无关），无需重置偏移。空文件/无候选 → ok=false。
func RecomputeGCIDFile(path string, candidates []int64) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	size := fi.Size()
	if size == 0 {
		return "", false, nil
	}
	for _, bs := range candidates {
		if bs <= 0 || size%bs != 0 {
			continue
		}
		outer := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
		block := make([]byte, bs)
		inner := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
		for off := int64(0); off < size; off += bs {
			if _, rerr := f.ReadAt(block, off); rerr != nil && rerr != io.EOF {
				return "", false, rerr
			}
			inner.Reset()
			inner.Write(block)
			outer.Write(inner.Sum(nil))
		}
		return hex.EncodeToString(outer.Sum(nil)), true, nil
	}
	return "", false, nil
}
