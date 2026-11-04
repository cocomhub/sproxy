// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// write_face_declared_test.go 锁「写面判定完全来自声明表」。
//
// 背景：写面（mutating）曾由 `isWriteFaceRoute` 的第二份路径清单维护（PUT /api/config、
// 凭据/cloud/sync/volumes 写族…）。漏改的后果是**集群只读副本上仍可写入**（副本与主分叉）。
// 现在写面由各路由表的 `Mutating` 字段（文件域/分享由 ClassUser 派生）回答，本测试保证
// 「已注册的路由都必须能查到声明」——新增路由若既没进表也没登记例外，这里就红。

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writeFaceNonTablePatterns 是不经 Route 声明、因此不可能出现在写面索引里的模式：
// 子树挂载（/dav/、/s3/、/ui/）与直接注册的极少数端点（根重定向、健康检查、隧道传输面、
// 动态插件登录面）。S3 的写方法由 isWriteFaceRoute 显式判定（前缀 + 方法）。
var writeFaceNonTablePatterns = map[string]bool{
	"/s/{token}": true, "/tunnel": true, "/ui/": true, "/{$}": true,
	"/healthz": true, "/version": true, "/metrics": true, "/livez": true, "/readyz": true,
	"/dav/": true, "/s3/": true,
}

// TestWriteFace_EveryRegisteredRouteIsDeclared 断言分类表里的每条路由都能查到写面声明
// （除挂载点/动态面）。
func TestWriteFace_EveryRegisteredRouteIsDeclared(t *testing.T) {
	t.Parallel()
	f, err := os.Open(filepath.Join("..", "..", "internal", "archcheck", "route_classes.tsv"))
	if err != nil {
		t.Fatalf("读取 route_classes.tsv: %v", err)
	}
	defer f.Close()

	var missing []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		pattern := strings.TrimSpace(parts[1])
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			method, path = "GET", pattern
		}
		// hub/signal/relay 是**节点本地协调面**（内存态：路由表、信令转发），
		// 不属持久写面——只读副本上仍应可用（与既有行为一致，见设计 §2.2）。
		if writeFaceNonTablePatterns[path] ||
			strings.HasPrefix(path, "/debug/pprof") ||
			strings.HasPrefix(path, "/auth/") ||
			strings.HasPrefix(path, "/api/hub/") ||
			strings.HasPrefix(path, "/api/signal/") ||
			strings.HasPrefix(path, "/api/relay/") {
			continue
		}
		key := method + " " + path
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := declaredMutating(key); !ok {
			missing = append(missing, key)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("扫描 route_classes.tsv: %v", err)
	}
	if len(seen) < 80 {
		t.Fatalf("解析到 %d 条路由，少于下限 80 —— 解析器或分类表失效", len(seen))
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("以下已注册路由没有写面声明（新增路由必须进表并声明 Mutating，或登记为非表挂载点）：\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestWriteFace_S3MountStillGuarded 锁 S3 子树挂载的写方法判定（非 Route 声明，必须特判）。
func TestWriteFace_S3MountStillGuarded(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"PUT", "POST", "DELETE"} {
		if !isWriteFaceRoute(m, "/s3/k.txt") {
			t.Errorf("%s /s3/k.txt 必须是写面（否则只读副本可经 S3 写入）", m)
		}
	}
	for _, m := range []string{"GET", "HEAD"} {
		if isWriteFaceRoute(m, "/s3/k.txt") {
			t.Errorf("%s /s3/k.txt 不应是写面", m)
		}
	}
}
