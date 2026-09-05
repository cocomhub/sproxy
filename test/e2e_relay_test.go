// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package test 提供端到端测试：构建真实二进制并启动。
// 本文件验证 mesh 中继的三端闭环：hub(sproxy) + leaf(sclient relay start) +
// caller(原始 CONNECT / mesh connect CLI)。
package sproxy_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// e2eModuleRoot 返回 sproxy module 根目录（本文件位于 test/，上级即 module 根）。
func e2eModuleRoot() string {
	_, currentFile, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(currentFile))
}

// ---- 共享二进制构建（S110）----

var (
	e2eBinOnce sync.Once
	e2eBinDir  string
	e2eBinErr  error
)

// e2eBinPath 返回指定 cmd 子路径（"cmd/sproxy" / "cmd/sclient"）的已构建二进制路径。
// 包级 sync.Once + os.Stat 保证同一二进制在整包测试中只构建一次（S110），
// 替代原先每个 helper 各 build 一次造成的重复 go build 子进程。
func e2eBinPath(t *testing.T, cmdPath string) string {
	t.Helper()
	e2eBinOnce.Do(func() {
		e2eBinDir, e2eBinErr = os.MkdirTemp("", "sproxy-e2e-bin")
	})
	if e2eBinErr != nil {
		t.Fatalf("create e2e bin dir: %v", e2eBinErr)
	}
	name := filepath.Base(cmdPath)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binPath := filepath.Join(e2eBinDir, name)
	if _, err := os.Stat(binPath); err == nil {
		return binPath
	}
	buildCmd := exec.Command("go", "build", "-o", binPath, "./"+cmdPath)
	buildCmd.Dir = e2eModuleRoot()
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", cmdPath, err, out)
	}
	return binPath
}

// newKillWaitCleanup 构造以 sync.Once 保护的 Kill+Wait cleanup。
// 避免「Fatal 路径先 Kill+Wait（如 waitNodeRegistered 超时）」与 defer cleanup
// 双 Wait 导致的 data race。
func newKillWaitCleanup(cmd *exec.Cmd) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
}

// e2eHTTPClient 是 hub 探测用的短超时客户端，避免轮询被挂起请求卡死。
var e2eHTTPClient = &http.Client{Timeout: 2 * time.Second}

// signedHubGET 用 SproxySig 签名 GET 请求 hub API（access_keys 配置后全 HTTP 面验签）。
func signedHubGET(baseURL, path, ak, sk string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	sproxysig.SignRequest(req, ak, sk)
	return e2eHTTPClient.Do(req)
}

// sproxySigHeader 构造 SproxySig Authorization 头值（带 body 哈希；原始 socket 请求用）。
func sproxySigHeader(ak, sk, method, path, query string, body []byte) string {
	now := time.Now()
	h := sproxysig.Header{
		Version: sproxysig.Version, AK: ak,
		TS: now.UnixMilli(), Exp: now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce: sproxysig.NewNonce(), BodySHA256: sproxysig.BodyHash(body),
	}
	return sproxysig.SignAndFormat(sk, h, method, path, query)
}

// hubNodesOK 报告 /api/hub/nodes 是否已装配并返回合法 JSON 数组
// （hub 路由就绪，S116；routeTable nil 时该路由返回 404）。
func hubNodesOK(baseURL, ak, sk string) bool {
	resp, err := signedHubGET(baseURL, "/api/hub/nodes", ak, sk)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var nodes []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return false
	}
	return nodes != nil
}

// hubNodeRegistered 查询 /api/hub/nodes 判断 nodeID 是否已在 hub 注册。
func hubNodeRegistered(baseURL, nodeID, ak, sk string) bool {
	resp, err := signedHubGET(baseURL, "/api/hub/nodes", ak, sk)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var nodes []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return false
	}
	for _, n := range nodes {
		if n.ID == nodeID {
			return true
		}
	}
	return false
}

// stderrSink 是子进程 stderr 的读接口：*bytes.Buffer 与 lockedBuffer 均可传入
// （mesh node 测试用带锁缓冲防止 -race 竞争标记）。
type stderrSink interface {
	io.Writer
	String() string
}

// waitNodeRegistered 轮询 /api/hub/nodes 直到 nodeID 出现（I72，替代固定 sleep）。
// 10s 对齐 registerAckTimeout；超时则 Kill+Wait 并打印 sclient stderr 后失败（S112）。
// killWait 由调用方传入（sync.Once 保护），超时路径与 defer cleanup 共享同一 Wait。
func waitNodeRegistered(t *testing.T, hubURL, nodeID, ak, sk string, stderrBuf stderrSink, killWait func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if hubNodeRegistered(hubURL, nodeID, ak, sk) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	killWait()
	t.Fatalf("sclient relay %s 未在 10s 内注册; stderr:\n%s", nodeID, stderrBuf.String())
}

