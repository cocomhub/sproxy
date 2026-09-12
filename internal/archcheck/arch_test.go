// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const modulePrefix = "github.com/cocomhub/sproxy/"

// scopeAnchor 是导入图的锚点包：它必然存在、且必然属于根 module 的图。
// 用于挡「图收缩成只含 Managed 包的小子图」——此时 Managed 存在性检查不会响
// （Managed 都在图里），而三条规则已经在缩水的图上跑。
const scopeAnchor = modulePrefix + "pkg/server"

// moduleRoot 返回仓库根目录（本包位于 <root>/internal/archcheck，故上溯两级）。
//
// 必须显式把它作为 go list 的工作目录：go test 以**包目录**为 cwd 运行测试
// 二进制，`go list ./...` 在包目录下只会解析出 internal/archcheck 自身
// （实测 1 个包），三条规则会全部退化为空断言——门禁看似常绿、实则无用。
// 用 runtime.Caller 而非固定相对路径，使结果不依赖调用方 cwd。
//
// 前提：测试**不得以 `-trimpath` 构建/运行**。当前不触发（`-trimpath` 只出现在
// release 的 LDFLAGS，而 archcheck 目标走 `$(RAW_GO) test`，`go env GOFLAGS` 为空）。
// 一旦给测试构建加上 `-trimpath`，runtime.Caller 会返回导入路径形式的相对路径
// （`github.com/cocomhub/sproxy/internal/archcheck`），cmd.Dir 随之非法、go list
// 报红；届时改用固定相对路径 `../..`（go test 保证以包目录为 cwd）。
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位 arch_test.go 源文件路径")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// importGraph 调 `go list` 解析工作区导入图（直接导入，不含测试导入）。
// 用子进程而非 go/build：模块模式下的路径解析交给工具链，避免自己实现一套。
func importGraph(t *testing.T) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}|{{join .Imports \" \"}}", "./...")
	cmd.Dir = moduleRoot(t)
	// CombinedOutput 而非 Output：go list 的真因几乎都在 stderr（缺 go:embed 生成物、
	// 依赖下载失败、go.work 指向缺失目录…）。只报 "exit status 1" 会把每次环境问题
	// 变成一轮往返——这条门禁要在 CI 里跨 8 片反复跑，失败必须自解释。
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list 失败（cwd=%s，需要 go 在 PATH 中）: %v\n--- go list 输出（stdout+stderr）---\n%s",
			cmd.Dir, err, out)
	}
	graph := map[string][]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		pkg := parts[0]
		if len(parts) == 1 || strings.TrimSpace(parts[1]) == "" {
			graph[pkg] = nil
			continue
		}
		graph[pkg] = strings.Fields(parts[1])
	}
	// 防退化：作用域一旦收缩（如上面 cmd.Dir 失效），图里就只剩本包，三条规则
	// 会静默变成空断言。两道检查**缺一不可**：
	//   - Managed 存在性：挡「图塌缩成只剩本包」这类整体失效；
	//   - scopeAnchor 存在性：挡「图收缩成只含 Managed 包的小子图」——此时
	//     Managed 都在，上一条不会响，而规则已在缩水的图上跑。
	for pkg := range Managed {
		if _, ok := graph[pkg]; !ok {
			t.Fatalf("导入图缺少 Managed 包 %s（go list 作用域错误？图中共 %d 个包）", pkg, len(graph))
		}
	}
	if _, ok := graph[scopeAnchor]; !ok {
		t.Fatalf("导入图缺少锚点包 %s（go list 作用域收缩？图中共 %d 个包）", scopeAnchor, len(graph))
	}
	return graph
}

// TestLayeringDirection 断言 R1：已登记包不得导入层级更高的已登记包。
func TestLayeringDirection(t *testing.T) {
	graph := importGraph(t)
	for pkg, imports := range graph {
		own, ok := Levels[pkg]
		if !ok {
			continue
		}
		for _, imp := range imports {
			other, ok := Levels[imp]
			if !ok {
				continue
			}
			if other > own {
				t.Errorf("分层违规：%s(L%d) 导入了 %s(L%d)，低层不得导入高层", pkg, own, imp, other)
			}
		}
	}
}

// isInSubtree 报告 pkg 是否等于 root 或位于 root 子树内。
func isInSubtree(pkg, root string) bool {
	return pkg == root || strings.HasPrefix(pkg, root+"/")
}

// isAssembly 报告 pkg 是否属于装配层（前缀匹配 AssemblyPackages）。
func isAssembly(pkg string) bool {
	for _, a := range AssemblyPackages {
		if isInSubtree(pkg, strings.TrimSuffix(a, "/")) {
			return true
		}
	}
	return false
}

// TestSubpackageVisibility 断言 R2：子包只允许父域子树与装配层导入。
func TestSubpackageVisibility(t *testing.T) {
	graph := importGraph(t)
	for pkg, imports := range graph {
		for _, imp := range imports {
			parent, ok := ParentDomain[imp]
			if !ok {
				continue
			}
			if isInSubtree(pkg, parent) || isAssembly(pkg) {
				continue
			}
			t.Errorf("子包可见性违规：%s 导入了子包 %s，但只有父域 %s 子树与装配层允许导入", pkg, imp, parent)
		}
	}
}

// TestManagedDependenciesRegistered 断言 R3：Managed 包不得导入 pkg/ 下未登记的包。
// 未登记的 pkg/ 包意味着依赖图里出现了一个没声明层级的节点；特别地，它也会拦下
// 「新包反向导入 pkg/server」——那正是本工作最需要防的成环方向。
func TestManagedDependenciesRegistered(t *testing.T) {
	graph := importGraph(t)
	for pkg, imports := range graph {
		if !Managed[pkg] {
			continue
		}
		for _, imp := range imports {
			if !strings.HasPrefix(imp, modulePrefix+"pkg/") {
				continue
			}
			if _, ok := Levels[imp]; !ok {
				t.Errorf("未登记依赖：Managed 包 %s 导入了 %s，它未在 Levels 登记。"+
					"若它是本工作的新包，同时加入 Managed 与 Levels；若是存量包，登记为 L1。", pkg, imp)
			}
		}
	}
}
