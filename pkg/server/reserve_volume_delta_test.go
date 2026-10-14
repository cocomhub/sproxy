// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// reserve_volume_delta_test.go 覆盖外部卷卷容量**净增**探测（第 5 轮对抗评审 P1：
// 原先按全量 size 探测 → used==capacity 后该卷一切写入（含覆盖写更小内容）恒 507）。

import (
	"context"
	"errors"
	"io"
	"testing"
)

// stubUploadSink 是仅实现 Stat 的 files.UploadSink 测试替身。
type stubUploadSink struct {
	size   int64
	exists bool
	err    error
}

func (s *stubUploadSink) MakeDir(context.Context, string) error { return nil }
func (s *stubUploadSink) WriteFile(context.Context, string, io.Reader, int64, int64) error {
	return nil
}
func (s *stubUploadSink) Stat(context.Context, string) (int64, bool, error) {
	return s.size, s.exists, s.err
}
func (s *stubUploadSink) Remove(context.Context, string) error { return nil }

// TestExternalReserveNeed 覆盖写只预留净增；不存在/Stat 失败回落全量。
func TestExternalReserveNeed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name string
		sink *stubUploadSink
		size int64
		want int64
	}{
		{"覆盖成更小 → 净增 0", &stubUploadSink{size: 100, exists: true}, 40, 0},
		{"覆盖成等大 → 净增 0", &stubUploadSink{size: 100, exists: true}, 100, 0},
		{"覆盖成更大 → 仅增量", &stubUploadSink{size: 100, exists: true}, 250, 150},
		{"新文件 → 全量", &stubUploadSink{}, 250, 250},
		{"Stat 失败 → 全量（fail-safe）", &stubUploadSink{err: errors.New("boom")}, 250, 250},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := externalReserveNeed(ctx, c.sink, "user/x.bin", c.size); got != c.want {
				t.Fatalf("got %d want %d", got, c.want)
			}
		})
	}
}
