// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxy_test

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startSClientMeshNode 启动一个 sclient mesh node 常驻节点（出口模式，--dial-allow，
// 宣告 serviceSpec）。extraArgs 追加到命令行（如 --discover=false、--discover-interval 1s）。
// 返回 cleanup（Kill+Wait，sync.Once 保护）。本测试只走 dial 帧分支（/api/relay/stream），
// 不触发 HTTP 中继，故 --local 指向不存在的服务。
func startSClientMeshNode(t *testing.T, hubURL, nodeID, serviceSpec string, extraArgs ...string) func() {
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
		"mesh", "node",
		"--config", configPath,
		"--hub", wsURL,
		"--node-id", nodeID,
		"--token", "e2e-relay-token",
		"--service", serviceSpec,
		"--dial-allow",
		"--local", "http://127.0.0.1:1",
	}
	args = append(args, extraArgs...)
	cmd := exec.Command(binPath, args...)
	cmd.Dir = e2eModuleRoot()
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sclient mesh node: %v", err)
	}

	logStderrOnFailure(t, "sclient mesh node "+nodeID, &stderrBuf)

	// 等 mesh node 注册到 hub（waitNodeRegistered 轮询 /api/hub/nodes）。
	waitNodeRegistered(t, hubURL, nodeID, &stderrBuf, newKillWaitCleanup(cmd))
	return newKillWaitCleanup(cmd)
}

// startSClientMeshNodeObservable 同 startSClientMeshNode，但把 stderr 写入外部 buffer
// （供自动对等发现的可观测性断言：轮询 stderr 出现 "mesh 自动对等直连建立"）。
func startSClientMeshNodeObservable(t *testing.T, hubURL, nodeID, serviceSpec string, out *bytes.Buffer, extraArgs ...string) func() {
	t.Helper()
	tmpDir := t.TempDir()
	binPath := e2eBinPath(t, "cmd/sclient")

	configPath := filepath.Join(tmpDir, "sclient.yaml")
	if err := os.WriteFile(configPath, []byte("server_url: "+hubURL+"\n"), 0644); err != nil {
		t.Fatalf("write sclient config: %v", err)
	}
	wsURL := strings.Replace(hubURL, "http://", "ws://", 1) + "/ws"
	args := []string{
		"mesh", "node",
		"--config", configPath,
		"--hub", wsURL,
		"--node-id", nodeID,
		"--token", "e2e-relay-token",
		"--service", serviceSpec,
		"--dial-allow",
		"--local", "http://127.0.0.1:1",
	}
	args = append(args, extraArgs...)
	cmd := exec.Command(binPath, args...)
	cmd.Dir = e2eModuleRoot()
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sclient mesh node (obs): %v", err)
	}
	waitNodeRegistered(t, hubURL, nodeID, out, newKillWaitCleanup(cmd))
	return newKillWaitCleanup(cmd)
}