// logStderrOnFailure 注册 cleanup：测试失败时打印子进程 stderr（S112）。
func logStderrOnFailure(t *testing.T, label string, stderrBuf stderrSink) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s stderr:\n%s", label, stderrBuf.String())
		}
	})
}

// startHubSPROXY 启动一个启用了 hub 中继 + SproxySig access_keys 的 sproxy，
// 返回 baseURL、生成的 AK/SK 与 cleanup。hub 注册准入由顶层 access_keys 提供
// （SproxySig AccessKey + HMAC proof），relay_token 已废除。
func startHubSPROXY(t *testing.T) (string, string, string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	binPath := e2eBinPath(t, "cmd/sproxy")

	// S115：端口分配采用「bind 端口 0 取空闲端口 → close → 子进程再绑定」的参考模式
	// （与 e2e_test.go:startSPROXY 一致）。close 到子进程 bind 之间的 TOCTOU 窗口极小，
	// 且由 healthz + /api/hub/nodes 就绪轮询兜底（端口被占则子进程退出、就绪检查失败）。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := l.Addr().String()
	l.Close() //nolint:staticcheck

	ak, sk := generateTestAccessKeyPair(t)
	uploadsDir := filepath.Join(tmpDir, "uploads")
	_ = os.MkdirAll(uploadsDir, 0755)
	// 凭据 store 化：access_keys 不再装配 Ring，须 pre-seed 使 ak 被识别。
	seedCredentialStore(t, uploadsDir, ak, sk)

	configPath := filepath.Join(tmpDir, "hub.yaml")
	configContent := fmt.Sprintf(`tls:
  enabled: false
hub:
  enabled: true
  transports:
    ws:
      enabled: true
      path: /ws
access_keys:
  - key: %q
    secret: %q
`, ak, sk)
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("write hub config: %v", err)
	}
	cmd := exec.Command(binPath, "--addr", addr, "--storage-root", uploadsDir, "--config", configPath)
	cmd.Dir = e2eModuleRoot()
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hub sproxy: %v", err)
	}
	baseURL := fmt.Sprintf("http://%s", addr)
	cleanup := newKillWaitCleanup(cmd)

	// 就绪门：healthz（HTTP 层）+ /api/hub/nodes（hub 路由装配，S116）。
	// /ws accept 循环就绪由各 relay helper 的注册等待（waitNodeRegistered）间接证明。
	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "OK" && hubNodesOK(baseURL, ak, sk) {
				ready = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		cleanup()
		t.Fatalf("hub sproxy not ready; stderr:\n%s", stderrBuf.String())
	}

	return baseURL, ak, sk, cleanup
}

// startSClientRelay 启动一个 sclient relay start 叶子节点（出口模式，--dial-allow）。
// 返回 cleanup（Kill+Wait，sync.Once 保护）。nodeID 是注册到 hub 的节点 ID。
// 本测试只走 dial 帧分支（/api/relay/stream），不触发 HTTP 中继，故不传 --local（S113）。
func startSClientRelay(t *testing.T, hubURL, nodeID, ak, sk string) func() {
	t.Helper()
	tmpDir := t.TempDir()
	binPath := e2eBinPath(t, "cmd/sclient")

	// 配置隔离（I74）：--config 指向临时配置，避免加载本地 ~/.config/sproxy/sclient.yaml。
	configPath := filepath.Join(tmpDir, "sclient.yaml")
	if err := os.WriteFile(configPath, []byte("server_url: "+hubURL+"\n"), 0644); err != nil {
		t.Fatalf("write sclient config: %v", err)
	}

	wsURL := strings.Replace(hubURL, "http://", "ws://", 1) + "/ws"
	args := []string{
		"relay", "start",
		"--config", configPath,
		"--hub", wsURL,
		"--node-id", nodeID,
		"--access-key", ak,
		"--access-key-secret", sk,
		"--dial-allow",                     // 允许叶子出站拨号（供 caller 经 /api/relay/stream 中继）
		"--dial-allow-cidr", "127.0.0.0/8", // E2E 用回环 echo，放行回环网段
	}
	cmd := exec.Command(binPath, args...)
	cmd.Dir = e2eModuleRoot()
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sclient relay: %v", err)
	}

	logStderrOnFailure(t, "sclient relay "+nodeID, &stderrBuf)
	killWait := newKillWaitCleanup(cmd)
	// 等待注册完成（I72）：轮询 /api/hub/nodes，替代固定 sleep。
	waitNodeRegistered(t, hubURL, nodeID, ak, sk, &stderrBuf, killWait)
	return killWait
}

