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

// audit_calls_gate_test.go 是门禁 R35-a：审计调用点**棘轮**（只减不增）。
//
// 设计（docs/designs/2026-10-19-audit-service.md §8.2）：审计能力唯一由 pkg/audit 提供；
// 业务审计调用点（RecordAudit( / recordFileAudit(）必须登记；新增散点会绕开唯一实现与覆盖
// 门禁 ⇒ 用登记表 + 棘轮锁死（沿用本仓 R30/R31/R34 纪律）。
//
// 判据：① 未登记文件不得出现审计调用点；② 已登记文件调用点数不得超过基线。
func TestAuditCallSites_Ratchet(t *testing.T) {
	t.Parallel()
	baseline := readAuditCallsBaseline(t)
	for f, n := range countAuditCalls(t) {
		base, ok := baseline[f]
		if !ok {
			t.Errorf("未登记的审计调用点文件：%s（%d 处）—— 请登记到 internal/archcheck/audit_calls.tsv 并说明理由（R35-a）", f, n)
			continue
		}
		if n > base {
			t.Errorf("审计调用点增加：%s 基线 %d → 实际 %d（棘轮只减不增）", f, base, n)
		}
	}
}

func readAuditCallsBaseline(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile("audit_calls.tsv")
	if err != nil {
		t.Fatalf("读取 audit_calls.tsv 失败: %v", err)
	}
	out := make(map[string]int)
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			t.Fatalf("audit_calls.tsv 行格式非法: %q", line)
		}
		n, cerr := strconv.Atoi(strings.TrimSpace(parts[1]))
		if cerr != nil {
			t.Fatalf("audit_calls.tsv 数量非法: %q", line)
		}
		out[filepath.ToSlash(strings.TrimSpace(parts[0]))] = n
	}
	if len(out) == 0 {
		t.Fatal("audit_calls.tsv 为空（门禁失效）")
	}
	return out
}

func countAuditCalls(t *testing.T) map[string]int {
	t.Helper()
	// 计入三种形态：装配层的 `.RecordAudit(` / `recordFileAudit(`，以及领域编排核心经注入的
	// 审计接口调用（`Audit.Record(`，P1-c S2a 起 cloudtasks 的调用点形态）——迁移不得让
	// 审计调用点从棘轮视野里消失（否则等于用依赖注入绕过门禁）。
	callRe := regexp.MustCompile(`\.RecordAudit\(|recordFileAudit\(|Audit\.Record\(`)
	out := make(map[string]int)
	root := moduleRoot(t)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// 跳过非源码目录（worktree 副本 / 构建产物 / 依赖缓存），否则他人分支的文件会被算进来。
		slash := "/" + filepath.ToSlash(path)
		for _, skip := range []string{"/.worktrees/", "/build/", "/node_modules/", "/.git/"} {
			if strings.Contains(slash, skip) {
				return nil
			}
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		n := 0
		for line := range strings.SplitSeq(string(b), "\n") {
			if strings.Contains(line, "func ") {
				continue
			}
			n += len(callRe.FindAllString(line, -1))
		}
		if n > 0 {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			out[filepath.ToSlash(rel)] = n
		}
		return nil
	})
	return out
}
