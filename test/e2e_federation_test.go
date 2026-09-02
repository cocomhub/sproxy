// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxy_test

import (
	"bufio"
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
	"testing"
	"time"
)

// buildSPROXYBin 构建 sproxy 二进制到临时目录，返回路径（与 startSPROXY 一致）。
func buildSPROXYBin(t *testing.T) string {
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
	return binPath
}

// freeLoopbackAddr 返回一个可用的 127.0.0.1 端口地址（监听后立即关闭）。
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// startFederatedHub 启动一个带 hub + 联邦配置的 sproxy 进程，返回 baseURL 与清理函数。
// peerURL 非空时配置本 hub 向该对端拉取（federation.peers）；为空则本 hub 仅作为被 peer。
func startFederatedHub(t *testing.T, binPath, name string, peerURL string) (string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	addr := freeLoopbackAddr(t)
	uploadsDir := filepath.Join(tmpDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		t.Fatalf("create uploads dir: %v", err)
	}
	var peersYAML string
	if peerURL != "" {
		peersYAML = fmt.Sprintf("    interval: \"1s\"\n    timeout: \"3s\"\n    peers:\n      - id: \"peer-%s\"\n        url: %q\n        access_key: %q\n        access_key_secret: %q\n",
			name, peerURL, e2eTestAK, e2eTestSK)
	} else {
		peersYAML = "    interval: \"1s\"\n"
	}
	configContent := fmt.Sprintf(`tls:
  enabled: false
access_keys:
  - key: %q
    secret: %q
hub:
  enabled: true
  transports:
    ws:
      enabled: true
  federation:
    enabled: true
%s
`, e2eTestAK, e2eTestSK, peersYAML)
	configPath := filepath.Join(tmpDir, name+".yaml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("write config %s: %v", name, err)
	}
	cmd := exec.Command(binPath, "--addr", addr, "--storage-root", uploadsDir, "--config", configPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sproxy %s: %v", name, err)
	}
	baseURL := fmt.Sprintf("http://%s", addr)
	cleanup := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	// 轮询 healthz 就绪（5s 超时）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return baseURL, cleanup
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	cleanup()
	t.Fatalf("sproxy %s 未在超时内就绪", name)
	return "", nil
}

// wsURL 把 http://host:port 转为 ws://host:port/ws（hub 注册端点）。
func wsURL(base string) string {
	return "ws://" + base[len("http://"):] + "/ws"
}

// waitNodeVisible 轮询 /api/hub/nodes（带 SproxySig 签名）直到出现指定 node-id。
func waitNodeVisible(t *testing.T, baseURL, nodeID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastBody string
	for time.Now().Before(deadline) {
		resp, err := authedHTTPClient.Get(baseURL + "/api/hub/nodes")
		if err == nil {
			var nodes []struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&nodes)
			_ = resp.Body.Close()
			for _, n := range nodes {
				if n.ID == nodeID {
					return
				}
			}
			lastBody = fmt.Sprintf("%+v", nodes)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("节点 %s 未在 %s 内出现在 %s 的节点表, last=%s", nodeID, timeout, baseURL, lastBody)
}

// TestE2E_DualHubFederation（DoD 1 CLI 级）：真实二进制双 hub peering——
// hub-A 联邦拉取 hub-B，node-b 经 sclient relay 注册到 hub-B 后，
// hub-A 的 /api/hub/nodes 出现 node-b（联邦候选同步生效）。
func TestE2E_DualHubFederation(t *testing.T) {
	binPath := buildSPROXYBin(t)

	// hub-B：被拉取方（不主动拉取）。
	hubB, cleanupB := startFederatedHub(t, binPath, "hubB", "")
	t.Cleanup(cleanupB)
	// hub-A：拉取方，peer 指向 hub-B。
	hubA, cleanupA := startFederatedHub(t, binPath, "hubA", hubB)
	t.Cleanup(cleanupA)

	// sclient relay 连接 hub-B 注册 node-b。
	sclient := buildSClient(t)
	relayCmd := exec.Command(sclient, "relay", "start",
		"--hub", wsURL(hubB),
		"--node-id", "node-b",
		"--access-key", e2eTestAK,
		"--access-key-secret", e2eTestSK,
	)
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start sclient relay: %v", err)
	}
	t.Cleanup(func() {
		_ = relayCmd.Process.Kill()
		_, _ = relayCmd.Process.Wait()
	})

	// hub-B 本地出现 node-b（注册成功）。
	waitNodeVisible(t, hubB, "node-b", 10*time.Second)
	// hub-A 经联邦拉取（1s 周期）看到 node-b。
	waitNodeVisible(t, hubA, "node-b", 15*time.Second)
}

