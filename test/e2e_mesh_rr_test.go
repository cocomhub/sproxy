// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 阶段 5 工作项 2 / PR-2：mesh 多副本 round-robin + 单点故障回退 e2e 回归。
// 依赖 PR-1（MeshTargetRefresher.Resolve 候选池 + 游标 RR + 失败冷却自愈）。
// 数据面确定性走 hub 中继（startSClientMeshConnect 硬编码 --webrtc=false），
// 规避既有 WebRTC mesh flaky（TestE2E_MeshNode_ServiceAccess 阶段 3 复盘记录）。
package sproxy_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// startMarkedEcho 启动一个带节点标识的 echo 服务：accept 后立即写 marker+"\n"
// （节点标识），使 mesh connect 连接后能根据读回内容判断实际到达哪个副本。
// 后续 io.Copy(cn, cn) 保持既有 echo 语义（回显后续字节）。
func startMarkedEcho(t *testing.T, marker string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听标记 echo 服务: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = cn.Write([]byte(marker + "\n"))
				_, _ = io.Copy(cn, cn) // 回显后续字节
			}(c)
		}
	}()
	return ln.Addr().String()
}

// meshEchoRoundTrip 建立一条到 mesh connect 本地转发端口的连接，读回节点标识首行。
// mesh connect 的本地端口转发对每个入站连接独立 Resolve + dial；目标副本的 echo
// 服务 accept 后写自身标识，故读回的首行即实际命中的节点。readTimeout 覆盖
// 「relay 数据面建立」等待（对端拨号完成后才会写 marker）。
func meshEchoRoundTrip(t *testing.T, listenAddr string, readTimeout time.Duration) (string, error) {
	t.Helper()
	conn, err := net.Dial("tcp", listenAddr)
	if err != nil {
		return "", fmt.Errorf("连接本地转发端口 %s: %w", listenAddr, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("读节点标识失败: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// identifyMeshTarget 带重试的节点标识往返：数据面建立初期（或单点故障切换后）单次
// 建连可能失败（relay 未就绪 / 拨号到死亡节点），重试至多 attempts 次。返回最终
// 成功的 marker 或最后一次错误。
func identifyMeshTarget(t *testing.T, listenAddr string, attempts int, readTimeout, retryInterval time.Duration) (string, error) {
	t.Helper()
	var lastErr error
	for i := range attempts {
		marker, err := meshEchoRoundTrip(t, listenAddr, readTimeout)
		if err == nil {
			return marker, nil
		}
		lastErr = err
		if i < attempts-1 {
			time.Sleep(retryInterval)
		}
	}
	return "", lastErr
}

// warmUpMeshTargets 暖机：持续建连直到 want 中每个副本标识都至少读到一次，证明
// 数据面端到端就绪且全部副本可达（RR 分布测量的前提）。
func warmUpMeshTargets(t *testing.T, listenAddr string, want []string, deadline time.Duration) {
	t.Helper()
	seen := map[string]bool{}
	dl := time.Now().Add(deadline)
	var lastErr error
	for len(seen) < len(want) {
		marker, err := meshEchoRoundTrip(t, listenAddr, 3*time.Second)
		if err != nil {
			lastErr = err
		} else {
			matched := false
			for _, w := range want {
				if marker == w {
					matched = true
					seen[marker] = true
				}
			}
			if !matched {
				t.Fatalf("暖机读到未知节点标识 %q", marker)
			}
		}
		if time.Now().After(dl) {
			t.Fatalf("暖机未在 %s 内覆盖全部副本（已见 %v，最后错误: %v）", deadline, seen, lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitNodeGone 轮询 /api/hub/nodes 直到 nodeID 从 hub 注销（kill 后 WS 断开，
// hub 路由表 RemoveIfOwned 移除节点）。超时失败。
func waitNodeGone(t *testing.T, hubURL, nodeID, ak, sk string, deadline time.Duration) {
	t.Helper()
	dl := time.Now().Add(deadline)
	for hubNodeRegistered(hubURL, nodeID, ak, sk) {
		if time.Now().After(dl) {
			t.Fatalf("节点 %s 未在 %s 内从 hub 注销", nodeID, deadline)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestE2E_MeshRR_RoundRobin 验证 mesh 多副本 round-robin：
// hub + 两个 mesh node 宣告同名 echo 服务（不同 node-id，echo 返回不同标识）+
// mesh connect（--webrtc=false 确定性走中继回落）连续建连 N 次，统计各副本命中
// 次数 ≈ 各半（RR 游标轮询；瞬时 relay 抖动触发 cooldown 导致分布偏斜时重试一轮，
// 真实 RR bug 会在每轮都呈现同一副本 0 命中）。
func TestE2E_MeshRR_RoundRobin(t *testing.T) {
	hubURL, ak, sk, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	echoA := startMarkedEcho(t, "node-a")
	echoB := startMarkedEcho(t, "node-b")

	// 两个 mesh node 宣告同名 echo 服务（node-id 不同，echo 返回不同标识）。
	cleanupA := startSClientMeshNode(t, hubURL, "e2e-rr-a", "echo:"+echoA, ak, sk, "--discover=false")
	defer cleanupA()
	cleanupB := startSClientMeshNode(t, hubURL, "e2e-rr-b", "echo:"+echoB, ak, sk, "--discover=false")
	defer cleanupB()

	// mesh connect 端口转发（--webrtc=false 走中继回落，确定性验证）。
	listenAddr, meshCleanup := startSClientMeshConnect(t, hubURL, "echo", ak, sk)
	defer meshCleanup()

	// 暖机：两副本标识都至少读到一次（数据面端到端就绪 + 候选池两副本均可达）。
	warmUpMeshTargets(t, listenAddr, []string{"node-a", "node-b"}, 20*time.Second)

	// 连续建连 N 次统计 RR 分布。
	const n = 10
	var counts map[string]int
	distOK := false
	for round := 1; round <= 3 && !distOK; round++ {
		counts = map[string]int{}
		for i := range n {
			marker, err := identifyMeshTarget(t, listenAddr, 3, 5*time.Second, 500*time.Millisecond)
			if err != nil {
				t.Fatalf("第 %d 次建连失败: %v", i+1, err)
			}
			counts[marker]++
		}
		// 容忍：健康两副本下 RR 应各半（±2）；瞬时 relay 失败触发 cooldown
		// （MeshFailCooldown=9s）会把后续采样导向存活副本导致偏斜——重试吸收。
		if counts["node-a"] >= n/2-2 && counts["node-b"] >= n/2-2 {
			distOK = true
			break
		}
		t.Logf("第 %d 轮 RR 分布偏斜（node-a=%d node-b=%d），等 cooldown 后重测", round, counts["node-a"], counts["node-b"])
		time.Sleep(client.MeshFailCooldown + time.Second)
	}
	if !distOK {
		t.Fatalf("RR 分布偏斜: node-a=%d node-b=%d（N=%d）", counts["node-a"], counts["node-b"], n)
	}
	t.Logf("RR 分布: node-a=%d node-b=%d（N=%d）", counts["node-a"], counts["node-b"], n)
}

// TestE2E_MeshRR_Failover 验证 mesh 单点故障回退：
// 两个副本都在时连接成功；kill 一个 mesh node 进程 → hub 注销该节点 → 后续
// mesh connect 自动跳过死亡节点（走存活副本）。每笔成功连接都应落到存活副本。
func TestE2E_MeshRR_Failover(t *testing.T) {
	hubURL, ak, sk, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	echoA := startMarkedEcho(t, "node-a")
	echoB := startMarkedEcho(t, "node-b")

	cleanupA := startSClientMeshNode(t, hubURL, "e2e-fo-a", "echo:"+echoA, ak, sk, "--discover=false")
	cleanupB := startSClientMeshNode(t, hubURL, "e2e-fo-b", "echo:"+echoB, ak, sk, "--discover=false")
	// cleanupA 在阶段 2 显式 Kill（newKillWaitCleanup sync.Once 保护，重复调用是
	// no-op），这里仍 defer 以防阶段 2 前失败导致进程泄漏。
	defer cleanupA()
	defer cleanupB()

	listenAddr, meshCleanup := startSClientMeshConnect(t, hubURL, "echo", ak, sk)
	defer meshCleanup()

	// 阶段 1：kill 前两副本均可达（数据面健康基线）。
	warmUpMeshTargets(t, listenAddr, []string{"node-a", "node-b"}, 20*time.Second)

	// 阶段 2：kill 节点 A 的进程（sync.Once 保护的 Kill+Wait）。
	cleanupA()

	// 阶段 3：等 hub 注销节点 A（kill 后 WS 断开，路由表 RemoveIfOwned 移除节点）。
	waitNodeGone(t, hubURL, "e2e-fo-a", ak, sk, 10*time.Second)

	// 阶段 4：后续建连自动跳过死亡节点，全部落到存活节点 B。
	const m = 5
	for i := range m {
		marker, err := identifyMeshTarget(t, listenAddr, 4, 5*time.Second, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("kill 后第 %d 次建连失败: %v", i+1, err)
		}
		if marker != "node-b" {
			t.Fatalf("kill 后建连应全部落到存活副本 node-b，实际 %s", marker)
		}
	}
	t.Logf("failover 通过：kill 后 %d 次建连全部命中存活副本 node-b", m)
}
