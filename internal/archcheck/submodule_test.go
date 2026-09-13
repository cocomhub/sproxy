// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// submodule_test.go 把 R2（子包可见性）与 R4（领域包不得导入装配层）扩展到**子 module**，
// 补上 archcheck 的最后一个已知盲区。
//
// 盲区成因：其余规则基于**根 module** 的 `go list ./...`，而 Go 不会把嵌套 module 纳入父
// module 的模式匹配 ⇒ `cmd/*`、`pkg/tunnel/xfer/ext/*`、`pkg/tunnel/mesh`、
// `pkg/tunnel/hub/ext/kad`、`pkg/telemetry/ext/otel`、`pkg/certmgr/ext/dnspod` 等子 module
// 里的**非测试**包对 R1–R4 完全隐形。实测风险：若 `pkg/tunnel/xfer/ext/ws` 导入
// `pkg/server`（领域包反向依赖装配层），R4 不会响。
//
// 实现选择「源码扫描」而不是「对每个子 module 再跑一次 go list」：后者要为 10 个子 module
// 各做一次模块解析（replace 指回根 module），会把门禁从百毫秒级拖到秒级；而这里要判定的
// 只是「某文件是否导入某个前缀」，**整行 import spec** 的文本匹配已足够精确——
// 注释行以 `//` 开头，不会命中 `"path"` 的整行形态（这一点由正探针兜住）。
package archcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// subModuleImportRe 匹配**整行 import spec**，覆盖 Go 的全部书写形态：
//
//	"path"                // 普通
//	alias "path"          // 具名
//	_ "path"              // 空白导入（实测教训：首版只认整行 `"path"`，探针写成 `_ "path"`
//	. "path"              // 点导入
//	"path" // 行尾注释    // 带注释
//
// 仍**只认整行**：注释里的路径引用（`// … 见 pkg/server/foo.go`）不会命中——这是本扫描
// 相对 go list 的代价，由 TestSubModuleDomainBoundaries 的正探针兜住（必须能在子 module 里
// 观察到 cmd/sproxy 对 pkg/server 的导入）。
var subModuleImportRe = regexp.MustCompile(`^\s*(?:[A-Za-z_.][A-Za-z0-9_.]*\s+)?"(github\.com/cocomhub/sproxy/[^"]+)"\s*(?://.*)?$`)

// subModuleImportSpec 返回该行导入的仓内包路径（非仓内导入 / 非 import spec 行返回 false）。
func subModuleImportSpec(line string) (string, bool) {
	m := subModuleImportRe.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// subModuleRoots 返回全部子 module 的目录（相对仓库根、使用 / 分隔）。
// 排除 build/vendor/.claude：与 Makefile 的 SUB_MODULE_DIRS 同一口径（避免把产物目录
// 里的 go.mod 当成源码 module）。
func subModuleRoots(t *testing.T) []string {
	t.Helper()
	root := moduleRoot(t)
	var mods []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if path != root && (base == "build" || base == "vendor" || base == ".claude" || base == ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		dir := filepath.Dir(path)
		if dir == root {
			return nil // 根 module 由 R1–R4 覆盖
		}
		rel, relErr := filepath.Rel(root, dir)
		if relErr != nil {
			return relErr
		}
		mods = append(mods, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 go.mod 失败: %v", err)
	}
	if len(mods) < 5 {
		t.Fatalf("只找到 %d 个子 module（作用域异常？期望 ≥5）", len(mods))
	}
	return mods
}

// isAssemblyDir 报告相对目录是否属于装配层（与 AssemblyPackages 同一口径，按目录判定）。
func isAssemblyDir(rel string) bool {
	return rel == "cmd" || strings.HasPrefix(rel, "cmd/") ||
		rel == "pkg/server" || strings.HasPrefix(rel, "pkg/server/") ||
		rel == "pkg/client" || strings.HasPrefix(rel, "pkg/client/")
}

// TestSubModuleDomainBoundaries 断言：子 module 的**非测试**包同样遵守 R2 与 R4。
//
//   - R4：不得导入装配层（`pkg/server/**`）——`cmd/` 是装配层例外；
//   - R2：不得导入登记了 ParentDomain 的子包——父域子树与装配层例外。
func TestSubModuleDomainBoundaries(t *testing.T) {
	root := moduleRoot(t)
	scanned := 0
	sawServerFromAssembly := false

	for _, mod := range subModuleRoots(t) {
		modAbs := filepath.Join(root, filepath.FromSlash(mod))
		err := filepath.WalkDir(modAbs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != modAbs && (d.Name() == "build" || d.Name() == "vendor" || d.Name() == ".git") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			importer := filepath.ToSlash(filepath.Dir(rel))
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			scanned++
			for line := range strings.SplitSeq(string(data), "\n") {
				imp, ok := subModuleImportSpec(line)
				if !ok {
					continue
				}
				// R4：领域包不得导入装配层
				if isInSubtree(imp, assemblyRoot) {
					if isAssemblyDir(importer) {
						sawServerFromAssembly = true
						continue
					}
					t.Errorf("分层倒置（子 module）：%s 导入了装配层包 %s。领域包不得依赖装配层——"+
						"需要的类型应下沉到领域包或基础包，由 cmd/ 在装配时注入。", filepath.ToSlash(rel), imp)
					continue
				}
				// R2：子包可见性
				parent, isSub := ParentDomain[imp]
				if !isSub {
					continue
				}
				if isInSubtree(importer, parent) || isAssemblyDir(importer) {
					continue
				}
				t.Errorf("子包可见性违规（子 module）：%s 导入了子包 %s，但只有父域 %s 子树与装配层允许导入",
					filepath.ToSlash(rel), imp, parent)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历子 module %s 失败: %v", mod, err)
		}
	}

	// 正探针 1：扫描面非平凡（防「子 module 列表为空 ⇒ 断言空转」）。
	if scanned < 50 {
		t.Fatalf("只扫描了 %d 个子 module 非测试 .go 文件（期望 ≥50），扫描面疑似收缩", scanned)
	}
	// 正探针 2：`cmd/sproxy` 确实导入 pkg/server——必须被本扫描看到且被装配层例外放行。
	// 它同时证明「整行 import spec 匹配」真的在工作（若匹配器失效，这里会红）。
	if !sawServerFromAssembly {
		t.Fatal("未在子 module 中观察到「装配层导入 pkg/server」——import spec 匹配器疑似失效（门禁在空转）")
	}
}
