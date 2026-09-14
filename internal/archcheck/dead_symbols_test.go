// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// dead_symbols_test.go 是「**已确认删除的死代码不得复活**」的墓碑门禁（R11）。
//
// 触发背景：2026-09-14 的死代码审计发现，仓库里积累了一批「生产调用已被删除、函数与测试
// 仍在养着它」的遗留符号——最典型的是 #90「清理死代码」那次提交删掉了 runBatchOperation 的
// 调用行却保留了函数体与测试，此后无人再碰。这类代码不会自己消失，只会误导阅读者。
//
// 判据（结构性，不做语义猜测）：以下符号不得以**词边界**形式出现在任何**非测试**源码中。
// 词边界（而非固定子串）是必须的：`startMeshNodeRole` 是 `startMeshNodeRoleWithCreds` 的前缀，
// 固定子串匹配会在删掉包装后依然命中合法函数。每个条目都对应一次有 git 取证的清理，证据见
// docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md §2.1。
// 允许出现在 `_test.go` 中（例如把旧调用点改写为规范入口的对照断言）。
//
// 扫描实现（2026-09-14 由 `git grep` 改为纯 Go 目录遍历）：① 不再依赖 .git 工作树——tarball /
// 无 .git 的构建上下文不再假红；② 覆盖**未跟踪的新文件**（`git grep` 只搜已跟踪文件，而新文件
// 恰是最可能夹带复活符号的场景）；③ 不再需要显式 cwd 与 pathspec（首版曾因未指定 cwd 只搜本包、
// 门禁假绿）。遍历范围 = moduleRoot 之下的非隐藏目录中、非 `_test.go` 的 .go 文件。
//
// 为什么不直接用 deadcode 工具当门禁：deadcode 不带 -test 时会把「仅被测试引用」的 helper
// （NewMock/DiscardLogger/SetHostOnly 等）一并报为不可达，输出永不为空，无法作为失败条件；
// 带 -test 时又会把上面这些真正该删的符号也算作可达。所以用一份显式的墓碑清单来守。
// （单独的可达性门禁见 `make deadcode-check` 与 gate_wiring_test.go。）

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// deadSymbols 是 2026-09-14 审计确认删除的历史遗留符号（逐条附删除依据）。
var deadSymbols = []string{
	// 生产调用被 client.FileClient.Archive 取代，仅剩测试引用。
	"writeArchiveResponse",
	// 生产调用被 batch_delete.go / batch_rename.go 的内联循环取代，仅剩测试引用。
	"runBatchOperation",
	// 生产调用被 download-archive（下载原始归档，不在本地解压）取代，仅剩测试引用。
	"extractTarGz",
	// 自 S5 引入起 root.go 就直接调用 startMeshNodeRoleWithCreds，该包装从未接过线。
	"startMeshNodeRole",
	// tunnel_key 已废除、handleSighup 不再热替换密钥，UpdateKey 全仓零调用。
	"TunnelUpdater",
	// 同次清理：Handlers.TunnelHandler() 访问器与 h.tunnelHandler 字段同义，删除后仅由字段担 POST /tunnel 路由；
	// 该名字不通用（仅 root.go 一条注释曾提及），故可入墓碑。
	"TunnelHandler",
	// 无任何实现断言或消费方的空接口（protoc 生成后才会出现的 Xfer_StreamServer；当前为手写骨架接口）。
	"XferServer",
}

// scanDeadSymbol 在 root 下遍历非测试 .go 文件，以**词边界**匹配 sym，
// 返回 "相对路径:行号: 行内容" 形式的命中列表（按遍历顺序）。
func scanDeadSymbol(root, sym string) ([]string, error) {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(sym) + `\b`)
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// 跳过 VCS/工具元数据、构建产物与依赖目录。隐藏目录一律跳过：它们不是产品
			// 源码，且可能含大体积拷贝（.git/.superpowers/.claude 等）。
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "build" || d.Name() == "vendor") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	return hits, err
}

func TestNoResurrectedDeadSymbols(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	for _, sym := range deadSymbols {
		hits, err := scanDeadSymbol(root, sym)
		if err != nil {
			t.Fatalf("扫描 %q 失败: %v", sym, err)
		}
		if len(hits) > 0 {
			t.Errorf("已删除符号 %q 复活（非测试源码命中）：\n%s", sym, strings.Join(hits, "\n"))
		}
	}
}

// TestScanDeadSymbol_ScopeAndWordBoundary 是上面门禁的**自检**：门禁本身也会坏（首版就因
// 未指定 cwd 只搜了本包、看似常绿），故用临时目录固定六条口径：命中/未跟踪新文件计入/
// 前缀名不算命中/测试文件排除/隐藏目录排除/构建产物目录排除。
func TestScanDeadSymbol_ScopeAndWordBoundary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("建目录 %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s: %v", rel, err)
		}
	}
	const sym = "writeArchiveResponse"
	write("pkg/prod.go", "func "+sym+"() {}\n")
	write("pkg/untracked_new.go", "// 未跟踪新文件里也不得残留\n"+sym+"()\n")
	write("pkg/prod_test.go", sym+"()\n")
	write("pkg/prefix.go", "func "+sym+"Extra() {}\n")
	write(".git/objects/blob.go", sym+"()\n")
	write("build/gen.go", sym+"()\n")

	hits, err := scanDeadSymbol(root, sym)
	if err != nil {
		t.Fatalf("scanDeadSymbol: %v", err)
	}
	joined := strings.Join(hits, "\n")
	if !strings.Contains(joined, "pkg/prod.go:1: ") {
		t.Errorf("应命中 pkg/prod.go:\n%s", joined)
	}
	if !strings.Contains(joined, "pkg/untracked_new.go:2: ") {
		t.Errorf("应命中未跟踪新文件（git grep 会漏的口径）：\n%s", joined)
	}
	if strings.Contains(joined, "prod_test.go") {
		t.Errorf("测试文件必须排除：\n%s", joined)
	}
	if strings.Contains(joined, "prefix.go") {
		t.Errorf("前缀名（无词边界）不得命中：\n%s", joined)
	}
	if strings.Contains(joined, ".git/") || strings.Contains(joined, "build/gen.go") {
		t.Errorf("隐藏目录/构建产物必须排除：\n%s", joined)
	}
	if len(hits) != 2 {
		t.Errorf("命中数 = %d, want 2：\n%s", len(hits), joined)
	}
}
