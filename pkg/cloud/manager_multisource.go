// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"fmt"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// verifyExpectedChecksum 在**转存之前**校验产物 checksum 是否等于期望值（2026-11-21）。
//
// 为什么（成功率关键）：修复场景要求"重下内容必须与原内容逐字节一致"，否则修完仍是坏文件，
// 而既有完整性管道只判断"下载器自报 vs 语义校验"，无法表达调用方的**期望值**。
// 空 `ExpectedChecksum` = 不额外校验（零回归）。
func (m *CloudDownloadManager) verifyExpectedChecksum(task *CloudTask, result *downloaderResultView) error {
	if task == nil || result == nil {
		return nil
	}
	m.mu.RLock()
	want := task.ExpectedChecksum
	m.mu.RUnlock()
	if want == "" || result.Checksum == "" {
		return nil
	}
	if result.Checksum == want {
		return nil
	}
	return fmt.Errorf("内容校验失败：期望 checksum %s，实际 %s（将按 content_mismatch 换源重试）",
		want, result.Checksum)
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
