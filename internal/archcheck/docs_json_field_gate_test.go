// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// docs_json_field_gate_test.go 是门禁 **R58**：**文档里出现的 JSON 字段名必须真的存在于源码**。
//
// 背景（与 R57 同一模式，2026-11-25）：R55 校验「文档路径 ↔ 路由表」、R57 校验「query 参数被读取」，
// 但**请求体/响应体的字段名**没有任何护栏。历史缺陷 `POST /api/shares` 的 `password` 就属于这一类：
// 文档写了字段，服务端结构体里**根本没有**该字段（JSON 解码静默忽略 ⇒ 功能看起来「没生效」但不报错）。
//
// 判据：
//  1. `docs/api.md` 里每个 `### METHOD /path` 章节内的 ```json 代码块，其**键名**必须能在
//     非测试 Go 源码中找到（`json:"k"` tag 或字符串字面量 `"k"`）——源码里完全没有出现过这个词，
//     说明该字段是凭空写的（文档腐化）；
//  2. 正当例外（示例中刻意展示的"外部系统字段"、示例占位等）登记在
//     `docs_json_fields.tsv`：`<字段名>\t<说明>`；**已能匹配的条目必须删除**（只减不增）；
//  3. 防假绿：文档侧 ≥80 个字段、源码侧 ≥300 个 JSON tag，否则判为解析器失效。

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
	// minR58DocFields / minR58SrcTags 是两侧下限（当前实际 ~192 / ~640，留足余量）。
	minR58DocFields = 80
	minR58SrcTags   = 300
	// docsJSONFieldsTSV 是正当例外清单（字段名 + 说明）。
	docsJSONFieldsTSV = "docs_json_fields.tsv"
)

var (
	// docJSONKeyRe 匹配 JSON 代码块里的键名（只认小写蛇形，避免误抓自然语言引号）。
	docJSONKeyRe = regexp.MustCompile(`"([a-z][a-z0-9_]*)"\s*:`)
	// srcJSONTagRe 匹配 Go 结构体 json tag 名。
	srcJSONTagRe = regexp.MustCompile(`json:"([a-z][a-z0-9_]*)`)
	// srcAnyStringRe 匹配源码中的任意字符串字面量（覆盖 map 字面量等无 tag 形态）。
	srcAnyStringRe = regexp.MustCompile(`"([a-z][a-z0-9_]*)"`)
)

// docJSONFields 从 docs/api.md 的 JSON 代码块取全部键名。
func docJSONFields(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "docs", "api.md"))
	if err != nil {
		t.Fatalf("R58：读取 docs/api.md: %v", err)
	}
	out := map[string]bool{}
	for _, b := range jsonCodeBlocks(string(data)) {
		for _, m := range docJSONKeyRe.FindAllStringSubmatch(b, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// jsonCodeBlocks 抽出 ```json ... ``` 代码块内容。
func jsonCodeBlocks(doc string) []string {
	var out []string
	rest := doc
	for {
		start := strings.Index(rest, "```json\n")
		if start < 0 {
			return out
		}
		rest = rest[start+len("```json\n"):]
		end := strings.Index(rest, "```")
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
		rest = rest[end+3:]
	}
}

// srcJSONVocabulary 返回源码里出现过的 JSON 相关词汇（tag 名 + 字符串字面量）。
func srcJSONVocabulary(t *testing.T) (tags map[string]bool, vocab map[string]bool) {
	t.Helper()
	root := moduleRoot(t)
	tags, vocab = map[string]bool{}, map[string]bool{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// 相对 root 判跳过（worktree 绝对路径含 /.worktrees/ —— 用绝对路径会把整个仓库跳过）。
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
		raw, rerr2 := os.ReadFile(path)
		if rerr2 != nil {
			return nil
		}
		text := string(raw)
		for _, m := range srcJSONTagRe.FindAllStringSubmatch(text, -1) {
			tags[m[1]] = true
		}
		for _, m := range srcAnyStringRe.FindAllStringSubmatch(text, -1) {
			vocab[m[1]] = true
		}
		return nil
	})
	return tags, vocab
}

// readDocJSONFieldExceptions 读取例外清单（缺失 = 空，合法终态）。
func readDocJSONFieldExceptions(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	f, err := os.Open(docsJSONFieldsTSV)
	if errors.Is(err, fs.ErrNotExist) {
		return out
	}
	if err != nil {
		t.Fatalf("R58：读取 %s: %v", docsJSONFieldsTSV, err)
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
			t.Fatalf("R58：%s 行格式非法（需 `<字段名>\\t<说明>`）: %q", docsJSONFieldsTSV, line)
		}
		out[strings.TrimSpace(parts[0])] = true
	}
	return out
}

// TestR58_DocJSONFieldsExist 校验「文档 JSON 示例里的字段名确实存在于源码」。
func TestR58_DocJSONFieldsExist(t *testing.T) {
	t.Parallel()
	docFields := docJSONFields(t)
	tags, vocab := srcJSONVocabulary(t)
	if len(docFields) < minR58DocFields {
		t.Fatalf("R58：解析到的文档 JSON 字段过少（%d < %d），docs/api.md 或解析器失效",
			len(docFields), minR58DocFields)
	}
	if len(tags) < minR58SrcTags {
		t.Fatalf("R58：解析到的源码 json tag 过少（%d < %d），扫描面或解析器失效",
			len(tags), minR58SrcTags)
	}
	exceptions := readDocJSONFieldExceptions(t)

	// 1) 文档出现、源码完全没有 ⇒ 红（字段是凭空写的）。
	var missing []string
	for k := range docFields {
		if !tags[k] && !vocab[k] && !exceptions[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("R58：docs/api.md 的 JSON 示例里出现了这些字段，但源码从未出现（文档腐化；\n"+
			"     补实现/改文档/确有正当例外则登记到 %s 并写理由）：\n  %s",
			docsJSONFieldsTSV, strings.Join(missing, "\n  "))
	}

	// 2) 清单只减不增：已能在源码找到的条目必须删除。
	var stale []string
	for k := range exceptions {
		if tags[k] || vocab[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("R58：以下字段已在源码中出现，请从 %s 删除（清单只减不增）：\n  %s",
			docsJSONFieldsTSV, strings.Join(stale, "\n  "))
	}
}
