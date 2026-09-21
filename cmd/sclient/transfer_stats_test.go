// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestTransferStats_AddFileAndFinalize 验证文件明细累加 + 平均速率汇总。
func TestTransferStats_AddFileAndFinalize(t *testing.T) {
	t.Parallel()
	s := NewTransferStats()
	s.AddFile("a.txt", 100, time.Second)
	s.AddFile("b.txt", 50, 500*time.Millisecond)
	s.Finalize()

	if s.FileCount != 2 {
		t.Fatalf("FileCount = %d, want 2", s.FileCount)
	}
	if s.TotalBytes != 150 {
		t.Fatalf("TotalBytes = %d, want 150", s.TotalBytes)
	}
	// 平均速率 = 150 B / 1.5s = 100 B/s
	if s.AvgRate != 100 {
		t.Fatalf("AvgRate = %v, want 100", s.AvgRate)
	}
	if len(s.Files) != 2 {
		t.Fatalf("Files 长度 = %d, want 2", len(s.Files))
	}
	if s.Files[0].Rate != 100 {
		t.Fatalf("Files[0].Rate = %v, want 100", s.Files[0].Rate)
	}
	if s.Files[1].Rate != 100 {
		t.Fatalf("Files[1].Rate = %v, want 100", s.Files[1].Rate)
	}
}

// TestTransferStats_ZeroElapsed 验证零耗时文件不除零（rate/avg 均为 0）。
func TestTransferStats_ZeroElapsed(t *testing.T) {
	t.Parallel()
	s := NewTransferStats()
	s.AddFile("instant.txt", 10, 0)
	s.Finalize()

	if s.AvgRate != 0 {
		t.Fatalf("零耗时 AvgRate = %v, want 0（不除零）", s.AvgRate)
	}
	if s.Files[0].Rate != 0 {
		t.Fatalf("零耗时 Files[0].Rate = %v, want 0", s.Files[0].Rate)
	}
}

// TestTransferStats_ZeroBytes 验证 0 字节文件速率为 0。
func TestTransferStats_ZeroBytes(t *testing.T) {
	t.Parallel()
	s := NewTransferStats()
	s.AddFile("empty.txt", 0, time.Second)
	s.Finalize()

	if s.AvgRate != 0 {
		t.Fatalf("0 字节 AvgRate = %v, want 0", s.AvgRate)
	}
	if s.TotalBytes != 0 {
		t.Fatalf("TotalBytes = %d, want 0", s.TotalBytes)
	}
	if s.Files[0].Rate != 0 {
		t.Fatalf("0 字节 Files[0].Rate = %v, want 0", s.Files[0].Rate)
	}
}

// TestTransferStats_SetChunkSuccessRate 验证分块成功率计算（全成功/部分失败/无效输入）。
func TestTransferStats_SetChunkSuccessRate(t *testing.T) {
	t.Parallel()

	s := NewTransferStats()
	s.SetChunkSuccessRate(4, 0)
	if s.ChunkSuccessRate == nil || *s.ChunkSuccessRate != 100 {
		t.Fatalf("全成功成功率 = %v, want 100", s.ChunkSuccessRate)
	}

	s2 := NewTransferStats()
	s2.SetChunkSuccessRate(4, 1)
	if s2.ChunkSuccessRate == nil || *s2.ChunkSuccessRate != 75 {
		t.Fatalf("3/4 成功率 = %v, want 75", s2.ChunkSuccessRate)
	}

	s3 := NewTransferStats()
	s3.SetChunkSuccessRate(0, 0)
	if s3.ChunkSuccessRate != nil {
		t.Fatalf("totalChunks=0 应保持 nil, got %v", s3.ChunkSuccessRate)
	}
}

// TestTransferStats_FormatLine 验证表格统计行内容（分块成功率段/非分块省略/服务端任务）。
func TestTransferStats_FormatLine(t *testing.T) {
	t.Parallel()

	s := NewTransferStats()
	s.AddFile("a.txt", 1024*1024, time.Second)
	s.SetChunkSuccessRate(4, 0)
	s.Finalize()
	line := s.FormatLine()
	for _, want := range []string{"耗时", "速率", "文件 1", "分块成功率 100.0%"} {
		if !strings.Contains(line, want) {
			t.Fatalf("统计行缺 %q: %s", want, line)
		}
	}

	// 非分块：不出现成功率段
	s2 := NewTransferStats()
	s2.AddFile("b.txt", 10, time.Second)
	s2.Finalize()
	if strings.Contains(s2.FormatLine(), "分块成功率") {
		t.Fatalf("非分块不应含成功率段: %s", s2.FormatLine())
	}

	// 服务端任务：速率 N/A + 任务数
	s3 := NewTransferStats()
	s3.ServerSideTask = true
	s3.Requests = 3
	s3.AddFile("cloud-download", 0, 2*time.Second)
	s3.Finalize()
	line3 := s3.FormatLine()
	for _, want := range []string{"N/A", "任务 3 个", "服务端任务"} {
		if !strings.Contains(line3, want) {
			t.Fatalf("服务端任务行缺 %q: %s", want, line3)
		}
	}
}

// TestTransferStats_JSON 验证 JSON 输出字段（脚本可解析契约）。
func TestTransferStats_JSON(t *testing.T) {
	t.Parallel()
	s := NewTransferStats()
	s.AddFile("a.txt", 100, time.Second)
	s.Finalize()

	var decoded map[string]any
	if err := json.Unmarshal([]byte(s.FormatJSON()), &decoded); err != nil {
		t.Fatalf("FormatJSON 解析失败: %v", err)
	}
	for _, k := range []string{"file_count", "total_bytes", "elapsed_ns", "avg_rate_bps", "files"} {
		if _, ok := decoded[k]; !ok {
			t.Fatalf("JSON 缺字段 %q: %s", k, s.FormatJSON())
		}
	}
	files, ok := decoded["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("files 应为 1 条明细, got %v", decoded["files"])
	}
}
