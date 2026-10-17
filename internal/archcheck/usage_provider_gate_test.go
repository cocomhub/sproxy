// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// usage_provider_gate_test.go 是「卷用量查询**单一查询面**」门禁（R30，2026-10-17）。
//
// **为什么需要**：`registry.UsageProvider` 的「外部卷后端真值覆盖」是**横切关注点**，而它曾被
// **三个调用点各自实现**（`volume_usage.go` 的唯一实现 + `volumes_api.go` 与
// `user_volume_api.go` 各一份**冗余复制**）—— 正是设计
// docs/designs/2026-10-16-quota-volume-layer.md §0 命名的根因形态：
// 「横切关注点实现成 N 个调用点义务 ⇒ 改一处漏一处 ⇒ 多源即多漂移面」。
//
// 本门禁把「唯一实现」机制化：`usage_provider_calls.tsv` 登记每文件允许的最大**直连次数**
// （只降不升）；新增文件或增加次数必须显式登记理由 —— 把「评审记得看」变成「CI 必须过」。
//
// **判据**：`.(registry.UsageProvider)` 类型断言的出现次数（文本计数，偏严不偏松 ✓）。
// 唯一实现 `pkg/server/volume_usage.go` 的优先级链（配额层 → 容量池 → 后端 UsageProvider）
// 是**全部消费方的共同入口** ✓。
//
// **变异**：在任一未登记文件里加一行 `if up, ok := be.(registry.UsageProvider); ok { _ = up }`
// → 本门禁红。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// usageProviderCallsFile 是登记表文件名（internal/archcheck/ 下）。
const usageProviderCallsFile = "usage_provider_calls.tsv"

// usageProviderAssertion 是判据字面（类型断言形态 ⇒ 只命中**真实直连**，不命中注释/文档提及 ✓）。
const usageProviderAssertion = ".(registry.UsageProvider)"

// scanRoots 是扫描根（白名单目录 ⇒ **禁**从仓库根 walk ✗ —— 曾误入 .worktrees/）。
var scanRoots = []string{"pkg", "cmd", "internal", "test", "web"}

// loadUsageProviderCalls 解析登记表（file → max_calls）。
func loadUsageProviderCalls(t *testing.T) map[string]int {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "internal", "archcheck", usageProviderCallsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", usageProviderCallsFile, err)
	}
	out := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			t.Fatalf("%s 行格式错误（want: file<TAB>max_calls<TAB>reason）: %q", usageProviderCallsFile, line)
		}
		n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			t.Fatalf("%s 次数非整数: %q", usageProviderCallsFile, line)
		}
		out[filepath.ToSlash(strings.TrimSpace(parts[0]))] = n
	}
	return out
}

// TestUsageProviderSingleQueryFace 钉住「`UsageProvider` 直连点只在唯一查询面」。
func TestUsageProviderSingleQueryFace(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	allowed := loadUsageProviderCalls(t)
	if len(allowed) == 0 {
		t.Fatal("判据非空自证失败：登记表为空 ⇒ 门禁失效")
	}

	counted := map[string]int{}
	found := 0
	for _, dir := range scanRoots {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// 排除嵌套 worktree / 依赖目录（白名单内的例外）。
				if n := d.Name(); n == ".git" || n == "node_modules" || n == ".worktrees" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			c := strings.Count(string(data), usageProviderAssertion)
			if c == 0 {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			counted[filepath.ToSlash(rel)] = c
			found += c
			return nil
		})
		if err != nil {
			t.Fatalf("扫描 %s: %v", dir, err)
		}
	}
	if found == 0 {
		t.Fatal("判据非空自证失败：全仓 0 处 `UsageProvider` 直连 ⇒ 判据字面已失效（门禁需更新）")
	}

	// ① 新增/增次 ⇒ 红（棘轮只降不升）。
	for file, c := range counted {
		max, ok := allowed[file]
		if !ok {
			t.Errorf("未登记的 `UsageProvider` 直连点：%s（%d 处）⇒ 用量查询必须经**唯一实现** "+
				"`pkg/server/volume_usage.go` 的 `volumeUsage`；确需新增请在 %s 登记并说明理由",
				file, c, usageProviderCallsFile)
			continue
		}
		if c > max {
			t.Errorf("%s 直连次数 %d > 登记上限 %d（棘轮只降不升）", file, c, max)
		}
	}
	// ② 登记项消失 ⇒ 红（强制同步收敛登记表，防白名单空转）。
	for file := range allowed {
		if counted[file] == 0 {
			t.Errorf("登记表条目已无对应直连点：%s ⇒ 请从 %s 移除（白名单不得空转）",
				file, usageProviderCallsFile)
		}
	}
}
