// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteKernelHasNoDirectIndexWrites（R47）守「派生状态单一写入口」
// （设计 2026-10-30-derived-state-projection I1）：写内核（`pkg/files/write_ops.go`）
// 不得直接调用搜索索引的写方法——索引只由 `storage.Root` 变异事件 → `ApplyRootMutation`
// 维护。写内核 + 事件双写会让每次写做两次全量 COW 深拷贝，且两条路径语义漂移时
// 只有一条被测试覆盖。
//
// 允许显式索引写的文件（不在此门禁范围）：
//   - `mutation_index.go`（投影层本身）；
//   - `tags.go`（标签不是 Root 事件，属显式写面）；
//   - `read_ops.go`（只读）；`search_index.go`（实现自身）；
//   - `service.go` 的 `RecordBypassWrite`（外部卷无本地 Root，事件不可达）。
//
// 变异：向 write_ops.go 加回 `s.index.upsert(...)` ⇒ 本门禁红。
func TestWriteKernelHasNoDirectIndexWrites(t *testing.T) {
	t.Parallel()
	path := filepath.Join(moduleRoot(t), "pkg", "files", "write_ops.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	src := string(raw)
	// 覆盖探针：扫描面变窄（文件被替换/方法改名）时立刻红。
	if strings.Count(src, "func (s *Service) ") < 2 {
		t.Fatal("write_ops.go 未包含预期的写内核方法——扫描面被改窄")
	}
	for _, token := range []string{
		"s.index.upsert(",
		"s.index.upsertDir(",
		"s.index.remove(",
		"s.index.removePrefix(",
		"s.index.rename(",
	} {
		if strings.Contains(src, token) {
			t.Fatalf("写内核不得直接调用 %s（R47）：搜索索引只由 storage.Root 事件投影维护；"+
				"新增写面请依赖 Root 事件或在 mutation_index.go 扩展投影", token)
		}
	}
}
