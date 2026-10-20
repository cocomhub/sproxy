// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// waitq_gates_test.go 汇总 W1 收敛的两条门禁（设计 docs/designs/2026-10-19-waitq.md §8.3）：
//
//	R36-a：非测试源码不得**新增**直接调用 TryReserve / WaitReserve / TryReserveChunked /
//	       semaphore.Weighted —— 新增获取路径必须走 pkg/waitq（Acquire/AcquireUnder），
//	       或登记到白名单适配器文件。基线见 quota_acquire_sites.tsv（棘轮：只减不增）。
//	R36-c：waitq.Resource **适配器必须登记**（资源名 / label / 单位 / 超时 / MaxWaiters /
//	       优先级），见 waitq_resources.tsv —— 否则无法统一评估超时与队列上界。
//
// 扫描口径 = **根 module**（下探遇到自带 go.mod 的嵌套 module 即停，与 R36-b 一致）。

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// waitqScanSkipDirs 是永不扫描的目录。
var waitqScanSkipDirs = map[string]bool{
	".git": true, ".worktrees": true, "build": true, "node_modules": true, "vendor": true,
}

// waitqGoFiles 返回根 module 下 pkg/cmd/internal 的非测试 Go 源码（相对路径）。
func waitqGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	var walk func(rel string)
	walk = func(rel string) {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		entries, err := os.ReadDir(abs)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			child := rel + "/" + name
			if e.IsDir() {
				if waitqScanSkipDirs[name] {
					continue
				}
				if _, statErr := os.Stat(filepath.Join(abs, name, "go.mod")); statErr == nil {
					continue // 嵌套 module 不在根 module 构建面内
				}
				walk(child)
				continue
			}
			if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
				out = append(out, child)
			}
		}
	}
	for _, d := range []string{"pkg", "cmd", "internal"} {
		walk(d)
	}
	return out
}

// readTSVRows 读取 TSV（跳过注释/空行），返回每行的字段切片。
func readTSVRows(t *testing.T, name, sep string) [][]string {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatalf("读取 %s: %v", name, err)
	}
	defer f.Close()
	var rows [][]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rows = append(rows, strings.Split(line, sep))
	}
	return rows
}

// quotaAcquireRe 匹配配额/容量获取入口（与生成基线时的口径一致；注释行不算）。
// 口径说明：**只禁「直接」获取**（TryReserve* / semaphore.Weighted）；`WaitReserve` 是
// **收敛后的入口**（内部即 waitq.AcquireUnder + 根等待锁）⇒ 允许，且它是迁移的目标形态。
var quotaAcquireRe = regexp.MustCompile(`[.](TryReserve|semaphore[.]Weighted)`)

// TestR36a_NoNewDirectAcquirePaths 棘轮：新增「直接获取」调用点即红（必须先收敛到 waitq
// 或登记到白名单适配器文件）。
func TestR36a_NoNewDirectAcquirePaths(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	base := map[string]int{}
	for _, row := range readTSVRows(t, "quota_acquire_sites.tsv", "	") {
		if len(row) < 2 {
			t.Fatalf("quota_acquire_sites.tsv 行格式非法（需 <文件>\\t<计数>\\t<whitelist>）: %v", row)
		}
		n, err := strconv.Atoi(row[1])
		if err != nil {
			t.Fatalf("quota_acquire_sites.tsv 计数非法: %v", row)
		}
		base[row[0]] = n
	}
	if len(base) == 0 {
		t.Fatal("基线为空（门禁失效）")
	}

	var grew, unknown []string
	for _, rel := range waitqGoFiles(t, root) {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("读取 %s: %v", rel, err)
		}
		n := 0
		for line := range strings.SplitSeq(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if quotaAcquireRe.MatchString(line) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		want, ok := base[rel]
		if !ok {
			unknown = append(unknown, rel+" ("+strconv.Itoa(n)+" 处)")
			continue
		}
		if n > want {
			grew = append(grew, rel+": "+strconv.Itoa(want)+" → "+strconv.Itoa(n))
		}
	}
	if len(unknown) > 0 {
		t.Fatalf("R36-a：以下文件**新增**了直接获取入口调用（必须先收敛到 pkg/waitq，或登记到白名单适配器文件）：\n  %s\n"+
			"设计 docs/designs/2026-10-19-waitq.md §8.2/§8.3。", strings.Join(unknown, "\n  "))
	}
	if len(grew) > 0 {
		t.Fatalf("R36-a：直接获取入口调用点数**增加**（棘轮只减不增）：\n  %s", strings.Join(grew, "\n  "))
	}
}

// resourceAdapterSigRe 是 waitq.Resource 方法集的特征签名（适配器检测口径）。
var resourceAdapterSigRe = regexp.MustCompile(`TryAcquire\(n int64\) error`)

// TestR36c_ResourceAdaptersRegistered 断言每个 waitq.Resource 适配器都已登记
// （资源名 / label / 单位 / 超时 / MaxWaiters / 优先级）。
func TestR36c_ResourceAdaptersRegistered(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	registered := map[string]bool{}
	for _, row := range readTSVRows(t, "waitq_resources.tsv", "|") {
		if len(row) < 8 {
			t.Fatalf("waitq_resources.tsv 行字段不足（需 8 列，`|` 分隔：文件|类型|资源名|label|单位|超时|MaxWaiters|优先级）: %v", row)
		}
		registered[row[0]] = true
	}
	if len(registered) == 0 {
		t.Fatal("适配器登记表为空（门禁失效）")
	}

	var missing []string
	for _, rel := range waitqGoFiles(t, root) {
		// pkg/waitq 自身声明 Resource 接口（非适配器）⇒ 排除。
		if strings.HasPrefix(rel, "pkg/waitq/") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("读取 %s: %v", rel, err)
		}
		if !resourceAdapterSigRe.MatchString(string(src)) {
			continue
		}
		if !registered[rel] {
			missing = append(missing, rel)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("R36-c：以下文件实现了 waitq.Resource 适配器但**未登记**到 internal/archcheck/waitq_resources.tsv：\n  %s\n"+
			"登记需写明：资源名模式 / label / 单位 / 超时 / MaxWaiters / 优先级（设计 §8.3）。",
			strings.Join(missing, "\n  "))
	}
	// 反向：登记的条目必须真实存在（防表腐化）。
	for rel := range registered {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("R36-c：登记表条目 %s 在源码中不存在（删除适配器需同步删表）: %v", rel, err)
		}
	}
}
