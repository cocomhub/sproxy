// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestHTTPClientConstructionRatchet —— 门禁：**禁止新增**裸 HTTP 客户端/传输构造点。
//
// 背景（量化，2026-11-08 实测本仓库）：裸 http.Client / http.Transport / DefaultTransport
// 构造曾达 89 处（含测试）、各写一遍的超时字段 90 处、指数退避 >=5 处 —— 没有任何一处能统一
// 回答「这个请求会不会卡、能不能取消」。baidupcs 上传「卡 ~90s 且不可取消」正是把该问题
// 留给第三方库的结果；同构病根在本仓库以「各写一遍」的形式存在。
//
// 因此新增通用基座 pkg/httpc（+ pkg/pipe）作为**唯一编排入口**：
//   - 需要 HTTP 交互 ⇒ 用 pkg/httpc.New(...)（默认 Transport = netutil.IsolatedTransport()）；
//   - 需要超时/重试/退避/并发编排 ⇒ 用 pkg/pipe；
//   - 本门禁是**棘轮**（只减不增）：存量构造点登记在 http_client_budgets.tsv，逐文件计数
//     不得增加；新增文件出现构造点即失败（除非按下方豁免写入理由）。
//
// 豁免目录与理由：
//   - pkg/httpc：基座自身（唯一允许构造 http.Client 的地方）；
//   - pkg/netutil：Transport 的唯一构造点（IsolatedTransport / DefaultTransport）；
//   - pkg/testutil：测试辅助（仅测试使用，非生产路径）；
//   - tools/ 与 *_test.go：非生产代码（且测试需自建隔离客户端，见硬规则 17）。
//
// 登记基线：ARCHCHECK_WRITE_BASELINE=1 go test ./internal/archcheck/ -run TestHTTPClientConstructionRatchet
// （只应在**减少**或经评审后调整时使用；增加必须写明理由）。
const httpClientBaselineFile = "http_client_budgets.tsv"

// httpClientConstructionPattern 匹配**生产代码中的构造点**（注释内的出现不计）。
var httpClientConstructionPattern = regexp.MustCompile(
	`&http\.Client\{|&http\.Transport\{|http\.DefaultTransport`)

// httpClientScanRoots 是扫描根（相对仓库根，测试工作目录即包目录 ⇒ 用 ../..）。
var httpClientScanRoots = []string{"pkg", "cmd", "internal"}

// httpClientExemptDirs 是**已写明理由**的豁免目录（见本文件头注释）。
var httpClientExemptDirs = []string{"pkg/httpc", "pkg/netutil", "pkg/testutil"}

// TestHTTPClientConstructionRatchet 见文件头说明。
func TestHTTPClientConstructionRatchet(t *testing.T) {
	t.Parallel()
	root := repoRootFor(t)
	cur := scanHTTPClientConstructions(t, root)

	if os.Getenv("ARCHCHECK_WRITE_BASELINE") == "1" {
		writeHTTPClientBaseline(t, httpClientBaselineFile, cur)
		return
	}
	base := readHTTPClientBaseline(t, httpClientBaselineFile)

	var added []string
	var grew []string
	for file, n := range cur {
		old, ok := base[file]
		if !ok {
			added = append(added, fmt.Sprintf("%s（%d 处）", file, n))
			continue
		}
		if n > old {
			grew = append(grew, fmt.Sprintf("%s: %d > 基线 %d", file, n, old))
		}
	}
	sort.Strings(added)
	sort.Strings(grew)
	if len(added) == 0 && len(grew) == 0 {
		return
	}
	t.Fatalf("禁止新增裸 HTTP 客户端/传输构造点（硬门禁；存量棘轮只减不增）：\n"+
		"  新增文件: %v\n  超出基线: %v\n"+
		"请改用 pkg/httpc.New(...)（默认 Transport = netutil.IsolatedTransport()）或 pkg/pipe 编排；\n"+
		"确需例外时在 %s 登记并写明理由（ARCHCHECK_WRITE_BASELINE=1 重写基线）。",
		added, grew, httpClientBaselineFile)
}

// repoRootFor 返回仓库根（本包位于 <root>/internal/archcheck）。
func repoRootFor(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return abs
}

// scanHTTPClientConstructions 统计生产 go 文件里的构造点（按仓库相对路径）。
func scanHTTPClientConstructions(t *testing.T, root string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, r := range httpClientScanRoots {
		base := filepath.Join(root, r)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if httpClientDirExempt(rel) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
				return nil
			}
			data, rErr := os.ReadFile(path)
			if rErr != nil {
				return rErr
			}
			if n := countHTTPClientConstructions(string(data)); n > 0 {
				out[rel] = n
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return out
}

// httpClientDirExempt 判定目录是否豁免（含 tools/ 与已写明理由的目录）。
func httpClientDirExempt(rel string) bool {
	if rel == "tools" || strings.HasPrefix(rel, "tools/") {
		return true
	}
	if rel == "build" || strings.HasPrefix(rel, "build/") {
		return true
	}
	for _, e := range httpClientExemptDirs {
		if rel == e || strings.HasPrefix(rel, e+"/") {
			return true
		}
	}
	return false
}

// countHTTPClientConstructions 统计单文件中的构造点（**忽略行内注释之后的内容**，
// 避免把文档里的字面量误判为构造点——历史教训：注释中的字面量曾导致门禁误报）。
func countHTTPClientConstructions(src string) int {
	n := 0
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		n += len(httpClientConstructionPattern.FindAllString(line, -1))
	}
	return n
}

// readHTTPClientBaseline 读基线（文件不存在 ⇒ 视为空基线，等价于硬禁止）。
func readHTTPClientBaseline(t *testing.T, path string) map[string]int {
	t.Helper()
	out := map[string]int{}
	fh, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatalf("open baseline %s: %v", path, err)
	}
	defer func() { _ = fh.Close() }()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			t.Fatalf("基线格式应为 <path>\\t<count>: %q", line)
		}
		n, cErr := strconv.Atoi(parts[1])
		if cErr != nil {
			t.Fatalf("基线计数非法 %q: %v", line, cErr)
		}
		out[parts[0]] = n
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan baseline: %v", err)
	}
	return out
}

// writeHTTPClientBaseline 重写基线（仅在显式设置 ARCHCHECK_WRITE_BASELINE=1 时调用）。
func writeHTTPClientBaseline(t *testing.T, path string, cur map[string]int) {
	t.Helper()
	files := make([]string, 0, len(cur))
	for f := range cur {
		files = append(files, f)
	}
	sort.Strings(files)
	var b strings.Builder
	b.WriteString("# 裸 HTTP 客户端/传输构造点基线（棘轮：只减不增）。\n")
	b.WriteString("# 生成：ARCHCHECK_WRITE_BASELINE=1 go test ./internal/archcheck/ -run TestHTTPClientConstructionRatchet\n")
	b.WriteString("# 新增必须写明理由；豁免目录理由见 http_client_ratchet_test.go 头注释。\n")
	for _, f := range files {
		fmt.Fprintf(&b, "%s\t%d\n", f, cur[f])
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	t.Logf("已重写基线 %s（%d 个文件）", path, len(files))
}
