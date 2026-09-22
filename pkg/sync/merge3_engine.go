// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

// syncFileMerge3 三方合并路径：以 tmpPath（旧目标，同步前内容）为 base、src 为 ours
// 做 diff3 合并，结果写 dstPath。theirs 在本仓 FS 模型下 = base（目标已被改名移走，
// 无独立当前版本；合并语义 = 源与目标旧版本的差异自动融合）。
//
// 返回 true = merge3 完成（含冲突标记文件）；false = 不适用（非文本/读失败）→
// 调用方回退整文件复制。
func (e *Engine) syncFileMerge3(ctx context.Context, src, dst FS, dstPath, tmpPath, srcPath string, srcE *Entry, rec func(FileResult)) bool {
	// 读 base（旧目标 tmpPath）。
	baseR, err := dst.OpenRead(ctx, tmpPath)
	if err != nil {
		return false
	}
	base, berr := io.ReadAll(baseR)
	_ = baseR.Close()
	if berr != nil {
		return false
	}
	// 读 ours（源）。
	oursR, err := src.OpenRead(ctx, srcPath)
	if err != nil {
		return false
	}
	ours, oerr := io.ReadAll(oursR)
	_ = oursR.Close()
	if oerr != nil {
		return false
	}
	// 非文本（含 NUL 或非法 UTF-8）→ 不合并，回退整文件复制（conflict_rename 语义由调用方回退）。
	if !isMerge3Textable(base) || !isMerge3Textable(ours) {
		return false
	}

	merged, conflicted, hunks := Merge3(base, ours, base)
	e.logger().Info("M3-DBG", "conflicted", conflicted, "base", string(base), "ours", string(ours), "dst", dstPath)
	if err := dst.WriteFile(ctx, dstPath, bytes.NewReader(merged), int64(len(merged)), srcE.MTime); err != nil {
		e.logger().Warn("三方合并写目标失败，回退整文件复制", "path", dstPath, "error", err)
		return false
	}
	action := ActionUpdated
	if conflicted {
		action = "merged_conflict"
		// 登记冲突索引（装配层注入 ConflictRecorder 时）。
		if e.ConflictRecorder != nil {
			e.ConflictRecorder(ConflictRecord{
				Path:      dstPath,
				HunkCount: len(hunks),
				BaseSHA:   conflictSHA256Hex(base),
				OursSHA:   conflictSHA256Hex(ours),
				TheirsSHA: conflictSHA256Hex(base),
				Ours:      flattenMerge3Hunks(hunks, true),
				Theirs:    flattenMerge3Hunks(hunks, false),
				Timestamp: time.Now().UnixNano(),
			})
		}
		e.logger().Info("三方合并冲突", "path", dstPath, "hunks", len(hunks))
	}
	rec(FileResult{Path: dstPath, Action: action, Size: int64(len(merged)), MTime: srcE.MTime, Checksum: srcE.Checksum})
	return true
}

// isMerge3Textable 文本探测：无 NUL 字节且为合法 UTF-8（空内容视为文本）。
// 用 utf8.Valid 判定（bytes.ValidUTF8 是 Go 1.27 新 API，保守用标准 utf8 包）。
func isMerge3Textable(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	return utf8.Valid(b)
}

// flattenMerge3Hunks 提取冲突段的 ours 或 theirs 行（索引展示用）。
func flattenMerge3Hunks(hunks []ConflictHunk, ours bool) []string {
	var out []string
	for _, h := range hunks {
		if ours {
			out = append(out, h.Ours...)
		} else {
			out = append(out, h.Theirs...)
		}
	}
	return out
}

// sha256Hex 计算 SHA-256 hex（冲突记录用）。
func conflictSHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var _ = fmt.Sprintf // 保留 fmt 导入（后续索引调试日志用）
