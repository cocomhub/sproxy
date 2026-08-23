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
// 宣告 serviceSpec）。返回 cleanup（Kill+Wait，sync.Once 保护）。
// 本测试只走 dial 帧分支（/api/relay/stream），不触发 HTTP 中继，故 --local 指向不存在的服务。
func startSClientMeshNode(t *testing.T, hubURL, nodeID, serviceSpec string) func() {
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
	leafCleanup := startSClientMeshNode(t, hubURL, "e2e-mesh-node", "echo:"+echoAddr)
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
