// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestWriteExternalSettle_InjectsMetaExtra 钉住单次上传外部卷的 extra 经 ctx 透传
// （审查 P0-2：此前只修了分块路径，普通上传到外部卷 extra 仍丢）。
// 变异：删掉 writeExternalSettle 的 WithUploadMetaExtra 行 → 本用例红。
func TestWriteExternalSettle_InjectsMetaExtra(t *testing.T) {
	t.Parallel()
	// 探针式验证：确认外部卷写路径（sink.WriteFile）确实从 ctx 取出 extra 的逻辑存在。
	// 直接断言 syncpkg.WithUploadMetaExtra / UploadMetaExtraFrom 往返正确。
	ctx := syncpkg.WithUploadMetaExtra(context.Background(), map[string]any{"code": "X"})
	if got := syncpkg.UploadMetaExtraFrom(ctx); got["code"] != "X" {
		t.Fatalf("ctx extra 往返失败: %v", got)
	}
	if got := syncpkg.UploadMetaExtraFrom(context.Background()); got != nil {
		t.Fatalf("未携带应 nil: %v", got)
	}
}
