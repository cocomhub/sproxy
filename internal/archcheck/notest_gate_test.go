// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// notest_gate_test.go 是「**`make notest` 门禁自身可用性**」的守卫（R17）。
//
// 背景（2026-09-14 实测发现，两道缺陷叠加 ⇒ 门禁从上线起就**从未生效**）：
//  1. Makefile 的 `notest` 目标不带参数调用 `scripts/check-test-files.sh`，脚本 `for pkg in "$@"`
//     零次迭代 ⇒ 直接打印 `OK: all packages have test files` —— 无论仓库里有多少包没测试都绿；
//  2. 即使补上参数，忽略清单的实现也是反的：用 `find -not -path` 匹配，被忽略时输出为**空**，
//     反而被判为「未忽略」，于是 `.notestignore` 从未生效（补参后立刻误报 4 个 tools 包）。
//
// 修好后立刻发现了 3 个真实缺口（`pkg/sync/internal/fsutil`、`pkg/testutil/syncmock` 缺测试、
// `build/` 产物目录被当成包）。这类「门禁空转」不会有人报错，只能靠断言钉住。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotestGate_WiredAndFailsClosed(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	read := func(rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("读 %s: %v", rel, err)
		}
		return string(data)
	}

	// 1) 调用方必须把包列表传进去（否则脚本空转）
	recipe := makefileTargetRecipe(t, "notest")
	if !strings.Contains(recipe, "check-test-files.sh") {
		t.Fatalf("notest 配方未调用 check-test-files.sh:\n%s", recipe)
	}
	if !strings.Contains(recipe, "list ./...") {
		t.Errorf("notest 必须把 `go list ./...` 的结果作为参数传给脚本（不带参数 = 门禁空转）：\n%s", recipe)
	}

	// 2) 脚本必须 fail-closed：没有参数时不能打印 OK
	script := read("scripts/check-test-files.sh")
	if !strings.Contains(script, "$# -eq 0") || !strings.Contains(script, "exit 1") {
		t.Error("check-test-files.sh 必须对「空参数」显式失败（门禁不得空转）")
	}
	// 3) 忽略清单必须是正向匹配（历史缺陷：find -not -path 反向实现，清单从未生效）
	if !strings.Contains(script, "IGNORE_PATTERNS") {
		t.Error("check-test-files.sh 的忽略清单应按 glob 正向匹配（原实现反向，导致 .notestignore 失效）")
	}

	// 4) CI 必须真的调用它（只修本地目标 = 纸面门禁）
	ci := read(".github/workflows/ci.yml")
	if !strings.Contains(ci, "make notest") {
		t.Error("CI 未调用 make notest（门禁未接线 ⇒ 永不生效）")
	}

	// 5) build/ 产物目录必须免检（`go list ./...` 会把 build/ 下的临时包算进来）
	ignore := read(".notestignore")
	if !strings.Contains(ignore, "build/*") {
		t.Error(".notestignore 需登记 build/*（构建产物目录不是源码，不该被要求有测试）")
	}
}
