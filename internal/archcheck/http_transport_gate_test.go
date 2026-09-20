// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// http_transport_gate_test.go 是「裸 http.Transport 构造不得用于代码」的门禁（R19）。
//
// 背景（2026-09-20）：全仓消除 http.DefaultClient 共享连接池后，自建 Transport 的正确姿势是
// netutil.IsolatedTransport()（DefaultTransport.Clone() 基座——保留 ProxyFromEnvironment /
// 拨号超时 30s / MaxIdleConns=100 / IdleConnTimeout=90s / TLSHandshakeTimeout=10s /
// ForceAttemptHTTP2 等标准库精心调校的默认配置，且独立连接池）。若用零值 &http.Transport{}：
//   - DialContext=nil → 内部零值 net.Dialer **无拨号超时**（连不可达地址挂 OS 级 2min+）；
//   - TLSHandshakeTimeout=0 → TLS 握手**无限等待**；
//   - 无 idle 连接缓存（MaxIdleConns=0）→ 无 keep-alive 复用。
//
// 判据（2026-09-20 C1 升级）：**任何** `&http.Transport{...}` 裸构造都禁止（生产+测试）——
// 连手写子集（如 `&http.Transport{MaxIdleConns: 100, ...}` 只覆写连接池、丢掉 DialContext/
// TLSHandshakeTimeout/ForceAttemptHTTP2/Proxy）也与零值同坑（缺默认调校）。正确姿势：
// `netutil.IsolatedTransport()` 基座 + 覆写定制字段（TLS/DialContext/超时/Proxy 等）。
// 豁免：仅 pkg/netutil/netutil.go 自身的防御回退（IsolatedTransport 类型断言失败分支）。
//
// 扫描实现（与 dead_symbols_test.go 同模式）：纯 Go 目录遍历 moduleRoot 之下的
// 非隐藏目录、非 _test.go 的 .go 文件（本门禁覆盖生产+测试，不跳过 _test.go）。

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoBareTransportConstruction 扫描全部源码（生产+测试）禁裸 &http.Transport{...} 构造。
func TestNoBareTransportConstruction(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// 跳过隐藏目录（.git/.worktrees）与 build 产物。
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "build" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// 排除 helper 定义处（仅 pkg/netutil/netutil.go 的注释描述正确姿势时提到
		// 裸构造，且 IsolatedTransport 有防御回退）与门禁自身文件（其注释/报错
		// 字符串必然提及该模式）。不要排除整个 pkg/netutil 目录。
		rel := filepath.ToSlash(path)
		if strings.HasSuffix(rel, "/pkg/netutil/netutil.go") || strings.HasSuffix(rel, "/internal/archcheck/http_transport_gate_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(data), "&http.Transport{") {
			violations = append(violations, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("代码不得裸构造 &http.Transport{...}（丢默认调校：拨号超时/TLS握手超时/连接池/Proxy）——"+
			"请用 netutil.IsolatedTransport() 基座 + 覆写定制字段:\n%s",
			strings.Join(violations, "\n"))
	}
}
