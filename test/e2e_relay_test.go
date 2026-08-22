// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package test 提供端到端测试：构建真实二进制并启动。
// 本文件验证 mesh 中继的三端闭环：hub(sproxy) + leaf(sclient relay start) + caller(relay dial)。
package sproxy_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// startHubSPROXY 启动一个启用了 hub 中继的 sproxy，返回 baseURL 与 cleanup。
func startHubSPROXY(t *testing.T) (string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	binName := "sproxy"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(currentFile))

	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/sproxy")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sproxy: %v\n%s", err, out)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := l.Addr().String()
	l.Close() //nolint:staticcheck

	uploadsDir := filepath.Join(tmpDir, "uploads")
	_ = os.MkdirAll(uploadsDir, 0755)

	configPath := filepath.Join(tmpDir, "hub.yaml")
	configContent := []byte(`tls:
  enabled: false
tunnel_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
hub:
  enabled: true
  relay_token: "e2e-relay-token"
  transports:
    ws:
      enabled: true
      path: /ws
`)
	if err := os.WriteFile(configPath, configContent, 0644); err != nil {
		t.Fatalf("write hub config: %v", err)
	}
	cmd := exec.Command(binPath, "--addr", addr, "--uploads-dir", uploadsDir, "--config", configPath)
	cmd.Dir = moduleRoot
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hub sproxy: %v", err)
	}
	baseURL := fmt.Sprintf("http://%s", addr)

	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "OK" {
				ready = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("hub sproxy not ready; stderr:\n%s", stderrBuf.String())
	}

	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return baseURL, cleanup
}

// startSClientRelay 启动一个 sclient relay start 叶子节点，返回其 node-id 与 cleanup。
// localAddr 是叶子转发的本地 HTTP 服务地址。
func startSClientRelay(t *testing.T, hubURL, nodeID, localAddr string) func() {
	t.Helper()
	tmpDir := t.TempDir()
	binName := "sclient"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(currentFile))

	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/sclient")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sclient: %v\n%s", err, out)
	}

	wsURL := strings.Replace(hubURL, "http://", "ws://", 1) + "/ws"
	args := []string{
		"relay", "start",
		"--hub", wsURL,
		"--node-id", nodeID,
		"--token", "e2e-relay-token",
		"--local", localAddr,
		"--dial-allow",                     // 允许叶子出站拨号（供 caller 经 /api/relay/stream 中继）
		"--dial-allow-cidr", "127.0.0.0/8", // E2E 用回环 echo，放行回环网段
	}
	cmd := exec.Command(binPath, args...)
	cmd.Dir = moduleRoot
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sclient relay: %v", err)
	}

	// 等待注册 ACK 完成（relay start 会打印「已注册到 Hub」）
	time.Sleep(1 * time.Second)
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// TestE2E_RelayStream 验证三端闭环：
// hub(sproxy with hub.enabled) + leaf(sclient relay start 注册) + caller(原始 CONNECT 经 /api/relay/stream)
// 叶子把中继请求转发到本地 echo 服务。
func TestE2E_RelayStream(t *testing.T) {
	hubURL, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	// 本地 echo 服务（叶子转发的目标）
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = io.Copy(cn, cn) // echo
			}(c)
		}
	}()
	localAddr := "http://" + echoLn.Addr().String()

	// 启动 leaf 节点
	leafCleanup := startSClientRelay(t, hubURL, "e2e-leaf", localAddr)
	defer leafCleanup()

	// caller：原始 CONNECT 风格经 hub /api/relay/stream 中继到 leaf，
	// 由 leaf 出站拨号到本地 echo（--dial-allow 出口模式）。
	host := strings.TrimPrefix(hubURL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	reqBody := fmt.Sprintf(`{"target":"e2e-leaf","type":"tcp","addr":"%s"}`, echoLn.Addr().String())
	fmt.Fprintf(conn, "POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", host, len(reqBody), reqBody)
	br := bufio.NewReader(conn)
	statusLine, rerr := br.ReadString('\n')
	if rerr != nil {
		t.Fatalf("read status: %v", rerr)
	}
	if !strings.Contains(statusLine, " 200 ") {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		t.Fatalf("hub 返回 %s%s", strings.TrimSpace(statusLine), rest)
	}
	// 读头直到空行
	for {
		line, lerr := br.ReadString('\n')
		if lerr != nil {
			t.Fatal(lerr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// 现在 conn 是纯字节流：写 payload 读回 echo（验证三端转发）
	payload := []byte("e2e-relay-payload")
	if _, werr := conn.Write(payload); werr != nil {
		t.Fatal(werr)
	}
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(conn, got); rerr != nil {
		t.Fatalf("read echo: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}
}

// startSClientRelayService 启动一个宣告 --service 的 relay start 出口叶子节点。
// 与 startSClientRelay 不同：不传 --dial-allow-cidr，依赖 --service 宣告的地址
// 被出口拨号策略精确放行（回归：loopback 服务无需额外 CIDR 白名单）。
func startSClientRelayService(t *testing.T, hubURL, nodeID, serviceSpec string) func() {
	t.Helper()
	tmpDir := t.TempDir()
	binName := "sclient"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(currentFile))

	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/sclient")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sclient: %v\n%s", err, out)
	}

	wsURL := strings.Replace(hubURL, "http://", "ws://", 1) + "/ws"
	args := []string{
		"relay", "start",
		"--hub", wsURL,
		"--node-id", nodeID,
		"--token", "e2e-relay-token",
		"--service", serviceSpec,
		"--dial-allow",
	}
	cmd := exec.Command(binPath, args...)
	cmd.Dir = moduleRoot
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sclient relay: %v", err)
	}
	time.Sleep(1 * time.Second)
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// startSClientMeshConnect 启动 sclient mesh connect 端口转发，返回转发监听地址与 cleanup。
// --webrtc=false 跳过信令，直测中继回落路径（出口叶子必须能拨自己宣告的服务地址）。
func startSClientMeshConnect(t *testing.T, hubURL, service string) (string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	binName := "sclient"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(currentFile))

	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/sclient")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sclient: %v\n%s", err, out)
	}

	// 配置隔离：--config 指向临时配置（避免加载本地 ~/.config/sproxy/sclient.yaml）
	configPath := filepath.Join(tmpDir, "sclient.yaml")
	if err := os.WriteFile(configPath, []byte("server_url: "+hubURL+"\n"), 0644); err != nil {
		t.Fatalf("write sclient config: %v", err)
	}
	// 预留转发端口（mesh connect 内部会再次绑定同一地址）
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenAddr := l.Addr().String()
	l.Close() //nolint:staticcheck

	args := []string{
		"mesh", "connect", service,
		"--config", configPath,
		"--listen", listenAddr,
		"--webrtc=false",
	}
	cmd := exec.Command(binPath, args...)
	cmd.Dir = moduleRoot
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mesh connect: %v", err)
	}
	return listenAddr, func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// TestE2E_MeshConnect_AnnouncedService 验证 mesh connect 全链路：
