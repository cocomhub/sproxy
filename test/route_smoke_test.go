// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

// route_smoke_test.go 是**能力面冒烟门禁**：遍历路由分类表（R38 已保证它与实际注册
// 一一对应），对每条路由发一次最小请求，断言**不得 5xx / 不得命中接线错误**。
//
// 为什么需要（三起真实故障，全部由本类断言可拦）：
//  1. GET /api/search/semantic 被包成 fileRouteRead，但不在 isReadOnlyFileRoute 名单 ⇒ 主 mux **每次 500**；
//  2. POST /api/backends/{type}/presign 名单里是模式串、运行时是具体路径 ⇒ **每次 500**
//     （且隧道内层判为「非文件组」⇒ 不做角色检查，静默越权）；
//  3. GET /api/volumes/export?volume=<不存在> 把客户端错误映射成 **500**。
//
// 三者的共同点：**函数级测试全绿，装配后必错**。此前仓库里没有任何测试遍历过路由表。
//
// 判据刻意宽松：只要不是 5xx、且不含接线错误标记，就算通过（400/401/403/404/501 都是
// 合法业务态）。破坏性端点跑在 E2E 的临时 storage 上，不影响任何真实数据。

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// routeSmokeStreaming 是**流式端点**：请求-响应语义不适用（SSE 长连接），
// 因此只做「连上即可」，用超时判据区分。新增流式端点须显式登记并说明理由
// ——不允许用 skip 掩盖 5xx。
var routeSmokeStreaming = map[string]string{
	"/api/events": "SSE 事件流（长连接）",
}

// wiringErrorMarkers 是 fileRoute/fileRouteRead 的接线自检文案：命中即说明
// 「包装器分类与分类谓词不一致」，属装配错误而非业务错误。
var wiringErrorMarkers = []string{
	"internal: route not in file group",
	"internal: route in read-only subgroup",
	"internal: route not in read-only subgroup",
}

// smokeRoute 是路由分类表的一行（去重后）。
type smokeRoute struct {
	method string
	path   string
	class  string
}

// loadSmokeRoutes 读取 internal/archcheck/route_classes.tsv（R38 已双向锁定其完整性与
// 分类准确性），按 (method, path) 去重——同一路由同时注册在两个 mux 上只需打一次。
func loadSmokeRoutes(t *testing.T) []smokeRoute {
	t.Helper()
	f, err := os.Open(filepath.Join(e2eModuleRoot(), "internal", "archcheck", "route_classes.tsv"))
	if err != nil {
		t.Fatalf("读取 route_classes.tsv: %v", err)
	}
	defer f.Close()

	seen := map[string]bool{}
	var out []smokeRoute
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			t.Fatalf("route_classes.tsv 行格式非法: %q", line)
		}
		pattern := strings.TrimSpace(parts[1])
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			// 纯路径挂载行（如 `/dav/`、`/s3/`）：子路路由树，冒烟按 GET 探一下挂载点。
			method, path = http.MethodGet, pattern
			if !strings.HasPrefix(path, "/") {
				t.Fatalf("route_classes.tsv 挂载点非法: %q", pattern)
			}
		}
		key := method + " " + path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, smokeRoute{method: method, path: path, class: parts[2]})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("扫描 route_classes.tsv: %v", err)
	}
	// 防解析退化（表被清空/格式变更时门禁不能假绿）。
	if len(out) < 80 {
		t.Fatalf("解析到 %d 条路由，少于下限 80 —— 解析器或分类表失效（门禁假绿）", len(out))
	}
	return out
}

// smokeConcretePath 把路由模式里的通配段替换为具体值（请求需要真实路径）。
func smokeConcretePath(pattern string) string {
	repl := strings.NewReplacer(
		"{$}", "",
		"{token}", "smoketoken",
		"{name}", "smokename",
		"{type}", "local",
		"{id}", "smokeid",
		"{session}", "smokesession",
	)
	p := repl.Replace(pattern)
	if p == "" {
		p = "/"
	}
	return p
}

// TestRouteSmoke_NoWiringFailure 逐条打真实服务，断言无 5xx、无接线错误。
func TestRouteSmoke_NoWiringFailure(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	routes := loadSmokeRoutes(t)
	t.Logf("冒烟路由数 = %d（去重后）", len(routes))

	for _, r := range routes {
		r := r
		path := smokeConcretePath(r.path)
		name := r.method + " " + path
		t.Run(name, func(t *testing.T) {
			// 流式端点：只验证连得上（超时 = 通过），不验证响应体。
			if reason, streaming := routeSmokeStreaming[path]; streaming {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, r.method, baseURL+path, nil)
				if err != nil {
					t.Fatalf("构造请求: %v", err)
				}
				resp, err := authedHTTPClient.Do(req)
				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) {
						t.Logf("流式端点按预期保持连接（%s）", reason)
						return
					}
					t.Fatalf("请求失败: %v", err)
				}
				defer resp.Body.Close()
				assertSmokeStatus(t, name, resp, nil)
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, r.method, baseURL+path, strings.NewReader(""))
			if err != nil {
				t.Fatalf("构造请求: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := authedHTTPClient.Do(req)
			if err != nil {
				t.Fatalf("请求失败（%s %s）: %v —— 路由已注册但发不出请求（接线问题）", r.method, path, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			assertSmokeStatus(t, name, resp, body)
		})
	}
}

// assertSmokeStatus 断言响应不是装配故障：非 5xx，且不含接线错误文案。
//
// 5xx 一律算失败：服务端故障不该由「客户端用了空 body」触发——空 body 的合法结果是
// 400/401/403/404/405/409/501（未启用）。确属 5xx 的实现缺陷应当修实现，不得在此豁免。
func assertSmokeStatus(t *testing.T, name string, resp *http.Response, body []byte) {
	t.Helper()
	text := string(body)
	for _, marker := range wiringErrorMarkers {
		if strings.Contains(text, marker) {
			t.Fatalf("%s 命中接线错误（status=%d）：%q\n含义：包装器分类与分类谓词不一致——改分类声明，不要改本测试",
				name, resp.StatusCode, marker)
		}
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		t.Fatalf("%s 返回 %d（装配面冒烟不允许 5xx）：%s",
			name, resp.StatusCode, snippetOf(text))
	}
}

func snippetOf(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
