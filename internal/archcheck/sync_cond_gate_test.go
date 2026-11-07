// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// sync_cond_gate_test.go 是门禁 **R36-b**：非测试源码**不得**再出现 `sync.Cond`
// （等待队列一律走 `pkg/waitq` 通用能力 —— 唯一实现）。设计 docs/designs/2026-10-19-waitq.md §8.3。
//
// 背景（W1 收敛，2026-10-19）：迁移前仓库有 **4 套**独立等待实现（quota 私有 cond、staging 私有
// cond + 观察者 goroutine、cloud 下载轮询、以及若干 sleep 轮询）。收敛到 `pkg/waitq` 后，
// quota 的两处私有 `sync.Cond` 已删除 ⇒ **基线为空**，任何新增即红（棘轮最严形态）。
//
// 扫描口径 = **根 module**：逐目录下探时，遇到自带 `go.mod` 的子目录即**不下探**（嵌套 module
// 不在本仓构建面内，例如 `pkg/tunnel/hub/ext/kad` 的 `persistCond` 与等待队列无关）。
// 与设计 §8.3 的「扫描口径 = 根 module go list ./...」等价。

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// syncCondAllowedFiles 是**显式例外**（每项必须写明理由；当前为空）。
var syncCondAllowedFiles = map[string]string{}

// syncCondSkipDirs 是永不扫描的目录（版本控制/工作树/构建产物）。
var syncCondSkipDirs = map[string]bool{
	".git": true, ".worktrees": true, "build": true, "node_modules": true, "vendor": true,
}

// scanSyncCond 扫描 dir 下的非测试 Go 源码，返回 `<相对路径>:<行号>` 命中列表。
func scanSyncCond(t *testing.T, root, dir string) []string {
	t.Helper()
	var hits []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(dir, name)
		if e.IsDir() {
			if syncCondSkipDirs[name] {
				continue
			}
			// 嵌套 module（自带 go.mod）不在根 module 构建面内 ⇒ 不下探。
			if _, statErr := os.Stat(filepath.Join(full, "go.mod")); statErr == nil {
				continue
			}
			hits = append(hits, scanSyncCond(t, root, full)...)
			continue
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel, relErr := filepath.Rel(root, full)
		if relErr != nil {
			rel = full
		}
		rel = filepath.ToSlash(rel)
		if _, ok := syncCondAllowedFiles[rel]; ok {
			continue
		}
		f, openErr := os.Open(full)
		if openErr != nil {
			t.Fatalf("读取 %s: %v", rel, openErr)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		line := 0
		for sc.Scan() {
			line++
			text := sc.Text()
			// 只看真实使用（声明/构造）；注释里的历史说明不算（避免误报）。
			if strings.HasPrefix(strings.TrimSpace(text), "//") {
				continue
			}
			if strings.Contains(text, "sync.Cond") || strings.Contains(text, "sync.NewCond(") {
				hits = append(hits, rel+":"+strconv.Itoa(line))
			}
		}
		_ = f.Close()
	}
	return hits
}

// TestR36b_NoSyncCondInRootModule 断言根 module 的非测试源码中**没有** `sync.Cond`
// （基线为空 ⇒ 棘轮最严；如需新增必须显式登记到 syncCondAllowedFiles 并写明理由）。
func TestR36b_NoSyncCondInRootModule(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	var hits []string
	for _, sub := range []string{"pkg", "cmd", "internal"} {
		hits = append(hits, scanSyncCond(t, root, filepath.Join(root, sub))...)
	}
	if len(hits) > 0 {
		t.Fatalf("R36-b：根 module 非测试源码中出现 sync.Cond（等待队列必须走 pkg/waitq）：\n  %s\n"+
			"设计 docs/designs/2026-10-19-waitq.md §8.3；确需例外请登记 syncCondAllowedFiles 并写明理由。",
			strings.Join(hits, "\n  "))
	}
}
