// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// audit_types_gate_test.go 是门禁 R35-c：**审计类型/常量/接口的唯一实现处 = pkg/audit**。
//
// 设计（docs/archive/2026-10-19-audit-service.md §2.1/§8.2）：其他包只装配/使用，不得自造
// 审计条目类型、结果常量或审计接口（否则出现「两套语义」与同名不同实现类缺陷）。
//
// 已登记的遗留例外（**只减不增**，收敛见设计 M1-4）：
//   - pkg/files：Auditor 接口与 auditResult* 常量（文件域写面族的历史抽象）。
func TestAuditTypes_OnlyInAuditPackage(t *testing.T) {
	t.Parallel()
	decl := regexp.MustCompile(`^(?:type|const|var)\s+(?:Auditor|AuditEvent|AuditEntry|AuditResult|auditResult[A-Za-z]*)`)
	allow := map[string]bool{
		"pkg/files/options.go": true, // Auditor 接口（遗留，待收敛）
		"pkg/files/service.go": true, // auditResult* 常量（遗留，待收敛）
		"pkg/server/audit.go":  true, // AuditEvent：**对外 JSON 视图**（读面投影，非第二实现）
	}
	var violations []string
	// 扫描根必须用 moduleRoot：`go test` 以**包目录**为 cwd，`WalkDir("..")` 只会扫到
	// internal/ —— 而本门禁管的是 pkg/ 的审计类型（实测：在 pkg/server 里新增一个
	// AuditEvent 系类型，旧实现在门禁下**依旧绿**）。
	root := moduleRoot(t)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// 跳过 worktree 副本 / 构建产物 / 依赖缓存（与其它全仓门禁同口径）。
		slash := "/" + filepath.ToSlash(path)
		for _, skip := range []string{"/.worktrees/", "/build/", "/node_modules/", "/.git/"} {
			if strings.Contains(slash, skip) {
				return nil
			}
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "pkg/audit/") || allow[rel] {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			if decl.MatchString(line) {
				violations = append(violations, rel+":"+strconv.Itoa(i+1)+" "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if len(violations) > 0 {
		t.Errorf("审计类型/常量/接口只能定义在 pkg/audit（R35-c）；发现 %d 处：\n  %s", len(violations), strings.Join(violations, "\n  "))
	}
}
