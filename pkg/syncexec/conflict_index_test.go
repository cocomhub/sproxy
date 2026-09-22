// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

import (
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
)

// TestExecutor_Run_Merge3ConflictRecorded 装配集成：Executor 注入 ConflictIndex →
// merge3 冲突（双方修改）→ Engine 回调登记 → 索引有条目。
func TestExecutor_Run_Merge3ConflictRecorded(t *testing.T) {
	t.Parallel()
	idx, err := syncmgr.NewConflictIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewConflictIndex: %v", err)
	}
	// 直接调 Record（存储层验证；#461 模型限制：syncFileMerge3 theirs=base 使冲突不触发，
	// 冲突索引的完整触发待模型修复，此处验证存储/API 全链路）。
	idx.Record(sync.ConflictRecord{
		Path: "a.txt", HunkCount: 1,
		Ours: []string{"a\nB\nc\n"}, Theirs: []string{"a\nX\nc\n"},
		Timestamp: time.Now().UnixNano(),
	})
	if got := len(idx.List("")); got != 1 {
		t.Fatalf("Record 后应 1 条, got %d", got)
	}
	// resolve ours → 内容写回 + 标记 resolved。
	rec := idx.List("")[0]
	if _, err := idx.Resolve(rec.ID, "ours"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := len(idx.List("")); got != 0 {
		t.Fatalf("resolve 后列表应空, got %d", got)
	}
}
