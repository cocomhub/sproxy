// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// json_response_gate_test.go 是门禁 **R54**：JSON 响应写出**单一实现**棘轮（只减不增）。
//
// 背景（2026-11-01 实测）：JSON 响应写出曾有三份等价实现——装配根 `sendJSONResponse`
// （328 处调用）、`pkg/files.Service.sendJSON`（146 处）、`pkg/server/ratelimit` 的 429
// 回包（因无法反向 import 装配根而字节级复制，见 #988）。另有 25 处「先 Set
// Content-Type 再 json.NewEncoder(w).Encode」的裸写，绕过统一实现（序列化失败无 500 兜底）。
//
// 收敛方向：实现收进 `internal/httpjson`（`Send` / `SendLogged` / `SendError`），各层只调用。
// 本门禁锁住**存量只减不增**：
//   - 判据：非测试源码中「裸 JSON 响应写」语句数按文件登记于 `json_response_sites.tsv`；
//     未登记文件出现裸写即红；已登记文件计数不得超过基线。
//   - 计入形态：`json.NewEncoder(w)`；同行同时出现 `w.Header().Set(` 与
//     `"application/json"` / `contentTypeJSON` 的响应头写。
//   - 豁免：`internal/httpjson/`（唯一实现本体）、`pkg/testutil/`（模拟外部服务的测试替身，
//     写的是「外部服务」的响应而非本服务契约）、`cmd/`（CLI 把 JSON 打到 stdout，`w` 是
//     io.Writer 而非 http.ResponseWriter，与 HTTP 响应契约无关）。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// jsonEncoderWriteRe 匹配响应侧流式 JSON 写出（`json.NewEncoder(w)`）。
var jsonEncoderWriteRe = regexp.MustCompile(`json\.NewEncoder\(w\)`)

// jsonResponseHeaderRe 匹配响应头写 Content-Type: application/json（字面量或常量）。
var jsonResponseHeaderRe = regexp.MustCompile(`w\.Header\(\)\.Set\([^)]*(?:"application/json"|contentTypeJSON)`)

// TestJSONResponseSites_Ratchet 门禁：裸 JSON 响应写只减不增。
func TestJSONResponseSites_Ratchet(t *testing.T) {
	t.Parallel()
	baseline := readJSONResponseSites(t)
	for f, n := range countJSONResponseSites(t) {
		base, ok := baseline[f]
		if !ok {
			t.Errorf("未登记的裸 JSON 响应写：%s（%d 处）—— 请改用 internal/httpjson 的 Send/SendError，或登记到 internal/archcheck/json_response_sites.tsv 并说明理由（R54）", f, n)
			continue
		}
		if n > base {
			t.Errorf("裸 JSON 响应写增加：%s 基线 %d → 实际 %d（棘轮只减不增，请改用 internal/httpjson）", f, base, n)
		}
	}
}

// readJSONResponseSites 读取基线表（缺文件 = 失败，避免门禁空转）。
func readJSONResponseSites(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile("json_response_sites.tsv")
	if err != nil {
		t.Fatalf("读取 json_response_sites.tsv 失败: %v", err)
	}
	out := make(map[string]int)
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			t.Fatalf("json_response_sites.tsv 行格式非法: %q", line)
		}
		n, cerr := strconv.Atoi(strings.TrimSpace(parts[1]))
		if cerr != nil {
			t.Fatalf("json_response_sites.tsv 数量非法: %q", line)
		}
		out[filepath.ToSlash(strings.TrimSpace(parts[0]))] = n
	}
	if len(out) == 0 {
		t.Fatal("json_response_sites.tsv 为空（门禁失效）")
	}
	return out
}

// countJSONResponseSites 统计非测试源码中的裸 JSON 响应写（按文件）。
func countJSONResponseSites(t *testing.T) map[string]int {
	t.Helper()
	out := make(map[string]int)
	root := moduleRoot(t)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// 跳过非源码目录（worktree 副本 / 构建产物 / 依赖缓存），否则他人分支的文件会被算进来。
		slash := "/" + filepath.ToSlash(path)
		for _, skip := range []string{"/.worktrees/", "/build/", "/node_modules/", "/.git/"} {
			if strings.Contains(slash, skip) {
				return nil
			}
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if isJSONResponseSiteExempt(relSlash) {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		n := 0
		for line := range strings.SplitSeq(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if jsonEncoderWriteRe.MatchString(line) {
				n++
			}
			if jsonResponseHeaderRe.MatchString(line) {
				n++
			}
		}
		if n > 0 {
			out[relSlash] = n
		}
		return nil
	})
	return out
}

// isJSONResponseSiteExempt 判断文件是否属于 R54 豁免面。
func isJSONResponseSiteExempt(rel string) bool {
	// 唯一实现本体。
	if strings.HasPrefix(rel, "internal/httpjson/") {
		return true
	}
	// 测试替身：模拟外部服务（vault/sync 等）的响应，不是本服务的响应契约。
	if strings.HasPrefix(rel, "pkg/testutil/") {
		return true
	}
	// CLI 输出：`w` 是 io.Writer（stdout），不是 HTTP 响应。
	if strings.HasPrefix(rel, "cmd/") {
		return true
	}
	return false
}
