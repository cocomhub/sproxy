// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// verifyExpectedChecksum 在**转存之前**校验产物 checksum 是否等于期望值（2026-11-21）。
//
// 为什么（成功率关键）：修复场景要求"重下内容必须与原内容逐字节一致"，否则修完仍是坏文件，
// 而既有完整性管道只判断"下载器自报 vs 语义校验"，无法表达调用方的**期望值**。
// 空 `ExpectedChecksum` = 不额外校验（零回归）。
// 语义（2026-11-24 修正）：调用方给了期望值 **就必须校验**——
// 下载器未提供 checksum 时**本地补算**（sha256 流式读 destPath），仍拿不到则判失败（fail-closed）。
// 早期实现"无 checksum 即跳过"是 fail-open：实测用错误期望值提交的探针任务被误判 completed ✗。
func (m *CloudDownloadManager) verifyExpectedChecksum(task *CloudTask, result *downloaderResultView, destPath string) error {
	if task == nil {
		return nil
	}
	m.mu.RLock()
	want := task.ExpectedChecksum
	m.mu.RUnlock()
	if want == "" {
		return nil // 未指定期望值：沿用既有完整性管道（零回归）
	}
	got := ""
	if result != nil {
		got = result.Checksum
	}
	if got == "" && destPath != "" {
		sum, err := sha256FileHex(destPath)
		if err != nil {
			return fmt.Errorf("内容校验失败：无法计算产物 checksum（期望 %s）: %w", want, err)
		}
		got = sum
	}
	if got == "" {
		return fmt.Errorf("内容校验失败：期望 %s，但产物 checksum 不可得（拒绝放行）", want)
	}
	if got == want {
		return nil
	}
	return fmt.Errorf("内容校验失败：期望 checksum %s，实际 %s（将按 content_mismatch 换源重试）", want, got)
}

// rotateSourceLocked 在重试前轮换来源（调用方持 m.mu）。
//
// 触发条件（设计 §3.4）：上一次失败归类属于**源类问题**（unreachable/throttled）或内容不一致，
// 且还有备选源。轮换后把 `SourceIndex` 前移并写回 URL；`Diagnostics.Attempts` 归零（新源重新计数），
// 但保留 `LastErrorClass` 供观测。
//
// 返回 true 表示发生了轮换（调用方应持久化任务后再 resume）。
func (t *CloudTask) rotateSourceLocked() bool {
	if len(t.FallbackSources) == 0 || t.Diagnostics == nil {
		return false
	}
	switch t.Diagnostics.LastErrorClass {
	case ErrClassSourceUnreachable, ErrClassThrottled, ErrClassContentMismatch:
	default:
		return false
	}
	next := t.SourceIndex + 1
	if next >= len(t.FallbackSources) {
		return false
	}
	t.SourceIndex = next
	t.URL = t.FallbackSources[next]
	t.Diagnostics.Attempts = 0
	return true
}

// downloaderResultView 是校验所需的最小视图（避免本文件依赖完整 downloader.Result 结构）。
type downloaderResultView struct {
	Checksum string
}

// resultViewOf 把下载器结果映射为校验视图（nil 安全）。
func resultViewOf(r *downloader.Result) *downloaderResultView {
	if r == nil {
		return nil
	}
	return &downloaderResultView{Checksum: r.Checksum}
}

// sha256FileHex 流式计算文件 sha256（不整文件入内存）。
func sha256FileHex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, cerr := io.Copy(h, f); cerr != nil {
		return "", cerr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// forceRotateSourceLocked 强制轮换到下一个备选源（**人工**动作，不看失败归类）。
// 与 rotateSourceLocked 的区别：后者只对源类失败自动触发；本函数由用户显式选择「换源重试」。
// 返回是否发生轮换（调用方持 m.mu）。
func (t *CloudTask) forceRotateSourceLocked() bool {
	next := t.SourceIndex + 1
	if next >= len(t.FallbackSources) {
		return false
	}
	t.SourceIndex = next
	t.URL = t.FallbackSources[next]
	if t.Diagnostics != nil {
		t.Diagnostics.Attempts = 0
	}
	return true
}