// hub + 出口叶子（--service 宣告 loopback echo，**无 --dial-allow-cidr**）+
// mesh connect 端口转发（--webrtc=false 走中继回落）。
// 回归：出口拨号策略必须精确放行自己宣告的服务地址（用户场景：relay start
// --service ssh:127.0.0.1:10022 --dial-allow，mesh connect 回落中继时出口需拨
// 127.0.0.1:10022）。
func TestE2E_MeshConnect_AnnouncedService(t *testing.T) {
	hubURL, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	// 本地 echo 服务（出口叶子要出站拨号的目标）
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = io.Copy(cn, cn) // echo
			}(c)
		}
	}()
	echoAddr := echoLn.Addr().String()

	// 出口叶子：宣告 echo:127.0.0.1:PORT（loopback），无 --dial-allow-cidr
	leafCleanup := startSClientRelayService(t, hubURL, "e2e-exit", "echo:"+echoAddr)
	defer leafCleanup()

	// mesh connect 端口转发
	listenAddr, meshCleanup := startSClientMeshConnect(t, hubURL, "echo")
	defer meshCleanup()

	// 轮询拨号直到 mesh 转发就绪（叶子注册 + mesh connect 启动有延迟）
	var conn net.Conn
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err = net.Dial("tcp", listenAddr)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connect to mesh forward: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	defer conn.Close()

	// 写 payload 读回 echo，验证「本地 ⇄ hub ⇄ 出口叶子出站」全链路
	payload := []byte("e2e-mesh-announced-service")
	if _, werr := conn.Write(payload); werr != nil {
		t.Fatal(werr)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(conn, got); rerr != nil {
		t.Fatalf("read echo: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}
}
