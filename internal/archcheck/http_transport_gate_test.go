// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// http_transport_gate_test.go 是「零值 http.Transport 不得用于生产代码」的门禁（R19）。
//
// 背景（2026-09-20）：全仓消除 http.DefaultClient 共享连接池后，自建 Transport 的正确姿势是
// netutil.IsolatedTransport()（DefaultTransport.Clone() 基座——保留 ProxyFromEnvironment /
// 拨号超时 30s / MaxIdleConns=100 / IdleConnTimeout=90s / TLSHandshakeTimeout=10s /
// ForceAttemptHTTP2 等标准库精心调校的默认配置，且独立连接池）。若用零值 &http.Transport{}：
//   - DialContext=nil → 内部零值 net.Dialer **无拨号超时**（连不可达地址挂 OS 级 2min+）；
//   - TLSHandshakeTimeout=0 → TLS 握手**无限等待**；
//   - 无 idle 连接缓存（MaxIdleConns=0）→ 无 keep-alive 复用。
//
// 判据：非测试 .go 源码不得以 `&http.Transport{}` 形式出现（零值构造）。测试允许
// （连 httptest 明文服务器零值无实际影响，但建议同样用 IsolatedTransport/Clone）。
// 允许 `netutil.IsolatedTransport()` / `base.Clone()` 等正确姿势。
//
// 扫描实现（与 dead_symbols_test.go 同模式）：纯 Go 目录遍历 moduleRoot 之下的
// 非隐藏目录、非 _test.go 的 .go 文件。

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoZeroValueTransportInProduction 扫描非测试源码禁零值 &http.Transport{}。
func TestNoZeroValueTransportInProduction(t *testing.T) {
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// 排除 helper 定义处（仅 pkg/netutil/netutil.go 的注释描述正确姿势时提到零值，
		// 且 IsolatedTransport 有防御回退）。不要排除整个 pkg/netutil 目录——否则该目录
		// 新增生产文件的零值 Transport 会漏过门禁（2026-09-20 变异验证实证）。
		if path == filepath.Join(root, "pkg", "netutil", "netutil.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(data), "&http.Transport{}") {
			violations = append(violations, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("生产代码不得用零值 &http.Transport{}（丢默认调校：拨号超时/TLS握手超时/连接池）——请用 netutil.IsolatedTransport():\n%s",
			strings.Join(violations, "\n"))
	}
}