// TestE2E_RelayStream 验证三端闭环：
// hub(sproxy with hub.enabled) + leaf(sclient relay start 注册) + caller(原始 CONNECT 经 /api/relay/stream)
// 叶子把中继请求转发到本地 echo 服务。
func TestE2E_RelayStream(t *testing.T) {
	hubURL, ak, sk, hubCleanup := startHubSPROXY(t)
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

	// 启动 leaf 节点
	leafCleanup := startSClientRelay(t, hubURL, "e2e-leaf", ak, sk)
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
	// access_keys 配置后 /api/relay/stream 需 SproxySig 签名（带 body 哈希）。
	auth := sproxySigHeader(ak, sk, "POST", "/api/relay/stream", "", []byte(reqBody))
	fmt.Fprintf(conn, "POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nAuthorization: %s\r\nContent-Length: %d\r\n\r\n%s", host, auth, len(reqBody), reqBody)
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

	// 现在 conn 是纯字节流：写 payload 读回 echo（验证三端转发）。
	// I71：echo 读加 5s deadline（对齐 mesh 测试），防止叶子 pump 卡死时挂满 10 分钟。
	payload := []byte("e2e-relay-payload")
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

// TestE2E_RelayStream_UnknownTarget 验证中继到未知节点时 hub 返回 404（S114）。
func TestE2E_RelayStream_UnknownTarget(t *testing.T) {
	hubURL, ak, sk, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	host := strings.TrimPrefix(hubURL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	reqBody := `{"target":"no-such-node","type":"tcp","addr":"127.0.0.1:1"}`
	auth := sproxySigHeader(ak, sk, "POST", "/api/relay/stream", "", []byte(reqBody))
	fmt.Fprintf(conn, "POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nAuthorization: %s\r\nContent-Length: %d\r\n\r\n%s", host, auth, len(reqBody), reqBody)
	br := bufio.NewReader(conn)
	statusLine, rerr := br.ReadString('\n')
	if rerr != nil {
		t.Fatalf("read status: %v", rerr)
	}
	if !strings.Contains(statusLine, " 404 ") {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		t.Fatalf("未知节点应返回 404，实际 %s%s", strings.TrimSpace(statusLine), rest)
	}
}

// TestE2E_RelayStream_DialRefused 验证叶子出口拨号失败（目标端口关闭）时 hub 返回
// 502（S114；B4 DialResultFrames error 帧路径）。
func TestE2E_RelayStream_DialRefused(t *testing.T) {
	hubURL, ak, sk, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	// 叶子节点（--dial-allow-cidr 放行回环；--dial-allow 触发 DialResultFrames 回帧）
	leafCleanup := startSClientRelay(t, hubURL, "e2e-leaf-refused", ak, sk)
	defer leafCleanup()

	// 取一个空闲端口后立即关闭，作为「已关闭端口」的拨号目标。
	// S115：bind+close 到叶子拨号之间存在极小 TOCTOU 窗口，回环上被其他进程抢占的
	// 概率可忽略；若极端撞上，叶子拨号「成功」到他人端口会使 502 变 200，测试暴露。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := l.Addr().String()
	l.Close() //nolint:staticcheck

	host := strings.TrimPrefix(hubURL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	reqBody := fmt.Sprintf(`{"target":"e2e-leaf-refused","type":"tcp","addr":"%s"}`, closedAddr)
	auth := sproxySigHeader(ak, sk, "POST", "/api/relay/stream", "", []byte(reqBody))
	fmt.Fprintf(conn, "POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nAuthorization: %s\r\nContent-Length: %d\r\n\r\n%s", host, auth, len(reqBody), reqBody)
	br := bufio.NewReader(conn)
	statusLine, rerr := br.ReadString('\n')
	if rerr != nil {
		t.Fatalf("read status: %v", rerr)
	}
	if !strings.Contains(statusLine, " 502 ") {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		t.Fatalf("叶子拨号失败应返回 502，实际 %s%s", strings.TrimSpace(statusLine), rest)
	}
}

// startSClientRelayService 启动一个宣告 --service 的 relay start 出口叶子节点。
// 与 startSClientRelay 不同：不传 --dial-allow-cidr，依赖 --service 宣告的地址
// 被出口拨号策略精确放行（回归：loopback 服务无需额外 CIDR 白名单）。
func startSClientRelayService(t *testing.T, hubURL, nodeID, serviceSpec, ak, sk string) func() {
	t.Helper()
	tmpDir := t.TempDir()
	binPath := e2eBinPath(t, "cmd/sclient")

	// 配置隔离（I74）：--config 指向临时配置，避免加载本地 ~/.config/sproxy/sclient.yaml。
	configPath := filepath.Join(tmpDir, "sclient.yaml")
	if err := os.WriteFile(configPath, []byte("server_url: "+hubURL+"\n"), 0644); err != nil {
		t.Fatalf("write sclient config: %v", err)
	}

	wsURL := strings.Replace(hubURL, "http://", "ws://", 1) + "/ws"
	args := []string{
		"relay", "start",
		"--config", configPath,
		"--hub", wsURL,
		"--node-id", nodeID,
		"--access-key", ak,
		"--access-key-secret", sk,
		"--service", serviceSpec,
		"--dial-allow",
	}
	cmd := exec.Command(binPath, args...)
	cmd.Dir = e2eModuleRoot()
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sclient relay: %v", err)
	}

	logStderrOnFailure(t, "sclient relay service "+nodeID, &stderrBuf)
	killWait := newKillWaitCleanup(cmd)
	// 等待注册完成（I72）：轮询 /api/hub/nodes，替代固定 sleep。
	waitNodeRegistered(t, hubURL, nodeID, ak, sk, &stderrBuf, killWait)
	return killWait
}

// startSClientMeshConnect 启动 sclient mesh connect 端口转发，返回转发监听地址与 cleanup。
// --webrtc=false 跳过信令，直测中继回落路径（出口叶子必须能拨自己宣告的服务地址）。
// extraArgs 追加到命令行（如 --gateway 127.0.0.1:18085 测复用已建链路）。
func startSClientMeshConnect(t *testing.T, hubURL, service, ak, sk string, extraArgs ...string) (string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	binPath := e2eBinPath(t, "cmd/sclient")

	// 配置隔离：--config 指向临时配置（避免加载本地 ~/.config/sproxy/sclient.yaml）
	configPath := filepath.Join(tmpDir, "sclient.yaml")
	if err := os.WriteFile(configPath, []byte("server_url: "+hubURL+"\n"), 0644); err != nil {
		t.Fatalf("write sclient config: %v", err)
	}
	// S115：端口分配采用「bind 端口 0 → close → 子进程再绑定」参考模式；TOCTOU 窗口
	// 极小，且由调用方（TestE2E_MeshConnect_AnnouncedService）的 echo 往返就绪轮询兜底。
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
		"--access-key", ak,
		"--access-key-secret", sk,
		"--webrtc=false",
	}
	args = append(args, extraArgs...)
	cmd := exec.Command(binPath, args...)
	cmd.Dir = e2eModuleRoot()
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mesh connect: %v", err)
	}

	logStderrOnFailure(t, "sclient mesh connect", &stderrBuf)
	return listenAddr, newKillWaitCleanup(cmd)
}

