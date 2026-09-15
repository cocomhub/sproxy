// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMakefileTargetsCompilingAllNeedPrepare 门禁：以 `prepare` 产出 embed 依赖的目标必须声明该依赖。
//
// 背景（2026-09-15，CI run 34939978058）：`bench` 目标缺 `prepare` 前置 ⇒
// `internal/buildmeta` 的 `//go:embed build/dirty_info.txt` 在干净 checkout 下找不到文件
// ⇒ 包编译失败；又因 Benchmark job 设了 `timeout-minutes: 8`，后续挂起被截断为 cancel，
// 表面现象是「Benchmark 超时」，真实根因却是缺前置目标（本地 `make bench` 能一眼复现）。
//
// 判据：Makefile 中会编译/测试**根 module 全量包**（`go test ... ./...` / `go build ... ./...`）
// 且会构建 `internal/buildmeta` 的目标，必须显式依赖 `prepare`（直接或经由别名）。
func TestMakefileTargetsCompilingAllNeedPrepare(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("读取 Makefile: %v", err)
	}
	lines := strings.Split(string(b), "\n")

	type tgt struct {
		name    string
		deps    string
		recipes []string
	}
	var targets []tgt
	cur := -1
	for _, ln := range lines {
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if !strings.HasPrefix(ln, "\t") && strings.Contains(ln, ":") && !strings.HasPrefix(ln, ".") {
			parts := strings.SplitN(ln, ":", 2)
			targets = append(targets, tgt{name: strings.TrimSpace(parts[0]), deps: strings.TrimSpace(parts[1])})
			cur = len(targets) - 1
			continue
		}
		if cur >= 0 && strings.HasPrefix(ln, "\t") {
			targets[cur].recipes = append(targets[cur].recipes, strings.TrimSpace(ln))
		}
	}
	if len(targets) < 20 {
		t.Fatalf("Makefile 目标解析异常（仅 %d 个）：门禁自检失败", len(targets))
	}
	checked := 0
	for _, g := range targets {
		body := strings.Join(g.recipes, " ")
		compilesAll := strings.Contains(body, "./...") && (strings.Contains(body, "$(GO) test") || strings.Contains(body, "$(GO) build"))
		if !compilesAll {
			continue
		}
		checked++
		if !strings.Contains(" "+g.deps+" ", " prepare ") && g.deps != "prepare" {
			t.Fatalf("目标 %s 会编译根 module 全量包（./...）但未依赖 `prepare` ⇒ internal/buildmeta 的 "+
				"//go:embed build/dirty_info.txt 在干净 checkout 下缺失（CI Benchmark 曾因此失败）", g.name)
		}
	}
	if checked == 0 {
		t.Fatal("门禁自检失败：未匹配到任何「全量编译」目标（判据失效，需更新本测试）")
	}
	_ = fs.SkipDir
}
