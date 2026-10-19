// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// audit_api_gate_test.go 是门禁 R35-d：`pkg/audit` 只能有**一套**审计 API。
//
// 设计（docs/designs/2026-10-19-audit-service.md §2.1/§8.2）要求「审计能力唯一由 pkg/audit 提供、
// 包内亦只有一套实现」：新 API = Service/Scope/Step/Journal/Index/Entry；旧 API
// （Logger/Row/Sink/FileSink/NewScope/NewScopeSink/ScopeOpts/WithContext/From/Archiver/Filter/Span）
// 必须整体删除（不做兼容门面 ⇒ 避免两套语义漂移与「同名封装不同实现」类缺陷）。
//
// 判据（机械化）：非测试源码中不得再出现旧 API 的声明；span/kv 入口各只有一套。
func TestAuditAPI_OnlyOneImplementation(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("..", "..", "pkg", "audit")
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	legacyDecl := regexp.MustCompile(`^(?:type|func|var)\s+(?:Logger|Row|Sink|FileSink|ScopeOpts|Archiver|Filter|Span|NewScope|NewScopeSink|WithContext|From)\b`)
	stepDecl := regexp.MustCompile(`^type Step\b`)
	kvDecl := regexp.MustCompile(`^func \(sc \*Scope\) (?:Save|Get)\b`)
	var src strings.Builder
	var step, kv int
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("ReadFile(%s): %v", name, rerr)
		}
		src.Write(b)
		for i, line := range strings.Split(string(b), "\n") {
			if legacyDecl.MatchString(line) {
				t.Errorf("pkg/audit/%s:%d 出现旧 API 声明（R35-d 要求唯一实现，旧 API 必须删除）：%s",
					name, i+1, strings.TrimSpace(line))
			}
			if stepDecl.MatchString(line) {
				step++
			}
			if kvDecl.MatchString(line) {
				kv++
			}
		}
	}
	if step != 1 {
		t.Errorf("span 入口应恰有一个类型（Step），实际 %d（R35-d：不得两套 span API）", step)
	}
	if kv != 2 {
		t.Errorf("kv 入口应恰为 Scope.Save/Get 两个声明，实际 %d", kv)
	}
	if !strings.Contains(src.String(), "func New(") {
		t.Errorf("缺少统一构造入口 New(Options)")
	}
}
