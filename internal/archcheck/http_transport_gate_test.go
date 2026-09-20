// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// http_transport_gate_test.go 是「裸 http.Transport 构造不得用于代码」的门禁（R19）
// 与「http.Client 构造必须带 Transport」的门禁（R20）。
//
// 背景（2026-09-20）：全仓消除 http.DefaultClient 共享连接池后，自建 Transport 的正确姿势是
// netutil.IsolatedTransport()（DefaultTransport.Clone() 基座——保留 ProxyFromEnvironment /
// 拨号超时 30s / MaxIdleConns=100 / IdleConnTimeout=90s / TLSHandshakeTimeout=10s /
// ForceAttemptHTTP2 等标准库精心调校的默认配置，且独立连接池）。若用零值 &http.Transport{}：
//   - DialContext=nil → 内部零值 net.Dialer **无拨号超时**（连不可达地址挂 OS 级 2min+）；
//   - TLSHandshakeTimeout=0 → TLS 握手**无限等待**；
//   - 无 idle 连接缓存（MaxIdleConns=0）→ 无 keep-alive 复用。
//
// R19 判据（2026-09-20 C1 升级）：**任何** `&http.Transport{...}` 裸构造都禁止（生产+测试）——
// 连手写子集（如 `&http.Transport{MaxIdleConns: 100, ...}` 只覆写连接池、丢掉 DialContext/
// TLSHandshakeTimeout/ForceAttemptHTTP2/Proxy）也与零值同坑（缺默认调校）。正确姿势：
// `netutil.IsolatedTransport()` 基座 + 覆写定制字段（TLS/DialContext/超时/Proxy 等）。
//
// R20 判据（2026-09-20 P1/P4）：**生产**代码中 `&http.Client{...}` 构造必须带 `Transport:`
// 字段——不带即隐式共享 http.DefaultTransport（`http.Client{Timeout}` 的 Transport 为 nil
// 时走包级全局连接池，外部 CloseIdleConnections 会打断在途请求）。测试侧不强制
// （传 `&http.Client{Timeout: 5s}` 给 relay.Serve 等 helper 参数无真实并发 Do 风险）。
// 正确姿势：`Transport: netutil.IsolatedTransport()`（或注入的共享实例）。
//
// 豁免：仅 pkg/netutil/netutil.go 自身的防御回退（IsolatedTransport 类型断言失败分支）
// 与门禁自身文件（注释/报错字符串必然提及模式）。不要排除整个 pkg/netutil 目录。
//
// 扫描实现（与 dead_symbols_test.go 同模式）：纯 Go 目录遍历 moduleRoot 之下的
// 非隐藏目录、非 _test.go 的 .go 文件（本门禁覆盖生产+测试，不跳过 _test.go）。

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// walkSourceFiles 遍历 moduleRoot 下全部 .go 文件（非隐藏目录/非 build/vendor），
// 排除豁免文件，对每个文件调用 fn（返回的字符串非空即违规）。
func walkSourceFiles(t *testing.T, fn func(rel string, data []byte) string) []string {
	t.Helper()
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
		rel := filepath.ToSlash(path)
		// 排除 helper 定义处（仅 pkg/netutil/netutil.go 的注释描述正确姿势时提到
		// 裸构造/客户端，且 IsolatedTransport 有防御回退）与门禁自身文件。
		if strings.HasSuffix(rel, "/pkg/netutil/netutil.go") || strings.HasSuffix(rel, "/internal/archcheck/http_transport_gate_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if msg := fn(rel, data); msg != "" {
			violations = append(violations, rel+": "+msg)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码: %v", err)
	}
	return violations
}

// TestNoBareTransportConstruction 扫描全部源码（生产+测试）禁裸 &http.Transport{...} 构造。
func TestNoBareTransportConstruction(t *testing.T) {
	t.Parallel()
	var violations []string
	for _, rel := range walkSourceFiles(t, func(_ string, data []byte) string {
		if strings.Contains(string(data), "&http.Transport{") {
			return "裸构造 &http.Transport{...}"
		}
		return ""
	}) {
		violations = append(violations, "  "+rel)
	}
	if len(violations) > 0 {
		t.Fatalf("代码不得裸构造 &http.Transport{...}（丢默认调校：拨号超时/TLS握手超时/连接池/Proxy）——"+
			"请用 netutil.IsolatedTransport() 基座 + 覆写定制字段:\n%s",
			strings.Join(violations, "\n"))
	}
}

// clientBlockHasTransport 判断 data 中 [start, end) 的 &http.Client{...} 构造块
// 是否含 Transport: 字段（块边界用 { } 配对扫描，容忍嵌套的 func(){} 等）。
func clientBlockHasTransport(data []byte, start int) bool {
	// 找到 { 与配对的 }
	open := bytes.IndexByte(data[start:], '{')
	if open < 0 {
		return false
	}
	open += start
	depth := 0
	end := -1
	for i := open; i < len(data); i++ {
		switch data[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return false
	}
	return strings.Contains(string(data[open:end]), "Transport:")
}

// TestNoClientWithoutTransportInProduction 扫描生产源码（非 _test.go）禁
// &http.Client{...} 构造不带 Transport: 字段（隐式共享 http.DefaultTransport）。
func TestNoClientWithoutTransportInProduction(t *testing.T) {
	t.Parallel()
	var violations []string
	for _, rel := range walkSourceFiles(t, func(rel string, data []byte) string {
		if strings.HasSuffix(rel, "_test.go") {
			return "" // 测试侧不强制
		}
		// 找所有 &http.Client{ 出现，逐个检查其后构造块是否带 Transport:。
		s := string(data)
		for {
			idx := strings.Index(s, "&http.Client{")
			if idx < 0 {
				break
			}
			sub := s[idx:]
			// 跳过注释/字符串中的匹配（朴素处理：构造块必须紧随 &http.Client{）
			if !clientBlockHasTransport([]byte(sub), 0) {
				return "&http.Client{...} 构造缺 Transport: 字段（隐式共享 http.DefaultTransport——硬规则 17）"
			}
			s = s[idx+len("&http.Client{"):]
		}
		return ""
	}) {
		violations = append(violations, "  "+rel)
	}
	if len(violations) > 0 {
		t.Fatalf("生产代码 &http.Client{...} 必须带 Transport: 字段（禁隐式共享 http.DefaultTransport）——"+
			"请用 netutil.IsolatedTransport() 或注入共享实例:\n%s",
			strings.Join(violations, "\n"))
	}
}
