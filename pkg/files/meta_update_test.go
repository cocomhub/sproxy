// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestSyncModeOf_Mapping 钉住 files 域模式 → syncpkg 模式的单一映射。
// 变异：把 replace 映射成 merge → 上层测试（replace 语义）红。
func TestSyncModeOf_Mapping(t *testing.T) {
	t.Parallel()
	if syncModeOf(MetaUpdateReplace) != syncpkg.MetaUpdateReplace {
		t.Fatal("replace 映射错")
	}
	if syncModeOf(MetaUpdateDelete) != syncpkg.MetaUpdateDelete {
		t.Fatal("delete 映射错")
	}
	if syncModeOf("") != syncpkg.MetaUpdateMerge || syncModeOf(MetaUpdateMerge) != syncpkg.MetaUpdateMerge {
		t.Fatal("零值/merge 应映射 merge")
	}
}

// TestSyncMetaUpdateMode_DefaultMerge 钉住 ctx 未携带模式时底层默认 merge。
func TestSyncMetaUpdateMode_DefaultMerge(t *testing.T) {
	t.Parallel()
	if got := syncpkg.MetaUpdateModeFrom(context.Background()); got != syncpkg.MetaUpdateMerge {
		t.Fatalf("默认应 merge, got %s", got)
	}
	if got := syncpkg.MetaUpdateModeFrom(syncpkg.WithMetaUpdateMode(context.Background(), syncpkg.MetaUpdateReplace)); got != syncpkg.MetaUpdateReplace {
		t.Fatalf("replace 应透传, got %s", got)
	}
}
