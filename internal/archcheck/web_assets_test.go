// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// web_assets_test.go 是「**Web 前端每个 JS 文件都必须被自动化测试覆盖**」的门禁（R10）。
//
// 触发背景（实测踩到）：W2 新增 `web/static/sclient/api/mesh.js` 时，忘了把它加进 Makefile 的
// `make web-test` 目标 ⇒ 该文件既没有 `node --check`（语法检查）也没有 `node --test`（单测），
// 而更糟的是 **`make web-test` 当时根本没在 CI 里跑**——两道缺口叠加意味着「前端改坏了也不会红」。
//
// 判据（结构性，不做语义猜测）：
//  1. `web/static` 下每个非 vendor 的 `.js` 文件，都必须在 Makefile 里被 `node --check` 或
//     `node --test` 引用；漏一个即失败并列出文件名（修复动作明确：加进 `web-test`）。
//  2. 每个 `*.test.js` 必须被 `node --test` 引用（只 --check 不算覆盖）。
//  3. `web-test` 必须**被 CI 引用**（否则门禁只在本地有效，CI 永远不跑 ⇒ 形同虚设）。
//
// 为什么用「Makefile 引用」作为判据：Makefile 的 web-test 是唯一的前端测试入口（本地与 CI 同源），
// 断言它覆盖完整 = 断言「没有前端文件可以绕过测试」。比逐个列白名单更不容易腐坏。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// makefileWebTestTarget 返回 Makefile 中 web-test 目标的正文（到下一个目标前）。
func makefileWebTestTarget(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("读 Makefile: %v", err)
	}
	lines := strings.Split(string(b), "\n")
	var body []string
	in := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "web-test:") {
			in = true
			continue
		}
		if in {
			// 目标正文 = 以 tab 开头的续行；遇下一个目标/注释即结束。
			if strings.HasPrefix(ln, "\t") {
				body = append(body, ln)
				continue
			}
			if strings.TrimSpace(ln) == "" {
				body = append(body, ln)
				continue
			}
			break
		}
	}
	if len(body) == 0 {
		t.Fatal("Makefile 缺少 web-test 目标（前端测试入口）")
	}
	return strings.Join(body, "\n")
}

// TestWebAssetsAllCoveredByWebTest 断言 web/static 下每个非 vendor 的 .js 都被 web-test 引用。
func TestWebAssetsAllCoveredByWebTest(t *testing.T) {
	root := moduleRoot(t)
	target := makefileWebTestTarget(t, root)

	webStatic := filepath.Join(root, "web", "static")
	var uncovered, testNotRun []string
	err := filepath.WalkDir(webStatic, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".js") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if !strings.Contains(target, rel) {
			uncovered = append(uncovered, rel)
			return nil
		}
		// `*.test.js` 必须真的被 `node --test` 跑，而不是只做语法检查。
		if strings.HasSuffix(d.Name(), ".test.js") {
			re := regexp.MustCompile(`node --test\s+` + regexp.QuoteMeta(rel) + `(\s|$)`)
			if !re.MatchString(target) {
				testNotRun = append(testNotRun, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 web/static: %v", err)
	}
	if len(uncovered) > 0 {
		t.Fatalf("以下前端 JS 未被 Makefile 的 web-test 覆盖（既无 node --check 也无 node --test）：\n  %s\n"+
			"修复：把它们加进 web-test（新增测试文件用 `node --test`，纯模块用 `node --check`）。\n"+
			"背景：曾因漏加 api/mesh.js + web-test 未接入 CI，导致前端改坏也不报错。",
			strings.Join(uncovered, "\n  "))
	}
	if len(testNotRun) > 0 {
		t.Fatalf("以下测试文件未被 `node --test` 执行（只做语法检查等于不跑）：\n  %s",
			strings.Join(testNotRun, "\n  "))
	}
}

// TestWebTestTargetWiredIntoCI 断言 web-test **被 CI 调用**：未接入 CI 的门禁只在本地有效。
func TestWebTestTargetWiredIntoCI(t *testing.T) {
	root := moduleRoot(t)
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("读 ci.yml: %v", err)
	}
	ci := string(b)
	if !regexp.MustCompile(`(?m)^\s*(run:\s*)?(make\s+)?make web-test\s*$`).MatchString(ci) &&
		!strings.Contains(ci, "make web-test") {
		t.Fatal("CI 未调用 `make web-test`：前端单测/语法检查只在本地跑，坏了也不会红。\n" +
			"修复：在 ui-e2e job（必检项 UI E2E Tests）里加一步 `run: make web-test`。")
	}
	// 且必须挂在 ui-e2e job 内（必检项），而不是某个可跳过的 job。
	uiIdx := strings.Index(ci, "ui-e2e:")
	if uiIdx < 0 {
		t.Fatal("ci.yml 缺少 ui-e2e job")
	}
	if !strings.Contains(ci[uiIdx:], "make web-test") {
		t.Fatal("`make web-test` 未挂在 ui-e2e job（UI E2E Tests 是必检项）内")
	}
}
