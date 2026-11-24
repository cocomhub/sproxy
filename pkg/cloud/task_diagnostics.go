// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"strings"
	"time"
)

// 错误类别（稳定字符串，供 UI/指标/重试策略分流）。
const (
	// ErrClassSourceUnreachable 源不可达/超时（可退避重试）。
	ErrClassSourceUnreachable = "source_unreachable"
	// ErrClassThrottled 源限流/触发风控（可退避重试，建议更长间隔）。
	ErrClassThrottled = "throttled"
	// ErrClassContentMismatch 内容不匹配（checksum/完整性校验失败）⇒ 应**换源**重试。
	ErrClassContentMismatch = "content_mismatch"
	// ErrClassTargetConflict 目标已存在且内容不同 ⇒ 需人工/显式覆盖，不自动重试。
	ErrClassTargetConflict = "target_conflict"
	// ErrClassTargetUnavailable 目标卷/后端**暂不可用**（未装配、卷不存在、后端不可达）——
	// **可重试**：装配顺序（cloud 任务恢复早于封装卷 attach）与后端抖动都会自愈。
	// 2026-11-24 实测：重启后任务恢复时转存报 `目标卷 "baidu" 未装配`，被归 unknown ⇒ 不重试 ✗。
	ErrClassTargetUnavailable = "target_unavailable"
	// ErrClassDiskFull 空间不足 ⇒ 需人工处理，不自动重试。
	ErrClassDiskFull = "disk_full"
	// ErrClassAuth 认证/权限问题 ⇒ 需人工处理，不自动重试。
	ErrClassAuth = "auth"
	// ErrClassUnknown 未归类。
	ErrClassUnknown = "unknown"
)

// TaskDiagnostics 是任务的**持久化诊断快照**（2026-11-21）。
//
// 为什么需要：此前失败任务只留 `error` 字符串，且失败 1h 后任务被清理 ⇒ 用户"看不到任务、
// 看不出下到哪、也无从重试"。该结构随任务持久化，使失败后仍可回答：
// 现在在哪个阶段、试了几次、失败原因属于哪一类（决定是否可自动重试）、部分产物在哪、多大。
type TaskDiagnostics struct {
	// Stage 是最后所处阶段：resolving | downloading | verifying | transferring。
	Stage string `json:"stage,omitempty"`
	// Attempts 是**已记录失败的次数**（每次失败递增；0 = 尚未失败过）。
	Attempts int `json:"attempts,omitempty"`
	// LastErrorClass 是最近一次失败的**归类**（见 ErrClass*），决定重试策略。
	LastErrorClass string `json:"last_error_class,omitempty"`
	// LastErrorAt 是最近一次失败时间。
	LastErrorAt time.Time `json:"last_error_at"`
	// LastError 是最近一次失败原文（截断，供人工判断）。
	LastError string `json:"last_error,omitempty"`
	// PartialSize 是失败时 cloud 桶里保留的已下载字节（续传基础；0 = 无部分产物）。
	PartialSize int64 `json:"partial_size,omitempty"`
	// PartialPath 是部分产物相对路径（cloud 桶内；空 = 无）。
	PartialPath string `json:"partial_path,omitempty"`
}

// maxDiagErrorLen 是 LastError 的截断长度（避免任务 JSON 膨胀）。
const maxDiagErrorLen = 512

// classifyCloudError 按错误文本归类（稳定、可测；不依赖具体错误类型以免跨层耦合）。
//
// 归类决定重试策略：source_unreachable/throttled 可退避重试；content_mismatch 换源；
// target_conflict/disk_full/auth 不自动重试（转人工）。
func classifyCloudError(msg string) string {
	s := strings.ToLower(msg)
	switch {
	case s == "":
		return ErrClassUnknown
	case strings.Contains(s, "no space") || strings.Contains(s, "storage full") ||
		strings.Contains(s, "insufficient") || strings.Contains(s, "507"):
		return ErrClassDiskFull
	case strings.Contains(s, "401") || strings.Contains(s, "403") || strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "forbidden") || strings.Contains(s, "auth"):
		return ErrClassAuth
	case strings.Contains(s, "checksum") || strings.Contains(s, "integrity") || strings.Contains(s, "damaged"):
		return ErrClassContentMismatch
	case strings.Contains(s, "未装配") || strings.Contains(s, "not assembled") ||
		strings.Contains(s, "not attached") || strings.Contains(s, "volume not found") ||
		strings.Contains(s, "卷不存在") || strings.Contains(s, "backend unavailable"):
		return ErrClassTargetUnavailable
	case strings.Contains(s, "exists") || strings.Contains(s, "conflict") || strings.Contains(s, "overwrite"):
		return ErrClassTargetConflict
	case strings.Contains(s, "429") || strings.Contains(s, "throttl") || strings.Contains(s, "rate limit") ||
		strings.Contains(s, "too many") || strings.Contains(s, "31034"):
		return ErrClassThrottled
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") || strings.Contains(s, "connection") ||
		strings.Contains(s, "unreachable") || strings.Contains(s, "no such host") || strings.Contains(s, "eof") ||
		strings.Contains(s, "502") || strings.Contains(s, "503") || strings.Contains(s, "504"):
		return ErrClassSourceUnreachable
	default:
		return ErrClassUnknown
	}
}

// diag 返回任务诊断对象（惰性创建）。调用方须持有 m.mu（或确认无并发）。
func (t *CloudTask) diag() *TaskDiagnostics {
	if t.Diagnostics == nil {
		t.Diagnostics = &TaskDiagnostics{}
	}
	return t.Diagnostics
}

// noteStage 记录阶段（幂等，不覆盖已有更细信息）。调用方须持有 m.mu。
func (t *CloudTask) noteStage(stage string) {
	t.diag().Stage = stage
}

// noteFailure 记录一次失败：归类 + 时间 + 原文 + 部分产物大小。调用方须持有 m.mu。
func (t *CloudTask) noteFailure(errMsg string, partialSize int64, partialPath string) {
	d := t.diag()
	d.Attempts++
	d.LastErrorClass = classifyCloudError(errMsg)
	d.LastErrorAt = time.Now()
	if len(errMsg) > maxDiagErrorLen {
		errMsg = errMsg[:maxDiagErrorLen]
	}
	d.LastError = errMsg
	if partialSize > 0 {
		d.PartialSize = partialSize
	}
	if partialPath != "" {
		d.PartialPath = partialPath
	}
}

// partialRelPath 返回任务部分产物的相对路径（cloud 桶内 `<owner>/<taskID>` 语义由调用方拼接，
// 这里只给稳定的任务目录标识；无部分产物时为空）。
func (t *CloudTask) partialRelPath() string {
	if t.ID == "" {
		return ""
	}
	return t.ID
}

// Diag 返回诊断快照（只读；nil 安全的副本语义由调用方保证不并发修改）。
func (t *CloudTask) Diag() TaskDiagnostics {
	if t == nil || t.Diagnostics == nil {
		return TaskDiagnostics{}
	}
	return *t.Diagnostics
}
