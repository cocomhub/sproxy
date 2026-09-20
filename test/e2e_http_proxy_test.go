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
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
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

	"github.com/cocomhub/sproxy/pkg/netutil"
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
	tr := netutil.IsolatedTransport()
	tr.Proxy = func(*http.Request) (*url.URL, error) { return url.Parse("http://" + proxyAddr) }
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
	trNoAuth := netutil.IsolatedTransport()
	trNoAuth.Proxy = func(*http.Request) (*url.URL, error) { return url.Parse("http://" + proxyAddr) }
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
	trAuth := netutil.IsolatedTransport()
	trAuth.Proxy = func(*http.Request) (*url.URL, error) { return url.Parse(authProxyURL) }
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

// TestE2E_HTTPProxy_NoProxyEnv 验证标准代理协议形态（env 客户端开箱即用）：
// 设 http_proxy 指向代理的客户端（curl --proxy / Go ProxyFromEnvironment）发出的是
// 绝对 URI 请求（HTTP）与 CONNECT（HTTPS）——本用例用裸 TCP 直接发送绝对 URI 请求行
// 验证代理对该标准形态的处理：代理启用 Basic 认证时，不带 Proxy-Authorization 必 407、
// 带正确 Basic 凭据放行。
//
// 注（Go 源码实证）：ProxyFromEnvironment 的 useProxy 对 host=="localhost"（
// httpproxy/proxy.go:178）与 loopback IP（:185 IsLoopback）**都返回不走代理**——
// 回环目标恒直连，无法用回环 host 验证 env 生效；裸请求形态等价 env 客户端请求，
// 且 407 是「请求确实抵达代理」的确定性证据。
func TestE2E_HTTPProxy_NoProxyEnv(t *testing.T) {
	proxyAddr, cleanup := startHTTPProxyLongRunning(t, "--proxy-user", "u", "--proxy-pass", "p")
	defer cleanup()

	// 目标：代理本地可达的任意地址（绝对 URI 由代理转发；目标无需回环特例）。
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "env-ok")
	}))
	defer target.Close()
	targetHost := strings.TrimPrefix(target.URL, "http://")

	// 不带认证 → 407（请求抵达代理且被认证拒绝，env 生效的确定性证据）。
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("连接代理: %v", err)
	}
	defer conn.Close()
	// 绝对 URI 请求行（RFC 7230 §5.3.2）——env 客户端（curl --proxy / Go
	// ProxyFromEnvironment）发出的就是这种形态。
	fmt.Fprintf(conn, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", targetHost, targetHost)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读代理响应: %v", err)
	}
	if !strings.Contains(status, "407") {
		t.Fatalf("未认证状态 = %q, want 407（http_proxy 未生效或请求未抵达代理）", status)
	}

	// 带正确 Basic 凭据 → 200 + body（新连接，Proxy-Authorization 头）。
	conn2, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("连接代理: %v", err)
	}
	defer conn2.Close()
	creds := base64.StdEncoding.EncodeToString([]byte("u:p"))
	fmt.Fprintf(conn2, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n",
		targetHost, targetHost, creds)
	br2 := bufio.NewReader(conn2)
	resp, err := http.ReadResponse(br2, nil)
	if err != nil {
		t.Fatalf("读代理响应: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带认证状态 = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "env-ok") {
		t.Fatalf("body = %q, want 含 env-ok", body)
	}
}
