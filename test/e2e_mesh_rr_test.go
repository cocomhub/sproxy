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
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// startMarkedEcho 启动一个带节点标识的 echo 服务：accept 后立即写 marker+"\n"
// （节点标识），使 mesh connect 连接后能根据读回内容判断实际到达哪个副本。
// 后续 io.Copy(cn, cn) 保持既有 echo 语义（回显后续字节）。
// 返回监听地址与关闭函数（审查 I-2：Failover 需显式关闭服务 listener 模拟
// "节点仍注册但服务不可达"，触发失败跳过 + 冷却自愈机制）。
func startMarkedEcho(t *testing.T, marker string) (string, func()) {
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
	var closeOnce sync.Once
	return ln.Addr().String(), func() {
		closeOnce.Do(func() { _ = ln.Close() })
	}
}

// checkTestDeadline 检查测试整体超时预算（审查 I-1）：e2e 用真实子进程，无 per-test
// 兜底时最坏路径（暖机超时 + 多轮 cooldown 重试 + 每次读满超时）可能超整个 test/ 包
// 的 -timeout。基于 t.Deadline()（即 -timeout 剩余）在循环内提前判断，留 30s 余量。
// 调用方在耗时循环的每轮开头调用；预算不足即 Fatalf（而非无界等待到包超时）。
func checkTestDeadline(t *testing.T) {
	t.Helper()
	if dl, ok := t.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining < 30*time.Second {
			t.Fatalf("测试超时预算不足（剩余 %s < 30s），提前终止避免撞 test/ 包 -timeout", remaining.Round(time.Second))
		}
	}
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
// 成功的 marker、是否曾内部重试（retried=true 表示首笔失败后重试成功——对 RR 测量
// 而言是"脏采样"，触发过 cooldown 会使分布偏斜，调用方应判脏轮重测）、或最后一次
// 错误。
func identifyMeshTarget(t *testing.T, listenAddr string, attempts int, readTimeout, retryInterval time.Duration) (string, bool, error) {
	t.Helper()
	var lastErr error
	for i := range attempts {
		marker, err := meshEchoRoundTrip(t, listenAddr, readTimeout)
		if err == nil {
			return marker, i > 0, nil // i>0 = 首笔失败后重试成功
		}
		lastErr = err
		if i < attempts-1 {
			time.Sleep(retryInterval)
		}
	}
	return "", false, lastErr
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

// TestE2E_MeshRR_RoundRobin 验证 mesh 多副本 round-robin：
// hub + 两个 mesh node 宣告同名 echo 服务（不同 node-id，echo 返回不同标识）+
// mesh connect（--webrtc=false 确定性走中继回落）连续建连 N 次，统计各副本命中
// 次数 ≈ 各半（RR 游标轮询；瞬时 relay 抖动触发 cooldown 导致分布偏斜时重试一轮，
// 真实 RR bug 会在每轮都呈现同一副本 0 命中）。
func TestE2E_MeshRR_RoundRobin(t *testing.T) {
	hubURL, ak, sk, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	echoA, _ := startMarkedEcho(t, "node-a")
	echoB, _ := startMarkedEcho(t, "node-b")

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
		checkTestDeadline(t) // 审查 I-1：超时预算兜底
		counts = map[string]int{}
		dirty := false // 本轮任一采样曾内部重试（触发过 cooldown）→ 分布偏斜，判脏
		for i := range n {
			checkTestDeadline(t)
			marker, retried, err := identifyMeshTarget(t, listenAddr, 3, 5*time.Second, 500*time.Millisecond)
			if err != nil {
				t.Fatalf("第 %d 次建连失败: %v", i+1, err)
			}
			if retried {
				// 审查 M-2：首笔失败→Invalidate→9s cooldown 会把本轮剩余采样导向存活
				// 副本致偏斜，这不是 RR bug。标记脏轮，本轮分布不纳入统计。
				dirty = true
			}
			counts[marker]++
		}
		if dirty {
			t.Logf("第 %d 轮含失败重试采样（cooldown 干扰），判脏重测", round)
			time.Sleep(client.MeshFailCooldown + time.Second)
			continue
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

// TestE2E_MeshRR_Failover 验证 mesh 单点故障回退（审查 I-2 升级为机制级验证）：
// 两个副本都在时连接成功；**关闭节点 A 的 echo 服务 listener（mesh node 进程保持
// 在线，注册与宣告不变）** → 节点 A 仍在 /api/hub/services 候选池中，但出口拨号
// 目标端口已关（hub 中继返回 502）→ mesh connect 拨号失败 → Invalidate(node-a) →
// 冷却期内 pickNextLocked 跳过它 → 后续建连全部落到存活副本 node-b。
// 这验证的是 PR-1 核心机制"失败跳过 + 冷却自愈"（节点仍注册时跳过，而非"节点下线
// 候选池刷新后只剩存活副本"的平凡场景——后者 kill 整个 node 即触发，本测试避免）。
func TestE2E_MeshRR_Failover(t *testing.T) {
	hubURL, ak, sk, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	echoA, closeEchoA := startMarkedEcho(t, "node-a")
	echoB, _ := startMarkedEcho(t, "node-b")

	cleanupA := startSClientMeshNode(t, hubURL, "e2e-fo-a", "echo:"+echoA, ak, sk, "--discover=false")
	cleanupB := startSClientMeshNode(t, hubURL, "e2e-fo-b", "echo:"+echoB, ak, sk, "--discover=false")
	defer cleanupA()
	defer cleanupB()

	listenAddr, meshCleanup := startSClientMeshConnect(t, hubURL, "echo", ak, sk)
	defer meshCleanup()

	// 阶段 1：故障前两副本均可达（数据面健康基线）。
	warmUpMeshTargets(t, listenAddr, []string{"node-a", "node-b"}, 20*time.Second)

	// 阶段 2：关闭节点 A 的 echo listener（节点进程与 hub 注册保持在线；候选池
	// 仍含 A，但出口拨号目标端口不可达 → 拨号失败触发 Invalidate 冷却）。
	closeEchoA()

	// 阶段 3：连续建连验证失败跳过——节点 A 仍注册但服务不可达，冷却期内
	// pickNextLocked 跳过它，全部落到存活副本 node-b。注意：不能依赖"候选池刷新
	// 后只剩 B"（节点没下线，候选池仍含 A）——这正是机制级验证的要点。
	const m = 5
	for i := range m {
		checkTestDeadline(t) // 审查 I-1：超时预算兜底
		marker, _, err := identifyMeshTarget(t, listenAddr, 4, 5*time.Second, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("服务故障后第 %d 次建连失败: %v", i+1, err)
		}
		if marker != "node-b" {
			t.Fatalf("服务故障后建连应全部落到存活副本 node-b（失败跳过），实际 %s", marker)
		}
	}
	t.Logf("failover 通过：节点 A 服务故障后 %d 次建连全部命中存活副本 node-b（失败跳过+冷却自愈）", m)
}
