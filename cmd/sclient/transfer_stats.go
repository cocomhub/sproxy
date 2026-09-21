// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// FileStat 是单个文件的传输统计明细。
type FileStat struct {
	Path    string  `json:"path"`
	Bytes   int64   `json:"bytes"`
	Elapsed float64 `json:"elapsed_sec"`
	Rate    float64 `json:"rate_bps"`
}

// TransferStats 汇总一次 upload/download/cloud-download 命令的传输统计。
// 供 --json 脚本解析与表格统计行展示。
type TransferStats struct {
	// FileCount 是传输的文件数。
	FileCount int `json:"file_count"`
	// TotalBytes 是所有文件传输的总字节数。
	TotalBytes int64 `json:"total_bytes"`
	// Elapsed 是整个命令的总耗时（秒）。
	Elapsed float64 `json:"elapsed_sec"`
	// AvgRate 是平均速率（B/s，Elapsed=0 时为 0）。
	AvgRate float64 `json:"avg_rate_bps"`
	// Files 是逐文件明细。
	Files []FileStat `json:"files"`
	// ChunkSuccessRate 是分块传输的成功率（0-100，仅分块传输且数据可得时非 nil）。
	// 普通上传/下载为 nil（字段省略，脚本解析者零破坏）。
	ChunkSuccessRate *float64 `json:"chunk_success_rate,omitempty"`
	// ServerSideTask 标记服务端任务（cloud-download）：本地无传输速率，速率展示 N/A。
	ServerSideTask bool `json:"server_side_task,omitempty"`
	// Requests 是服务端任务的请求/任务数（cloud-download 提交数）。
	Requests int `json:"requests,omitempty"`

	// started 记录命令开始时间（内部用，不入 JSON）。
	started time.Time
	// accElapsed 累加各文件实测耗时（内部用，AvgRate 基准；不入 JSON）。
	accElapsed time.Duration
}

// NewTransferStats 创建传输统计收集器，并记录命令开始时间。
func NewTransferStats() *TransferStats {
	return &TransferStats{started: time.Now()}
}

// AddFile 追加一个文件的传输明细（bytes 与 elapsed 由调用方实测提供）。
func (s *TransferStats) AddFile(path string, bytes int64, elapsed time.Duration) {
	secs := elapsed.Seconds()
	rate := 0.0
	if secs > 0 && bytes > 0 {
		rate = float64(bytes) / secs
	}
	s.Files = append(s.Files, FileStat{Path: path, Bytes: bytes, Elapsed: secs, Rate: rate})
	s.TotalBytes += bytes
	s.accElapsed += elapsed
}

// SetChunkSuccessRate 设置分块传输成功率（totalChunks<=0 时忽略，保持 nil）。
func (s *TransferStats) SetChunkSuccessRate(totalChunks, failedChunks int) {
	if totalChunks <= 0 {
		return
	}
	ok := max(totalChunks-failedChunks, 0)
	rate := float64(ok) * 100 / float64(totalChunks)
	s.ChunkSuccessRate = &rate
}

// Finalize 在命令结束时汇总（补齐 FileCount/Elapsed/AvgRate）。
// AvgRate 基于各文件实测耗时累加（accElapsed）而非墙钟——测试可注入确定耗时；
// 墙钟 Elapsed 仅用于命令总耗时展示。
func (s *TransferStats) Finalize() {
	s.FileCount = len(s.Files)
	s.Elapsed = time.Since(s.started).Seconds()
	acc := s.accElapsed.Seconds()
	if acc > 0 && s.TotalBytes > 0 {
		s.AvgRate = float64(s.TotalBytes) / acc
	}
}

// FormatLine 生成表格模式的一行统计摘要。
func (s *TransferStats) FormatLine() string {
	if s.ServerSideTask {
		// 服务端任务：不假装本地有传输速率（任务在服务端跑）。
		return fmt.Sprintf("耗时 %.1fs | 速率 N/A | 任务 %d 个 | 服务端任务",
			s.Elapsed, s.Requests)
	}
	line := fmt.Sprintf("耗时 %.1fs | 速率 %s/s | 文件 %d",
		s.Elapsed, client.FormatByte(s.AvgRate), s.FileCount)
	if s.ChunkSuccessRate != nil {
		line += fmt.Sprintf(" | 分块成功率 %.1f%%", *s.ChunkSuccessRate)
	}
	return line
}

// FormatJSON 输出脚本可解析的 JSON 统计（snake_case 字段）。
func (s *TransferStats) FormatJSON() string {
	b, err := json.MarshalIndent(map[string]any{
		"file_count":         s.FileCount,
		"total_bytes":        s.TotalBytes,
		"elapsed_ns":         int64(s.Elapsed * float64(time.Second)),
		"avg_rate_bps":       s.AvgRate,
		"files":              s.Files,
		"chunk_success_rate": s.ChunkSuccessRate,
		"server_side_task":   s.ServerSideTask,
		"requests":           s.Requests,
	}, "", "  ")
	if err != nil {
		return `{"error":"stats json marshal failed"}`
	}
	return string(b)
}
