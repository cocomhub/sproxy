// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// moveOnlyAdapter 是只支持 Move 的替身：**移动成功**，但 Meta 恒 ErrNotFound
// （精确模拟实测现象：移动后 Meta 可见性偶发延迟 30–60s）。
type moveOnlyAdapter struct {
	moved int
}

func (m *moveOnlyAdapter) Upload(context.Context, string, string, bool) error { return ErrUnsupported }
func (m *moveOnlyAdapter) Download(context.Context, string, string) error     { return ErrUnsupported }
func (m *moveOnlyAdapter) Copy(context.Context, string, string) error         { return ErrUnsupported }
func (m *moveOnlyAdapter) Move(context.Context, string, string) error {
	m.moved++
	return nil
}
func (m *moveOnlyAdapter) Meta(context.Context, string) (*ObjectMeta, error) {
	return nil, ErrNotFound // 模拟"移动后 Meta 不可见"（实测偶发）
}
func (m *moveOnlyAdapter) List(context.Context, string) ([]ObjectMeta, error) {
	return nil, ErrNotFound
}

var (
	_ Adapter          = (*moveOnlyAdapter)(nil)
	_ metadataProvider = (*moveOnlyAdapter)(nil)
)

// TestStorage_Move_DoesNotRequirePostStat 钉住 **Move 成功即成功**（2026-11-08 实测定论）：
//   - 移动 API 的返回值是可靠成功信号（实测：链式 8/8、突发 50/50 全成功）；
//   - 而**移动后 Meta 的可见性偶发延迟**（同一结构实测出现过 ≥60s 不可见、也出现过 0.14s 可见）
//     ⇒ 若在成功后用 Stat 复核，就会把**已成功**的移动报成 `31066 file does not exist`
//     （线上表现：迁移报假失败 → 重试放大 → 整轮中止）。
//   - 移动**不是数据操作**（内容不变、无 md5 语义）⇒ 此处不需要摘要；
//     **上传/下载等数据校验仍必须用 Meta/Stat 取 md5**，不因本改动放宽。
//
// **变异**：把实现改回 `return s.Stat(ctx, dstKey)` ⇒ 本用例红。
func TestStorage_Move_DoesNotRequirePostStat(t *testing.T) {
	t.Parallel()
	ad := &moveOnlyAdapter{}
	st, err := NewStorage(StorageConfig{Root: "/r", TempDir: t.TempDir(), Adapter: ad, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	// Put/Stat 等仍应正常报 ErrNotFound（替身语义），确保我们测的是 Move 而非 Stat 的放宽
	if _, serr := st.Stat(context.Background(), "a"); !errors.Is(serr, ErrNotFound) {
		t.Fatalf("替身 Stat 语义应保持 ErrNotFound，got %v", serr)
	}
	if _, merr := st.Move(context.Background(), "a", "b"); merr != nil {
		t.Fatalf("Move 成功后不得因 Meta 不可见而报错（实测：Meta 可见性偶发延迟），got %v", merr)
	}
	if ad.moved != 1 {
		t.Fatalf("应调用底层 Move 一次，got %d", ad.moved)
	}
}
