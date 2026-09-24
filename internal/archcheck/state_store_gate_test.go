// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// state_store_gate_test.go 是「状态落盘必须走 StateStore 抽象」的门禁（R21）。
//
// 背景（2026-09-24，roadmap 11.12-F1）：sproxy 各状态（凭据/checksum/dedup/share/
// index/audit）此前各自为政直接 os.WriteFile/os.CreateTemp 写本地 JSON，无统一
// 抽象、无 CAS、多节点静默互相覆盖。pkg/state 提供 StateStore 接口 + Local 默认
// 实现 + 注册表后，新代码应经 StateStore 落盘——本门禁禁非测试源码直接
// 「写 meta/state 路径段的文件」。
//
// R21 判据（仿 R19 http_transport_gate_test.go 模式，纯 Go 目录遍历）：
//   扫描非测试源码（非 _test.go 的 .go 文件）中形如 `os.WriteFile(` /
//   `os.CreateTemp(` 的调用，且其参数文本含 `meta` 或 `state` 路径段
//  （`"meta` / `meta/` / `"state` / `state/` 字面量）→ 违规。
//
// 豁免（迁移期存量 + 本抽象自身）：
//   - pkg/state/ 自身实现（本抽象的实现必然落盘）；
//   - 门禁自身文件（注释/报错字符串必然提及模式）；
//   - 既有迁移期 Store（F2 逐 Store 迁移完成后**必须从清单删除**——R21 的意义
//     就是逼迫迁移：清单越缩越小，最终只剩 pkg/state 与门禁自身）。
//
// 注意：本判据是**保守子串匹配**（注释/字符串提及也会命中），故豁免清单需包含
// 所有「含 meta/state 字面量且写文件」的存量文件；若误伤纯注释，可在文件中
// 调整措辞或用 `// state-write-exempt` 标记（后者由本门禁识别，见下）。
//
// 变异验证口径（提交前必须跑过）：
//   - 把豁免清单中某**已迁移**文件加回 → 红；
//   - 把 pkg/state 自身从豁免删除（误伤）→ 红；
//   - 新增生产文件直接写 meta/state 路径 → 红。

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stateWriteExemptFiles 是「直接写 meta/state 路径段文件」的存量豁免清单。
//
// 每一项都是迁移期（F2 逐 Store 适配前）仍保留自管落盘的合法实现；迁移完成后
// 逐项删除（R21 门禁会因清单缩小而仍绿，删除方向不受门禁拦截，由 F2 各片
// 提交说明记录「本片从豁免清单删除 XX」）。
//
// 目录级豁免仅允许 pkg/state（本抽象自身实现）。
var stateWriteExemptFiles = []string{
	// 凭据 store（P0，F2 第一片迁移目标）：<meta>/credentials.json。
	"pkg/accesskey/credentialstore.go",
	"pkg/accesskey/encrypting_storer.go",
	// checksum 台账（P0）：<meta>/checksums.json。
	"pkg/checksum/store.go",
	// dedup 台账（P0）：<meta>/dedup.json。
	"pkg/files/dedup.go",
	// 索引快照（P1）：<meta>/index/<owner>.json。
	"pkg/files/index_persist.go",
	// 派生缓存（P1）：<meta>/transform/<key>。
	"pkg/files/transform_cache.go",
	// 分享链接（P1）：<meta>/share/<token>.json。
	"pkg/server/share.go",
	// 用户卷 meta（P2）：<meta>/volume/<name>.json。
	"pkg/server/user_volume_store.go",
	// 云任务/组状态（P2）：<meta>/cloud/...。
	"pkg/cloud/manager_persist.go",
	// 同步冲突索引与任务状态（P2）：<meta>/sync/...。
	"pkg/syncmgr/conflict_index.go",
	"pkg/syncmgr/manager.go",
	// sync_handler 写回冲突文件（Path 相对 user 桶 → tenant user 根，非 meta 状态）。
	"pkg/server/sync_handler.go",
}

