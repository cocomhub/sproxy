// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// json_response_gate_test.go 是门禁 **R54**：JSON 响应写出**单一实现**。
//
// 背景（2026-11-01 实测）：JSON 响应写出曾有 **5 份**等价实现——装配根
// `sendJSONResponse`（328 处调用）、`pkg/files.Service.sendJSON`（146 处）、
// `pkg/server/ratelimit` 的 429 回包（因无法反向 import 装配根而字节级复制，见 #988）、
// `pkg/server/cloudtasks.writeJSON`、`pkg/server/remotefs` 的两个本地 helper；另有
// 41 处「先 Set Content-Type 再 json.NewEncoder(w).Encode」的裸写（序列化失败无 500 兜底、
// 且各自复制日志/状态码逻辑）。
//
// 收敛结果：唯一实现在 `internal/httpjson`（`Send` / `SendOpt` / `SendErr` /
// `SendError` / `WriteBody`），各层只调用它。本门禁从「棘轮」升级为**硬禁止**：
// 非测试源码中出现裸 JSON 响应写即红（不再保留基线）。
//
// 判据（任一命中即红）：
//   - `json.NewEncoder(w)`：响应侧流式 JSON 写出；
//   - 同行同时出现 `w.Header().Set(` 与 `"application/json"` / `contentTypeJSON`。
//
// 豁免（按路径前缀，均写明理由）：
//   - `internal/httpjson/`：唯一实现本体；
//   - `pkg/testutil/`：模拟外部服务的测试替身，写的是「外部服务」的响应而非本服务契约；
//   - `cmd/`：CLI 把 JSON 打到 stdout，`w` 是 io.Writer 而非 http.ResponseWriter。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// jsonEncoderWriteRe 匹配响应侧流式 JSON 写出（`json.NewEncoder(w)`）。
var jsonEncoderWriteRe = regexp.MustCompile(`json\.NewEncoder\(w\)`)

// jsonResponseHeaderRe 匹配响应头写 Content-Type: application/json（字面量或常量）。
var jsonResponseHeaderRe = regexp.MustCompile(`w\.Header\(\)\.Set\([^)]*(?:"application/json"|contentTypeJSON)`)

// TestJSONResponseSites_NoRawWrites 门禁：JSON 响应写出必须经 internal/httpjson。
func TestJSONResponseSites_NoRawWrites(t *testing.T) {
	t.Parallel()
	for f, n := range countJSONResponseSites(t) {
		t.Errorf("裸 JSON 响应写：%s（%d 处）—— 请改用 internal/httpjson 的 Send/SendOpt/SendErr/SendError/WriteBody（R54）", f, n)
	}
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
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
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
