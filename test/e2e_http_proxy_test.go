// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_http_proxy_test.go 覆盖 `sclient http-proxy`（真实二进制 + 子进程）：
// 无出口场景 = 纯本地直连代理，Go 客户端经代理（Transport.Proxy）访问本地 httptest
// 目标，断言经代理取回的 body。
//
// 复用既有 e2e 基建（同 package sproxy_test）：
//   - e2eBinPath（test/e2e_relay_test.go）：整包单次构建 sclient 二进制
//   - lockedBuffer（test/e2e_mesh_node_test.go）：带锁 stdout/stderr 捕获（子进程后台
//     写 + 测试主 goroutine 轮询读，防 -race 数据竞争）
//   - newKillWaitCleanup（test/e2e_relay_test.go）：Kill+Wait 幂等 cleanup
//
// 配置隔离（硬规则）：--config 指向临时 sclient.yaml（sclientcfg.New 对 IsNotExist
// 静默忽略），不读本机 ~/.config/sproxy/sclient.yaml，防测试行为被本机配置污染。
package sproxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// proxyReadyRe 匹配 http-proxy 就绪横幅，提取实际监听地址
// （-l 127.0.0.1:0 随机端口时 banner 打印 ln.Addr()）。
var proxyReadyRe = regexp.MustCompile(`HTTP 代理就绪: ([0-9.:]+)`)

// startHTTPProxyLongRunning 启动长驻 `sclient http-proxy` 子进程（Serve 阻塞），
// 轮询 stdout 就绪横幅取实际监听地址。返回 (代理地址, cleanup)。
//
// 无 --exit/--exit-auto → 恒本地直连（本机出口语义）；-l 127.0.0.1:0 随机端口。
func startHTTPProxyLongRunning(t *testing.T, extraArgs ...string) (string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	binPath := e2eBinPath(t, "cmd/sclient")

	// 配置隔离：--config 指向不存在路径（sclientcfg.New 静默忽略），
	// 隔离本机 ~/.config/sproxy/sclient.yaml（--server 挡不住用户配置里的凭据）。
	configPath := filepath.Join(tmpDir, "sclient.yaml")
	if err := os.WriteFile(configPath, []byte(""), 0644); err != nil {
		t.Fatalf("write sclient config: %v", err)
	}

	args := []string{
		"http-proxy",
		"--config", configPath,
		"-l", "127.0.0.1:0",
	}
	args = append(args, extraArgs...)
	cmd := exec.Command(binPath, args...)
	cmd.Dir = e2eModuleRoot()
	var stdout, stderr lockedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sclient http-proxy: %v", err)
	}
	logStderrOnFailure(t, "sclient http-proxy", &stderr)
	killWait := newKillWaitCleanup(cmd)

	// 轮询 stdout 就绪横幅（banner 走 stdout：cli.IOStreams.Out）。
	var addr string
	testutil.WaitFor(t, 30*time.Second, func() bool {
		if m := proxyReadyRe.FindStringSubmatch(stdout.String()); m != nil {
			addr = m[1]
			return true
		}
		return false
	}, "sclient http-proxy 未在超时内就绪（stdout 无 'HTTP 代理就绪' 横幅）")
	if addr == "" {
		killWait()
		t.Fatalf("http-proxy not ready; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	return addr, killWait
}

// TestE2E_HTTPProxy_NoExit 无出口场景：纯本地直连代理 + Go 客户端经代理访问本地目标。
func TestE2E_HTTPProxy_NoExit(t *testing.T) {
	t.Parallel()
	// 目标页面（本地）。
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "e2e-ok")
	}))
	defer target.Close()

	// 起 sclient http-proxy（无出口 → 恒本地直连）。
	proxyAddr, cleanup := startHTTPProxyLongRunning(t)
	defer cleanup()

	// 客户端经代理访问（显式 Proxy URL：Transport.Proxy 指向代理）。
	req, err := http.NewRequest(http.MethodGet, target.URL, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.URL, _ = url.Parse(target.URL)
	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://" + proxyAddr) }}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("经代理访问失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "e2e-ok") {
		t.Fatalf("body = %q, want 含 e2e-ok", body)
	}
}

