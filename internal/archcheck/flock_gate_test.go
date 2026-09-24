// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// flock_gate_test.go 是「直接 flock 调用不得用于代码」的门禁（R23）。
//
// 背景（roadmap 11.11 / docs/designs/2026-09-24-leader-elector.md §5.4）：
// 多节点共享存储需要**写面唯一**，选主统一经 LeaderElector 抽象
// （pkg/leader）——LocalLeaderElector 内部用排他文件锁（Unix flock /
// Windows LockFileEx），其它代码若绕过抽象直接 flock 自造「文件锁选主」，
// 会与 LeaderElector 的租约/续租/写面门语义分叉（无 ErrLeaseLost、无
// WriteGuard 降级、无可观测日志），多节点下静默双写。
//
// R23 判据：**非测试**源码禁止直接调用 `syscall.Flock` /
// `golang.org/x/sys/unix.Flock`（排除 _test.go——测试可对锁原语做平台级
// 断言；豁免 pkg/leader 自身实现与门禁自身文件）。
//
// 存量豁免：pkg/server/flock_unix.go（lockFile/unlockFile 内部文件锁原语，
// 供 acquireFileLock 文件级互斥/带宽协调用——非选主语义，key 是 owner+rel
// 而非全局写面；2026-09-24 盘库实证非测试源码中仅此两处调用点）。
//
// 扫描实现与 R19（http_transport_gate_test.go）同模式：纯 Go 目录遍历
// moduleRoot 之下的非隐藏目录、非 _test.go 的 .go 文件。
// 词边界匹配（`\bsyscall\.Flock\b`）防误伤注释/字符串中的描述性提及。

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// flockCallRe 匹配直接 flock 调用（词边界：仅 `syscall.Flock(` 与
// `unix.Flock(` 两种真实调用形态——golang.org/x/sys/unix 无 Flock 符号，
// 该分支保留以覆盖未来平台文件扩展后出现的调用）。
var flockCallRe = regexp.MustCompile(`\bsyscall\.Flock\s*\(|\bunix\.Flock\s*\(`)

// scanFlockCalls 遍历 root 下非测试 .go 文件，返回「相对路径:行号: 行内容」
// 形态的命中列表。豁免 pkg/leader 自身实现与门禁自身文件。
func scanFlockCalls(root string) ([]string, error) {
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root && repoScanSkipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		// 豁免 pkg/leader 自身实现（LockFileEx/flock 的正当使用点）、门禁自身、
		// 与存量 pkg/server/flock_unix.go（内部文件锁原语，非选主语义）。
		if strings.HasPrefix(rel, "pkg/leader/") || rel == "internal/archcheck/flock_gate_test.go" ||
			rel == "pkg/server/flock_unix.go" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(data), "\n") {
			if flockCallRe.MatchString(line) {
				hits = append(hits, rel+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	return hits, err
}

// itoa 避免引入 strconv（门禁内联小整数转换）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestNoDirectFlockOutsideLeader 扫描生产源码禁直接 flock 调用（引导走 LeaderElector）。
func TestNoDirectFlockOutsideLeader(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	hits, err := scanFlockCalls(root)
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("非测试源码不得直接调用 flock——选主/写面唯一化必须走 pkg/leader 的 "+
			"LeaderElector 抽象（本地实现已封装 Unix flock / Windows LockFileEx，含租约/续租/"+
			"ErrLeaseLost/WriteGuard 语义）：\n%s", strings.Join(hits, "\n"))
	}
}

// TestScanFlockCalls_Scope 钉住门禁扫描口径（R13 风格防静默退化）：
// 命中生产文件 / 未跟踪新文件计入；测试文件排除；pkg/leader 豁免；隐藏目录排除；
// 前缀名（FlockExtra）不算命中（词边界）。
func TestScanFlockCalls_Scope(t *testing.T) {
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
	write("pkg/prod.go", "func f() { syscall.Flock(1, 2) }\n")
	write("pkg/untracked_new.go", "// 未跟踪新文件里也不得直接 flock\nunix.Flock(3, 4)\n")
	write("pkg/prod_test.go", "syscall.Flock(1, 2)\n")
	write("pkg/leader/local.go", "syscall.Flock(1, 2) // 豁免：LeaderElector 自身实现\n")
	write("pkg/prefix.go", "func FlockExtra() {}\n")
	write(".git/objects/blob.go", "syscall.Flock(1, 2)\n")
	write("build/gen.go", "syscall.Flock(1, 2)\n")

	hits, err := scanFlockCalls(root)
	if err != nil {
		t.Fatalf("scanFlockCalls: %v", err)
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
	if strings.Contains(joined, "pkg/leader/") {
		t.Errorf("pkg/leader 自身实现必须豁免：\n%s", joined)
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