// TestE2E_MeshConnect_AnnouncedService 验证 mesh connect 全链路：
// hub + 出口叶子（--service 宣告 loopback echo，**无 --dial-allow-cidr**）+
// mesh connect 端口转发（--webrtc=false 走中继回落）。
// 回归：出口拨号策略必须精确放行自己宣告的服务地址（用户场景：relay start
// --service ssh:127.0.0.1:10022 --dial-allow，mesh connect 回落中继时出口需拨
// 127.0.0.1:10022）。
func TestE2E_MeshConnect_AnnouncedService(t *testing.T) {
	hubURL, ak, sk, hubCleanup := startHubSPROXY(t)
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
	leafCleanup := startSClientRelayService(t, hubURL, "e2e-exit", "echo:"+echoAddr, ak, sk)
	defer leafCleanup()

	// mesh connect 端口转发
	listenAddr, meshCleanup := startSClientMeshConnect(t, hubURL, "echo", ak, sk)
	defer meshCleanup()

	// I73：就绪判定升级为「全 echo 往返」。每个尝试建立一条新的 mesh 连接
	// （net.Dial → meshForwardListen → 中继到出口叶子出站），写 payload 并带
	// 2s deadline 读回 echo；匹配成功即代表数据面（本地 ⇄ hub ⇄ 出口叶子）端到端
	// 就绪，而非仅本地 listener 就绪。全局 15s 上限（> relayStreamDialResultTimeout 12s）。
	payload := []byte("e2e-mesh-announced-service")
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for {
		conn, derr := net.Dial("tcp", listenAddr)
		if derr != nil {
			lastErr = derr
		} else {
			_, werr := conn.Write(payload)
			if werr != nil {
				lastErr = werr
				conn.Close()
			} else {
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				got := make([]byte, len(payload))
				_, rerr := io.ReadFull(conn, got)
				conn.Close()
				switch {
				case rerr != nil:
					lastErr = rerr
				case string(got) != string(payload):
					lastErr = fmt.Errorf("echo mismatch: got %q want %q", got, payload)
				default:
					return // 数据面端到端就绪，测试完成
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("mesh 数据面未在 15s 内就绪（最后错误: %v）", lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