// TestE2E_CrossHubRelay（DoD 1 CLI 级）：真实二进制双 hub 链式中继——
// caller → hub-A →（联邦转发）→ hub-B → node-b 叶子 → 本地 echo。
// 验证「A→hub1→hub2→B 数据往返」在真实进程下成立。
func TestE2E_CrossHubRelay(t *testing.T) {
	binPath := buildSPROXYBin(t)

	// 本地 echo 服务（node-b 叶子转发的目标）。
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echoLn.Close() })
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

	// hub-B：被拉取方（不主动拉取）。
	hubB, cleanupB := startFederatedHub(t, binPath, "hubB", "")
	t.Cleanup(cleanupB)
	// hub-A：拉取方，peer 指向 hub-B（联邦同步 1s 周期）。
	hubA, cleanupA := startFederatedHub(t, binPath, "hubA", hubB)
	t.Cleanup(cleanupA)

	// node-b 叶子：sclient relay start 注册到 hub-B（出口模式，放行回环）。
	sclient := buildSClient(t)
	relayCmd := exec.Command(sclient, "relay", "start",
		"--hub", wsURL(hubB),
		"--node-id", "node-b",
		"--access-key", e2eTestAK,
		"--access-key-secret", e2eTestSK,
		"--dial-allow",
		"--dial-allow-cidr", "127.0.0.0/8",
	)
	if startErr := relayCmd.Start(); startErr != nil {
		t.Fatalf("start sclient relay: %v", startErr)
	}
	t.Cleanup(func() {
		_ = relayCmd.Process.Kill()
		_, _ = relayCmd.Process.Wait()
	})

	// hub-B 本地出现 node-b（注册成功），hub-A 经联邦（1s 周期）看到 node-b。
	waitNodeVisible(t, hubB, "node-b", 10*time.Second)
	waitNodeVisible(t, hubA, "node-b", 15*time.Second)

	// caller：经 hub-A 中继到 node-b（hub-A 联邦转发 → hub-B → 叶子）。
	host := strings.TrimPrefix(hubA, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial hub-A: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	reqBody := fmt.Sprintf(`{"target":"node-b","type":"tcp","addr":"%s"}`, echoLn.Addr().String())
	auth := sproxySigHeader(e2eTestAK, e2eTestSK, "POST", "/api/relay/stream", "", []byte(reqBody))
	fmt.Fprintf(conn, "POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nAuthorization: %s\r\nContent-Length: %d\r\n\r\n%s", host, auth, len(reqBody), reqBody)
	br := bufio.NewReader(conn)
	statusLine, rerr := br.ReadString('\n')
	if rerr != nil {
		t.Fatalf("read status: %v", rerr)
	}
	if !strings.Contains(statusLine, " 200 ") {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		t.Fatalf("跨 hub 中继应 200，实际 %s%s", strings.TrimSpace(statusLine), rest)
	}
	for {
		line, lerr := br.ReadString('\n')
		if lerr != nil {
			t.Fatal(lerr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// 数据往返：写 payload 读回 echo（A→hub1→hub2→B 链路）。
	payload := []byte("cross-hub-e2e-payload")
	if _, werr := conn.Write(payload); werr != nil {
		t.Fatalf("写 payload: %v", werr)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(conn, got); rerr != nil {
		t.Fatalf("读 echo（跨 hub 链路）: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo 不匹配: got %q want %q", got, payload)
	}
}