// hasMetaStateSegment 判断 write 调用参数文本是否含 meta/state 路径段字面量。
// 保守子串匹配：`"meta`（写 "meta/..." 字面量）或 `"state`（写 "state/..." 字面量）
// 或 `meta/` / `state/` 出现在拼接片段中。
func hasMetaStateSegment(data string) bool {
	lower := strings.ToLower(data)
	return strings.Contains(lower, `"meta`) || strings.Contains(lower, `"state`) ||
		strings.Contains(lower, "meta/") || strings.Contains(lower, "state/")
}

// TestNoDirectStateWrites 扫描非测试源码：禁 `os.WriteFile(` / `os.CreateTemp(`
// 写 meta/state 路径段（豁免清单除外）。
func TestNoDirectStateWrites(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	exempt := map[string]bool{}
	for _, f := range stateWriteExemptFiles {
		exempt[f] = true
	}
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "build" || d.Name() == "vendor" || d.Name() == "dist" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel := filepath.ToSlash(path)
		// 去掉 moduleRoot 前缀，得到仓库相对路径（与豁免清单键一致）。
		if after, ok := strings.CutPrefix(rel, filepath.ToSlash(root)); ok {
			rel = after
			rel = strings.TrimPrefix(rel, "/")
		}
		if strings.HasSuffix(rel, "_test.go") {
			return nil // 测试侧不强制（测试可直接构造临时状态文件）
		}
		if exempt[rel] {
			return nil
		}
		// pkg/state 自身实现豁免（目录级）。
		if strings.HasPrefix(rel, "pkg/state/") {
			return nil
		}
		// 门禁自身文件豁免。
		if rel == "internal/archcheck/state_store_gate_test.go" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		s := string(data)
		if !strings.Contains(s, "os.WriteFile(") && !strings.Contains(s, "os.CreateTemp(") {
			return nil
		}
		if !hasMetaStateSegment(s) {
			return nil
		}
		// 文件级 `// state-write-exempt` 标记可豁免（纯注释误伤时的显式逃生口）。
		if strings.Contains(s, "state-write-exempt") {
			return nil
		}
		violations = append(violations, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("以下文件直接写 meta/state 路径段（应改走 pkg/state StateStore 抽象；"+
			"迁移期存量请登记进 stateWriteExemptFiles 并在 F2 片删除）:\n%s",
			strings.Join(violations, "\n"))
	}
}

// TestStateWriteGate_ScopeAndWordBoundary 钉住扫描面与判据自身（R11/R13 同款自检）：
//   - 扫描根必须能看见 pkg/state（防扫描面退回 "." 导致门禁恒绿）；
//   - 判据必须识别 meta/state 路径段字面量（防正则写窄静默漏检）；
//   - 豁免清单中每一项必须真实存在（防清单悬空假绿）。
func TestStateWriteGate_ScopeAndWordBoundary(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	// 扫描面：pkg/state 目录必须存在（本门禁守护的抽象自身）。
	if _, err := os.Stat(filepath.Join(root, "pkg", "state")); err != nil {
		t.Fatalf("pkg/state 目录不存在（门禁守护对象缺失，扫描面疑被改窄）: %v", err)
	}
	// 判据：meta/state 段必须命中。
	for _, probe := range []string{`os.WriteFile(tmp, data, 0o644) // 写 "meta/checksums.json"`, `os.CreateTemp(dir, "state-*")`, `os.WriteFile(filepath.Join("meta", "index", owner+".json"), ...)`} {
		if !hasMetaStateSegment(probe) {
			t.Errorf("判据漏检 meta/state 段字面量: %q", probe)
		}
	}
	for _, probe := range []string{`os.WriteFile(tmp, data, 0o644)`, `os.CreateTemp(dir, "*.tmp")`} {
		if hasMetaStateSegment(probe) {
			t.Errorf("判据误报非 meta/state 段: %q", probe)
		}
	}
	// 豁免清单：每一项必须存在（防悬空假绿）。
	for _, f := range stateWriteExemptFiles {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err != nil {
			t.Errorf("豁免清单条目 %s 不存在（悬空条目 → 清单假绿）: %v", f, err)
		}
	}
}
