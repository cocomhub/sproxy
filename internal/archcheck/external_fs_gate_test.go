// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// external_fs_gate_test.go 是「外部卷后端 FS 直连点」门禁（R27）。
//
// 为什么需要：外部/远端卷没有 `*storage.Root` ⇒ 封装卷封印只能套在**装配处**的
// `be.FS()` 上（`pkg/volume/enclosure.SealBackend`）。每多一处直接取 `be.FS()`，就多一个
// 潜在绕过点；新增而忘记评估 = **安全边界静默失效**（读走密文/写入明文破坏 at-rest 语义）。
// 本门禁把「直连点清单」机制化：`external_fs_calls.tsv` 登记每文件允许的最大次数（只降不升），
// 新增文件或增加次数必须显式登记理由——把「评审记得看」变成「CI 必须过」。
//
// 变异：在任一未登记文件里加一行 `_ = be.FS()` → 本门禁红。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// externalFSCallsFile 是登记表文件名（internal/archcheck/ 下）。
const externalFSCallsFile = "external_fs_calls.tsv"

// countFSCalls 统计文件内 `.FS()` 出现次数（与登记口径一致：文本计数，偏严不偏松）。
func countFSCalls(data string) int {
	return strings.Count(data, ".FS()")
}

// loadExternalFSCalls 解析登记表（file → max_calls）。
func loadExternalFSCalls(t *testing.T) map[string]int {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "internal", "archcheck", externalFSCallsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", externalFSCallsFile, err)
	}
	out := map[string]int{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 3 {
			t.Fatalf("%s 行格式应为 file<TAB>max_calls<TAB>reason: %q", externalFSCallsFile, line)
		}
		n, cerr := strconv.Atoi(strings.TrimSpace(cols[1]))
		if cerr != nil {
			t.Fatalf("%s 次数非法 %q: %v", externalFSCallsFile, cols[1], cerr)
		}
		out[strings.TrimSpace(cols[0])] = n
	}
	return out
}

// TestExternalFSCallsRegistered 钉住全部直连点已登记且次数只降不升。
func TestExternalFSCallsRegistered(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	registered := loadExternalFSCalls(t)
	// 判据非空自证（解析失效时不得静默通过）。
	if len(registered) < 5 {
		t.Fatalf("登记表条目过少（%d）——判据可能失效", len(registered))
	}
	if registered["pkg/volume/enclosure/sealfs.go"] == 0 {
		t.Fatal("登记表必须含封印装饰器自身（哨兵条目）——防解析失效")
	}

	var problems []string
	seen := map[string]bool{}
	for _, dir := range []string{"pkg", "cmd", "internal"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // 遍历容错：不可读目录不影响门禁结论
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil //nolint:nilerr // 同上
			}
			n := countFSCalls(string(data))
			if n == 0 {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil //nolint:nilerr // 同上
			}
			rel = filepath.ToSlash(rel)
			seen[rel] = true
			max, ok := registered[rel]
			if !ok {
				problems = append(problems, rel+"（未登记，次数 "+strconv.Itoa(n)+"）")
				return nil
			}
			if n > max {
				problems = append(problems, rel+"（次数 "+strconv.Itoa(n)+" > 登记 "+strconv.Itoa(max)+"）")
			}
			return nil
		})
	}
	if len(problems) > 0 {
		t.Fatalf("发现未登记/次数上升的外部卷 FS 直连点（R27：必须评估封印影响并在 %s 登记）:\n  %s",
			externalFSCallsFile, strings.Join(problems, "\n  "))
	}
	// 反向核查：登记表不得留下已消失的条目（避免表与实际漂移）。
	for file, max := range registered {
		if !seen[file] && max > 0 {
			t.Logf("提示：登记表条目 %s 已无 .FS() 调用（可下调/删除该行）", file)
		}
	}
}
