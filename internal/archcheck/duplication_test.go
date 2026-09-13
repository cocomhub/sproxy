// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// duplication_test.go 是**重复实现门禁**：把「共享辅助必须走单一事实源」从文档约定变成
// 可执行断言。与同包的 R1–R5（分层/可见性，基于 `go list` 导入图）分工不同：
// 这里扫的是**源码文本**，因为「同一个私有函数被复制到多个包」在导入图上完全隐形。
//
// 为什么需要：这些副本都诞生于「包之间不能互相导入」的合理理由（抽取时把私有依赖一起带上），
// 但理由成立不等于需要各自一份——共享实现应放进 `internal/`，而不是每个包抄一份。
// 没有守卫时，副本只会随每次抽取继续增加（实测：defaultLogger 一度有 5 份）。
package archcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sharedHelperGuards 是「不得在 pkg/ 下重新定义」的共享辅助清单。
//
// 每项 = (被禁止的本地定义签名, 应使用的共享实现, 说明)。
var sharedHelperGuards = []struct {
	// signature 是本地定义的函数签名前缀（去空白后精确匹配）。
	signature string
	// replacement 是应当改用的共享实现（用于失败信息）。
	replacement string
	// why 说明「为什么必须单一事实源」（写进失败信息，避免后人误判为洁癖）。
	why string
}{
	{
		signature:   "func defaultLogger(",
		replacement: "internal/slogutil.Default",
		why: "nil logger 归一（nil → slog.Default()）必须单一事实源：它决定「未配置日志器」时" +
			"落盘/落 stdout 的行为，多处副本一旦分叉（如某处改成 Discard 或改 Level）就会出现" +
			"同一进程内不同组件日志去向不一致，且只在 nil logger 的边界场景显形。",
	},
}

// moduleDirs 返回根 module 下 `./pkg/...` 各包的目录（绝对路径）。
//
// **不返回子 module 的目录**：Go 的模式匹配不会下探嵌套 module，故
// `pkg/tunnel/hub/ext/kad`（独立 module，其 defaultLogger 语义不同：返回 Discard logger）
// 不在其中——这是本门禁「只约束根 module」的实现方式，用例里的正探针会核验这一点。
func moduleDirs(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", "./pkg/...")
	cmd.Dir = moduleRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list ./pkg/... 失败（cwd=%s）: %v\n%s", cmd.Dir, err, out)
	}
	var dirs []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			dirs = append(dirs, line)
		}
	}
	if len(dirs) < 10 {
		t.Fatalf("go list ./pkg/... 只返回 %d 个目录（作用域异常？）", len(dirs))
	}
	return dirs
}

// TestNoSharedHelperDuplication 断言 sharedHelperGuards 里的辅助在 pkg/ 下**没有本地定义**。
// 扫全部 .go（含 _test.go）：测试文件里复制一份同样会让「两份实现」成立。
func TestNoSharedHelperDuplication(t *testing.T) {
	dirs := moduleDirs(t)
	scanned := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读取目录 %s 失败: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读取 %s 失败: %v", path, err)
			}
			scanned++
			for line := range strings.SplitSeq(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				for _, g := range sharedHelperGuards {
					if !strings.HasPrefix(trimmed, g.signature) {
						continue
					}
					rel, relErr := filepath.Rel(moduleRoot(t), path)
					if relErr != nil {
						rel = path
					}
					t.Errorf("重复实现：%s 定义了 %q，应改用共享实现 %s。\n理由：%s",
						filepath.ToSlash(rel), g.signature, g.replacement, g.why)
				}
			}
		}
	}
	// 正探针：扫描面必须非平凡（防「目录列表为空 ⇒ 断言空转」）。
	if scanned < 20 {
		t.Fatalf("只扫描了 %d 个 .go 文件，扫描面疑似收缩（门禁可能空转）", scanned)
	}
}

// TestModuleDirsExcludesNestedModule 是上面门禁的**正探针**（防「作用域过大/过小」误判）：
// 内嵌 module `pkg/tunnel/hub/ext/kad` 必须被排除——它的 defaultLogger 语义不同
// （返回 Discard logger，用于 DHT 持久化的损坏告警），不在本门禁约束内。
// 若某天 Go 的 ./... 语义变化把嵌套 module 纳入，本用例会红，提示重新评估该排除。
func TestModuleDirsExcludesNestedModule(t *testing.T) {
	nested := filepath.ToSlash(filepath.Join("pkg", "tunnel", "hub", "ext", "kad"))
	for _, dir := range moduleDirs(t) {
		if strings.HasSuffix(filepath.ToSlash(dir), nested) {
			t.Fatalf("嵌套 module %s 被纳入扫描面：其 defaultLogger 语义不同（Discard），"+
				"不应受 sharedHelperGuards 约束——要么调整共享签名，要么显式排除该目录", nested)
		}
	}
}
