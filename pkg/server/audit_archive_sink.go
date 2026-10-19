// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_archive_sink.go 用 **pkg/sync 的 FS 能力**实现 `audit.SegmentSink`
// （用户裁定 2026-10-20：归档与「云端下载 → 转存」同类 ⇒ 实际字节搬运必须复用 sync 能力，
// 审计包只做编排/护栏，绝不重复实现拷贝 ✗）。
//
// 收益：本地目录 / 外部卷 / 加密卷 / 远端**共用同一套 FS 语义**（`WriteFile`/`Stat`/`OpenRangeRead`），
// 换目的地只换 FS 实现，归档侧零改动 ✓。

import (
	"context"
	"io"
	"log/slog"
	"path"

	"github.com/cocomhub/sproxy/pkg/audit"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// syncSegmentSink 把 sync.FS 适配为 audit.SegmentSink（**唯一**的实际写实现）。
type syncSegmentSink struct{ fs syncpkg.FS }

// Put 写整个对象（自动确保父目录存在；归属键用零值——归档是系统数据，不做用户配额归属）。
func (s syncSegmentSink) Put(ctx context.Context, p string, r io.Reader, size int64) error {
	if dir := path.Dir(p); dir != "." && dir != "/" {
		_ = s.fs.MakeDir(ctx, dir) // 已存在不算错；真正失败由 WriteFile 报出
	}
	return s.fs.WriteFile(ctx, syncpkg.LogicalKey{}, p, r, size, 0)
}

// Size 返回已有对象大小（不存在 ⇒ (0,false,nil)——归档侧据此幂等跳过）。
func (s syncSegmentSink) Size(ctx context.Context, p string) (int64, bool, error) {
	e, err := s.fs.Stat(ctx, p)
	if err != nil {
		if syncpkg.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if e == nil {
		return 0, false, nil
	}
	return e.Size, true, nil
}

// newColdSegmentSink 构造冷段 sink。当前用 `sync.NewLocalFS`（冷段目录，可挂载任意卷）；
// 要落到外部/加密卷时只需换成该卷的 FS（同一接口 ⇒ 归档侧不改）。
func newColdSegmentSink(dir string, log *slog.Logger) audit.SegmentSink {
	return syncSegmentSink{fs: syncpkg.NewLocalFS(dir, log)}
}