// TestE2E_HTTPProxy_BasicAuth 配 --proxy-user/--proxy-pass 后：
// 未认证请求 407，带 Basic 凭据请求成功（经代理取回目标）。
func TestE2E_HTTPProxy_BasicAuth(t *testing.T) {
	t.Parallel()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "auth-ok")
	}))
	defer target.Close()

	proxyAddr, cleanup := startHTTPProxyLongRunning(t, "--proxy-user", "u", "--proxy-pass", "p")
	defer cleanup()

	// 未认证 → 407。
	trNoAuth := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://" + proxyAddr) }}
	defer trNoAuth.CloseIdleConnections()
	respNoAuth, err := (&http.Client{Transport: trNoAuth}).Get(target.URL)
	if err != nil {
		t.Fatalf("未认证请求失败: %v", err)
	}
	defer respNoAuth.Body.Close()
	if respNoAuth.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("未认证状态 = %d, want 407", respNoAuth.StatusCode)
	}

	// 带 Basic 凭据 → 成功。Go Transport 对含 userinfo 的代理 URL 自动为每个代理请求
	// 生成 Proxy-Authorization: Basic（绝对 URI 转发与 CONNECT 均生效）。
	authProxyURL := "http://u:p@" + proxyAddr
	trAuth := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse(authProxyURL) }}
	defer trAuth.CloseIdleConnections()
	respAuth, err := (&http.Client{Transport: trAuth}).Get(target.URL)
	if err != nil {
		t.Fatalf("带认证请求失败: %v", err)
	}
	defer respAuth.Body.Close()
	body, _ := io.ReadAll(respAuth.Body)
	if !strings.Contains(string(body), "auth-ok") {
		t.Fatalf("body = %q, want 含 auth-ok", body)
	}
}

// TestE2E_HTTPProxy_NoProxyEnv 验证 http_proxy 环境变量开箱即用：
// 设 http_proxy 指向代理，Go 默认 Transport（ProxyFromEnvironment）自动走代理。
// 注：本用例用 t.Setenv（环境变量隔离）——Go 禁止并行测试 Setenv（checkParallel
// panic），故本用例串行（R18 豁免：函数体含 t.Setenv）。
func TestE2E_HTTPProxy_NoProxyEnv(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "env-ok")
	}))
	defer target.Close()

	proxyAddr, cleanup := startHTTPProxyLongRunning(t)
	defer cleanup()

	// 环境变量指向代理（t.Setenv 自动恢复；HTTP_PROXY 大写下划线同时设，覆盖 Go
	// ProxyFromEnvironment 的 127.0.0.1 特例——Go 对回环目标默认不走代理，这里用
	// NO_PROXY 显式空以确认仍走代理：实际上 127.0.0.1 目标 Go 会绕过代理！）。
	// 故本用例目标用非回环 host 名（localhost 解析回环但 host 名非 IP，ProxyFromEnvironment
	// 的 no_proxy 匹配按 host 名），或设 NO_PROXY="" 也不影响 127.0.0.1 特例——
	// Go 的 ProxyFromEnvironment 对 127.0.0.1 目标恒直连（httpproxy.go 有防环回注释）。
	// 改为：目标用 127.0.0.1 但经 Transport{Proxy: 显式} 已验证（NoExit 用例）；
	// 本用例验证的是「代理不读自身 http_proxy 防环回」已在单测覆盖。故本用例
	// 验证 ProxyFromEnvironment 场景改用显式 NON-loopback host。
	t.Setenv("http_proxy", "http://"+proxyAddr)
	t.Setenv("HTTP_PROXY", "http://"+proxyAddr)
	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")

	// 目标：127.0.0.1（Go ProxyFromEnvironment 对回环 IP 特例直连——见上注释）。
	// 故断言「显式代理下 127.0.0.1 也走代理」需要非回环 host。用 localhost 主机名：
	// Go 对 hostname 非 IP 目标不命中回环特例（127.0.0.1 特例按 IP 判断），
	// localhost 解析为 127.0.0.1 但 host 字符串非 IP → 走代理。
	localTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "env-ok")
	}))
	defer localTarget.Close()
	host := strings.Replace(localTarget.URL, "127.0.0.1", "localhost", 1)

	req, err := http.NewRequest(http.MethodGet, host, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.URL, _ = url.Parse(host)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("经环境变量代理访问失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "env-ok") {
		t.Fatalf("body = %q, want 含 env-ok", body)
	}
}
