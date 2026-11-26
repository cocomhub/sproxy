// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// docs_query_param_gate_test.go 是门禁 **R57**：**文档声明的 query 参数必须真的被读取**。
//
// 背景（2026-11-25 实测，三处同型缺陷）：
//   - `docs/api.md` 声称 `POST /api/shares` 支持 `password?` —— 服务端**从未实现**，分享无法加密；
//   - 文档与代码注释都声称 `GET /s/{token}` 支持 `?transform=thumb&watermark=` ——
//     分享面直接 `io.Copy` 原文件，参数被**完全忽略**；
//   - v2 传 `ttl: 7`（天数）被后端按 Go duration 解析 ⇒ 创建分享恒 400。
//
// R55 只校验「文档里的**路径**是否存在」；参数是否被读取**完全没有护栏** ⇒ 上述漂移不会让任何
// 测试变红。本门禁把「文档参数必须能在服务端源码里找到读取点（或显式登记理由）」变成判据。
//
// 判据：
//  1. `docs/api.md` 标题与正文里出现的 `?k=` / `&k=` 参数名，必须能在**非测试 Go 源码**中
//     匹配到读取形态（`Query().Get("k")` / `FormValue("k")` / `PathValue("k")` /
//     `q.Get("k")` 等），否则红；
//  2. 确有正当例外（如参数由通用中间件转发、由反序列化整体接收）必须逐条登记在
//     `docs_query_params.tsv`：`<参数名>\t<说明>`；**已能匹配的条目必须删除**（清单只减不增）；
//  3. 防假绿：两侧解析结果各需达到下限（文档 ≥30 个参数、源码 ≥50 个读取点），
//     解析器或输入失效时不会「两边都空 ⇒ 恒真」。

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	// minR57DocParams / minR57ReadParams 是两侧下限护栏（当前实际 ~43 / ~200，留足余量）。
	minR57DocParams  = 30
	minR57ReadParams = 50
	// docsQueryParamsTSV 是正当例外清单（参数名 + 说明）。
	docsQueryParamsTSV = "docs_query_params.tsv"
)

// docQueryParamRe 匹配文档中的 query 参数（`?k=` 或 `&k=`），只认小写蛇形名。
var docQueryParamRe = regexp.MustCompile(`[?&]([a-z][a-z0-9_]*)=`)

// srcQueryParamRe 匹配源码中的 query 参数读取形态（覆盖本仓实际用到的全部写法）。
var srcQueryParamRe = regexp.MustCompile(
	`(?:Query\(\)\.Get|Query\(\)\[|FormValue|PathValue|PostFormValue|` +
		`q\.Get|query\.Get|params\.Get)\(\s*"([a-z][a-z0-9_]*)"`)

// docQueryParams 从 docs/api.md 取「文档声明过的全部 query 参数名」。
func docQueryParams(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "docs", "api.md"))
	if err != nil {
		t.Fatalf("R57：读取 docs/api.md: %v", err)
	}
	out := map[string]bool{}
	for _, m := range docQueryParamRe.FindAllStringSubmatch(string(data), -1) {
		out[m[1]] = true
	}
	return out
}

// srcReadParams 扫描非测试 Go 源码，返回「被读取过的 query 参数名」集合。
func srcReadParams(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRoot(t)
	out := map[string]bool{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// 注意：判据必须用**相对 root 的**路径——worktree 的绝对路径本身可能含
		// `/.worktrees/`（本仓 agent 一律在 .worktrees/<task> 内开发），用绝对路径判定会把
		// **整个仓库**都跳过 ⇒ 扫描结果为 0（下限护栏会拦住，但根因必须修）。
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		slash := "/" + filepath.ToSlash(rel)
		for _, skip := range []string{"/.worktrees/", "/build/", "/node_modules/", "/.git/"} {
			if strings.Contains(slash, skip) {
				return nil
			}
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range srcQueryParamRe.FindAllStringSubmatch(string(raw), -1) {
			out[m[1]] = true
		}
		return nil
	})
	return out
}

// readDocQueryParamExceptions 读取例外清单（缺失 = 空清单，合法终态）。
func readDocQueryParamExceptions(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	f, err := os.Open(docsQueryParamsTSV)
	if errors.Is(err, fs.ErrNotExist) {
		return out
	}
	if err != nil {
		t.Fatalf("R57：读取 %s: %v", docsQueryParamsTSV, err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			t.Fatalf("R57：%s 行格式非法（需 `<参数名>\\t<说明>`）: %q", docsQueryParamsTSV, line)
		}
		out[strings.TrimSpace(parts[0])] = true
	}
	return out
}

// TestR57_DocQueryParamsAreRead 校验「文档声明的 query 参数确实被服务端读取」。
func TestR57_DocQueryParamsAreRead(t *testing.T) {
	t.Parallel()
	docParams := docQueryParams(t)
	readParams := srcReadParams(t)
	if len(docParams) < minR57DocParams {
		t.Fatalf("R57：解析到的文档 query 参数过少（%d < %d），docs/api.md 或解析器失效",
			len(docParams), minR57DocParams)
	}
	if len(readParams) < minR57ReadParams {
		t.Fatalf("R57：解析到的源码参数读取点过少（%d < %d），扫描面或解析器失效",
			len(readParams), minR57ReadParams)
	}
	exceptions := readDocQueryParamExceptions(t)

	// 1) 文档声称但源码从不读取 ⇒ 红（要么实现，要么改文档，要么显式登记理由）。
	var unimplemented []string
	for p := range docParams {
		if !readParams[p] && !exceptions[p] {
			unimplemented = append(unimplemented, p)
		}
	}
	sort.Strings(unimplemented)
	if len(unimplemented) > 0 {
		t.Errorf("R57：docs/api.md 声明了这些 query 参数，但服务端源码从未读取（文档腐化或能力缺失；\n"+
			"     实现它 / 改文档 / 确有正当例外则登记到 %s 并写理由）：\n  %s",
			docsQueryParamsTSV, strings.Join(unimplemented, "\n  "))
	}

	// 2) 清单只减不增：已经能匹配到读取点的条目必须删除。
	var stale []string
	for p := range exceptions {
		if readParams[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("R57：以下参数已在源码中被读取，请从 %s 删除（清单只减不增）：\n  %s",
			docsQueryParamsTSV, strings.Join(stale, "\n  "))
	}
}