// TestE2E_MeshNode_RelayReachable 验证 mesh node 单进程常驻节点的中继可达性：
// hub + mesh node（--service 宣告 loopback echo，--dial-allow）+ mesh connect
// （--webrtc=false 确定性走中继回落）→ echo 数据面端到端就绪。
// 这是 mesh 自动组网"中转可达"的第一步：mesh node 取代 relay start 成为常驻出口节点。
func TestE2E_MeshNode_RelayReachable(t *testing.T) {
	hubURL, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	// 本地 echo 服务（mesh node 出口拨号的目标）。
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

	// mesh node：单进程常驻，宣告 echo 服务 + 出口拨号放行。
	// --discover=false：本测试只验证中继可达路径，隔离对 mesh connect 临时节点的空探测。
	leafCleanup := startSClientMeshNode(t, hubURL, "e2e-mesh-node", "echo:"+echoAddr, "--discover=false")
	defer leafCleanup()

	// mesh connect 端口转发（--webrtc=false 走中继回落，确定性验证 mesh node 中继路径）。
	listenAddr, meshCleanup := startSClientMeshConnect(t, hubURL, "echo")
	defer meshCleanup()

	// 就绪判定：全 echo 往返（I73）。全局 15s 上限（> relayStreamDialResultTimeout 12s）。
	payload := []byte("e2e-mesh-node-relay-reachable")
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
					return // 数据面端到端就绪（mesh node 中继路径）
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("mesh node 中继数据面未在 15s 内就绪（最后错误: %v）", lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestE2E_MeshNode_Discovery 验证 mesh node 自动对等发现：两个 mesh node 启动后，
// node-a 自动发现 node-b（经 hub 节点列表）并 webrtc 直连保持（进程级观测 stderr 日志）。
func TestE2E_MeshNode_Discovery(t *testing.T) {
	hubURL, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	// node-a 的 stderr 可观测（出现 "mesh 自动对等直连建立" + peer=node-b）。
	var stderrA bytes.Buffer
	cleanupA := startSClientMeshNodeObservable(t, hubURL, "e2e-disc-a", "", &stderrA, "--discover-interval", "1s")
	defer cleanupA()
	cleanupB := startSClientMeshNode(t, hubURL, "e2e-disc-b", "", "--discover-interval", "1s")
	defer cleanupB()

	// node-a < node-b 半拨号去重 → A 拨 B。轮询 stderr 出现自动直连建立（≤20s）。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) &&
		(!strings.Contains(stderrA.String(), "mesh 自动对等直连建立") || !strings.Contains(stderrA.String(), "peer=e2e-disc-b")) {
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(stderrA.String(), "mesh 自动对等直连建立") || !strings.Contains(stderrA.String(), "peer=e2e-disc-b") {
		t.Fatalf("node-a 未自动直连 node-b; stderr:\n%s", stderrA.String())
	}
}

// TestE2E_MeshNode_ServiceAccess 验证 mesh 完全服务互访的核心路径：
// hub + node-svc（宣告 echo + 出口拨号精确放行）+ node-ap（自动直连 node-svc，
// 本地网关 18085）+ mesh connect --gateway → 经已建直连链路路由到 node-svc 的 echo，
// 数据面端到端就绪（复用已建链路，零重新打洞）。
func TestE2E_MeshNode_ServiceAccess(t *testing.T) {
	hubURL, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	// 本地 echo 服务（node-svc 出口拨号的目标）。
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

	// node-ap（低 ID "e2e-ap"，访问方）：自动拨号 node-svc；网关 18085（默认）。
	// 可观测 stderr 等待自动直连建立。
	var stderrAP bytes.Buffer
	cleanupAP := startSClientMeshNodeObservable(t, hubURL, "e2e-ap", "", &stderrAP,
		"--discover-interval", "1s", "--gateway-addr", "127.0.0.1:18085")
	defer cleanupAP()

	// node-svc（服务宿主）：宣告 echo + --dial-allow；网关换端口避免同机冲突。
	cleanupSvc := startSClientMeshNode(t, hubURL, "e2e-svc", "echo:"+echoAddr,
		"--discover-interval", "1s", "--gateway-addr", "127.0.0.1:18086")
	defer cleanupSvc()

	// 等 node-ap 自动直连 node-svc（进程级 stderr 观测，≤20s）。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) &&
		(!strings.Contains(stderrAP.String(), "mesh 自动对等直连建立") || !strings.Contains(stderrAP.String(), "peer=e2e-svc")) {
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(stderrAP.String(), "mesh 自动对等直连建立") || !strings.Contains(stderrAP.String(), "peer=e2e-svc") {
		t.Fatalf("node-ap 未自动直连 node-svc; stderr:\n%s", stderrAP.String())
	}

	// mesh connect 经 node-ap 本地网关复用已建链路路由到 node-svc 的 echo。
	listenAddr, meshCleanup := startSClientMeshConnect(t, hubURL, "echo",
		"--gateway", "127.0.0.1:18085")
	defer meshCleanup()

	// 就绪判定：全 echo 往返（I73）。全局 15s 上限。
	payload := []byte("e2e-mesh-node-service-access")
	deadline = time.Now().Add(15 * time.Second)
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
					return // 数据面端到端就绪（复用已建链路路径）
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("mesh connect --gateway 数据面未在 15s 内就绪（最后错误: %v）", lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
