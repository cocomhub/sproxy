// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// chunked_persist_serialization_test.go 门禁：分块会话的**持久化入口**必须全部经会话内嵌串行锁
// （session.persistMu），否则「拍快照 → 落盘」两阶段可能交错 ⇒ 旧快照晚落盘覆盖新快照 ⇒ 重启丢分块
// （RV10-SLICE3 审计的既有缺陷，修复见 pkg/files/chunked_store.go 的 persistSnapshot）。
//
// 锁为何内嵌会话对象（v2 演进）：初版用 UploadStore 级 map（persistLocks）在会话删除后**永不回收**
// ⇒ 长期运行内存随已删除会话数无限增长（每个 ~56 B）。改为会话对象自带的 *sync.Mutex（newSession
// 初始化、随会话 GC 回收）⇒ 无泄漏；同一会话的并发持久化（对象相同）⇒ 同一把锁 ⇒ 串行；跨会话
// （同 id 新会话）⇒ 新锁新对象，旧持久化拍已删除会话快照会被 writeSessionJSON 的目录缺失 fail-closed。
//
// 为什么用门禁而非仅靠并发测试：并发乱序缺陷的测试是**概率性**的（需要两个持久化恰好交错，
// 修复后锁使交错结构性不可能——同一会话的持久化完全串行，任何「在锁内挂门制造交错」的测试
// 都会死锁，见 chunked_persist_order_test.go 的注释）。因此「把锁去掉」不会让并发测试确定性红
// ⇒ 用**源码级门禁**钉住「任何持久化入口都必须持锁」。
//
// 判据：pkg/files/chunked_store.go 的非测试源码里，函数 persistSession / PersistNow /
// PersistNowIfCurrent 的函数体内**必须**出现 `.persistMu.Lock()`（持会话内嵌串行锁）；其余任何
// 函数体内不得出现 `us.writeSessionJSON(` 的直接调用（所有持久化都经 persistSnapshot 间接调用，
// persistSnapshot 本身由三入口在锁内调用）——新增持久化入口若绕过锁会在此红。
import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	rePersistFn = regexp.MustCompile(`(?m)^func \(us \*UploadStore\) (persistSession|PersistNow|PersistNowIfCurrent)\(`)
	reLockCall  = regexp.MustCompile(`\.persistMu\.Lock\(\)`)
	reWriteCall = regexp.MustCompile(`us\.writeSessionJSON\(`)
)

// TestChunkedPersistEntriesSerialized 断言三个持久化入口都在 per-id 串行锁内。
func TestChunkedPersistEntriesSerialized(t *testing.T) {
	t.Parallel()

	src := readFileString(t, filepath.Join(moduleRoot(t), "pkg", "files", "chunked_store.go"))

	// 收集三个入口的函数体范围（从 func 行到下一个顶层 func 行）。
	lines := strings.Split(src, "\n")
	var fnRanges [][2]int // [start,end) 行号（1 基）
	for i, ln := range lines {
		if rePersistFn.MatchString(ln) {
			start := i + 1
			end := len(lines) + 1
			for j := i + 1; j < len(lines); j++ {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), "func ") {
					end = j + 1
					break
				}
			}
			fnRanges = append(fnRanges, [2]int{start, end})
		}
	}
	if len(fnRanges) != 3 {
		t.Fatalf("应观察到 3 个持久化入口（persistSession/PersistNow/PersistNowIfCurrent），实测 %d（判据失效）", len(fnRanges))
	}

	// 正探针：persistMu 字段定义必须存在（否则“三入口都持锁”可能因字段不存在而假绿）。
	if !strings.Contains(src, "persistMu *sync.Mutex") {
		t.Fatal("未观察到 ChunkedUploadSession.persistMu 字段（会话内嵌串行锁），判据失效")
	}

	for _, r := range fnRanges {
		body := strings.Join(lines[r[0]-1:r[1]-1], "\n")
		if !reLockCall.MatchString(body) {
			t.Fatalf("持久化入口必须经 per-id 串行锁（lockPersistID）——%s 函数体（行 %d-%d）未调用：\n%s",
				lineFuncName(lines[r[0]-1]), r[0], r[1]-1, body)
		}
	}

	// 反向探针：任何非三入口的顶层函数不得直接调 writeSessionJSON（所有**持久化**都经 persistSnapshot）。
	// 豁免：saveNewSession / GetOrCreateSession 的**会话创建**路径（首次写 session.json，写的是
	// 全新会话、无并发持久化竞争，且调用方保证不在持锁状态）——它们不经 per-id 锁是有意的。
	for i, ln := range lines {
		if reWriteCall.MatchString(ln) && !strings.Contains(ln, "func ") {
			inEntry := false
			for _, r := range fnRanges {
				if i+1 >= r[0] && i+1 < r[1] {
					inEntry = true
					break
				}
			}
			inCreate := isInFuncNamed(lines, i+1, "saveNewSession", "GetOrCreateSession", "persistSnapshot")
			if !inEntry && !inCreate {
				t.Fatalf("第 %d 行在持久化入口之外直接调用 writeSessionJSON（应经 persistSnapshot 在锁内调用）:\n%s", i+1, ln)
			}
		}
	}
}

// readFileString 读文件为字符串（t.Fatalf 失败）。
func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}

// lineFuncName 从一行提取函数名（门禁消息用）。
func lineFuncName(line string) string {
	for seg := range strings.FieldsSeq(line) {
		if strings.Contains(seg, "(") && !strings.HasPrefix(seg, "func") && !strings.HasPrefix(seg, "*") {
			return seg
		}
	}
	return line
}

// isInFuncNamed 返回行号 lineNo（1 基）是否落在指定函数（按名字）的函数体内。
func isInFuncNamed(lines []string, lineNo int, names ...string) bool {
	for i, ln := range lines {
		if !strings.HasPrefix(strings.TrimSpace(ln), "func ") {
			continue
		}
		fnName := ""
		if _, after, ok := strings.Cut(ln, "func "); ok {
			rest := after
			// 跳过接收者（`(recv) Name(`）或裸函数（`Name(`）：取「最后一个 `)` 之后」的名字。
			if strings.HasPrefix(rest, "(") {
				if cp := strings.Index(rest, ") "); cp > 0 {
					fnName = strings.TrimSpace(rest[cp+2:])
					if sp := strings.IndexAny(fnName, "("); sp > 0 {
						fnName = fnName[:sp]
					}
				}
			} else if sp := strings.IndexAny(rest, " ("); sp > 0 {
				fnName = rest[:sp]
			}
		}
		matched := false
		for _, n := range names {
			if strings.HasSuffix(fnName, n) || fnName == n {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		start := i + 1
		end := len(lines) + 1
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "func ") {
				end = j + 1
				break
			}
		}
		if lineNo >= start && lineNo < end {
			return true
		}
	}
	return false
}
