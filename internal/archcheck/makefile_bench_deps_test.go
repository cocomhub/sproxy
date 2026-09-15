// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMakefileTargetsCompilingAllNeedPrepare 门禁：任何会编译/类型检查根 module 的入口
// 都必须先产出 buildmeta 的 embed 依赖（build/dirty_info.txt）。
//
// 背景（两次同源事故）：
//   - CI run 34939978058：`bench` 缺 `prepare` ⇒ internal/buildmeta 的
//     //go:embed build/dirty_info.txt 在干净 checkout 下缺失 ⇒ 编译失败；又因 Benchmark job
//     有 timeout 兜底，表面现象是「Benchmark 超时」，真实根因是缺前置目标。
//   - Release run 34958665107（tag v0.11.1）：**GoReleaser 是非 make 的编译路径**，直接编译
//     ⇒ `internal/buildmeta/buildmeta.go:14:12: pattern build/dirty_info.txt: no matching
//     files found` ⇒ 发布失败、制品缺失。
//
// 判据（两段，均需满足）：
//  1. Makefile：recipe 中会编译/类型检查**本仓发布包**（go build/test/run/vet、golangci-lint，
//     且目标路径落在 ./... ./cmd ./pkg ./internal ./test）的目标必须显式依赖 `prepare`。
//     排除 `go run tools/...`（工具程序不 import buildmeta）与 `go install <pkg>@<ver>`
//     （外部 module，无本仓路径）。
//  2. `.goreleaser.yaml`：必须有 `before.hooks` 且其中生成 dirty_info（GoReleaser 不走
//     Makefile，故 Makefile 依赖无法覆盖它）。
func TestMakefileTargetsCompilingAllNeedPrepare(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	// ---- 断言 1：Makefile 编译/类型检查类目标必须依赖 prepare ----
	targets := parseMakefileTargets(t, root)
	var offenders []string
	checked := 0
	for _, g := range targets {
		var runnable []string
		for _, r := range g.recipes {
			// 排除纯 echo 的用法说明（`help` 目标会把 `go test ./...` 当文案打印，误判为编译）。
			if strings.HasPrefix(r, "@echo") || strings.HasPrefix(r, "echo") {
				continue
			}
			runnable = append(runnable, r)
		}
		body := strings.Join(runnable, " ")
		if !touchesRepoPackages(body) {
			continue
		}
		checked++
		if !hasPrepareDep(g.deps) {
			offenders = append(offenders, fmt.Sprintf("  %s: deps=[%s]", g.name, g.deps))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("以下目标会编译/类型检查根 module 但未依赖 `prepare` ⇒ internal/buildmeta 的 "+
			"//go:embed build/dirty_info.txt 在干净 checkout 下缺失（CI Benchmark 与 v0.11.1 Release "+
			"均因此失败）；请加上 `prepare` 前置：\n%s", strings.Join(offenders, "\n"))
	}
	if checked == 0 {
		t.Fatal("门禁自检失败：未匹配到任何「编译/类型检查」目标（判据失效，需更新本测试）")
	}

	// ---- 断言 2：GoReleaser 编译前必须生成 embed 文件 ----
	gr, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("读取 .goreleaser.yaml: %v", err)
	}
	before := yamlTopLevelSection(string(gr), "before")
	if before == "" {
		t.Fatal(".goreleaser.yaml 缺少 `before:` 段：GoReleaser 是非 make 编译路径，必须在 " +
			"before.hooks 里生成 buildmeta embed 文件（否则干净 checkout 下报 " +
			"`pattern build/dirty_info.txt: no matching files found`）")
	}
	if !strings.Contains(before, "hooks:") {
		t.Fatalf(".goreleaser.yaml 的 `before:` 段缺少 `hooks:`：%q", before)
	}
	if !strings.Contains(before, "prepare") && !strings.Contains(before, "dirty_info") {
		t.Fatalf(".goreleaser.yaml 的 before.hooks 未生成 buildmeta embed 文件（应调用 `make prepare` "+
			"或写入 dirty_info）：%q", before)
	}
}

type makeTarget struct {
	name    string
	deps    string
	recipes []string
}

func parseMakefileTargets(t *testing.T, root string) []makeTarget {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("读取 Makefile: %v", err)
	}
	var targets []makeTarget
	cur := -1
	for ln := range strings.SplitSeq(string(b), "\n") {
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if !strings.HasPrefix(ln, "\t") && strings.Contains(ln, ":") && !strings.HasPrefix(ln, ".") {
			parts := strings.SplitN(ln, ":", 2)
			targets = append(targets, makeTarget{name: strings.TrimSpace(parts[0]), deps: strings.TrimSpace(parts[1])})
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
	return targets
}

// touchesRepoPackages 判断一条（已拼接的）recipe 是否会编译/类型检查本仓发布包——即会构建
// internal/buildmeta，因此需要 build/dirty_info.txt 先行存在。
func touchesRepoPackages(body string) bool {
	// 归一化 go 命令变量：$(GO) / $(RAW_GO) → go，避免因变量名差异漏判（vet/test-all/build-all
	// 用的都是 $(RAW_GO)，旧判据对它们完全盲）。
	norm := strings.NewReplacer("$(GO)", "go", "$(RAW_GO)", "go").Replace(body)
	// golangci-lint 会对其作用域内的包做类型检查，缺 embed 文件即报错；子 module lint 也可能
	// 经 replace 链编译到根 module，故一律要求先 prepare（prepare 幂等且开销极低）。
	if strings.Contains(norm, "golangci-lint") {
		return true
	}
	verbs := []string{"go test", "go build", "go run", "go vet"}
	matched := false
	for _, v := range verbs {
		if strings.Contains(norm, v) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	// 仅当命令指向本仓包路径时才算：`go run tools/gencoverview/main.go` 与
	// `go install github.com/google/addlicense@latest` 都不构建本仓发布包。
	scopes := []string{"./...", "./cmd/", "./pkg/", "./internal/", "./test/", " cmd/sproxy", " cmd/sclient"}
	for _, s := range scopes {
		if strings.Contains(norm, s) {
			return true
		}
	}
	return false
}

func hasPrepareDep(deps string) bool {
	return deps == "prepare" || strings.Contains(" "+deps+" ", " prepare ")
}

// yamlTopLevelSection 返回顶层键 key 的原文片段（到下一个顶层键为止），
// 以免引入 YAML 依赖地校验 .goreleaser.yaml 的局部结构。
func yamlTopLevelSection(src, key string) string {
	var out []string
	in := false
	for ln := range strings.SplitSeq(src, "\n") {
		top := ln != "" && !strings.HasPrefix(ln, " ") && !strings.HasPrefix(ln, "\t")
		if top && strings.HasPrefix(ln, key+":") {
			in = true
			out = append(out, ln)
			continue
		}
		if in {
			if top {
				break
			}
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}
