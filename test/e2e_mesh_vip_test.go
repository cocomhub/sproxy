// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxy_test

import (
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"
)

// hubNodeVirtualIP 查询 /api/hub/nodes 返回指定 nodeID 的 virtual_ip（空串表示未分配）。
func hubNodeVirtualIP(t *testing.T, baseURL, nodeID, ak, sk string) string {
	t.Helper()
	resp, err := signedHubGET(baseURL, "/api/hub/nodes", ak, sk)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var nodes []struct {
		ID        string `json:"id"`
		VirtualIP string `json:"virtual_ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return ""
	}
	for _, n := range nodes {
		if n.ID == nodeID {
			return n.VirtualIP
		}
	}
	return ""
}

// TestE2E_MeshConnect_VirtualIP 验证虚拟 IP 端到端（设计 AD-6）：
// hub 为 node-svc 分配虚拟 IP；mesh connect <vip>:<echoPort>（--webrtc=false 走 hub
// 中继）拨到 node-svc 出口，出口 DialPolicy 识别 ==selfVIP 且端口 ∈ 宣告白名单 →
// 改写 127.0.0.1:<port> → 本机 echo 服务回显。安全红线：未宣告端口不可达（C-1）。
func TestE2E_MeshConnect_VirtualIP(t *testing.T) {
	hubURL, ak, sk, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	// node-svc 本机 echo 服务（出口拨号改写目标）。
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
	_, echoPort, _ := net.SplitHostPort(echoLn.Addr().String())

	// node-svc 出口节点：宣告 echo 服务（端口自动进入虚拟 IP 白名单），--dial-allow。
	cleanupSvc := startSClientMeshNode(t, hubURL, "node-svc", "echo:127.0.0.1:"+echoPort, ak, sk)
	defer cleanupSvc()

	// 轮询 /api/hub/nodes 拿 node-svc 的 virtual_ip（hub 权威分配）。
	deadline := time.Now().Add(15 * time.Second)
	var vip string
	for time.Now().Before(deadline) {
		if vip = hubNodeVirtualIP(t, hubURL, "node-svc", ak, sk); vip != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if vip == "" {
		t.Fatal("node-svc 未在 hub 获得虚拟 IP")
	}
	t.Logf("node-svc 虚拟 IP: %s", vip)

	// mesh connect <vip>:<echoPort>（--webrtc=false 走 hub 中继，确定性路径）。
	listenAddr, meshCleanup := startSClientMeshConnect(t, hubURL, vip+":"+echoPort, ak, sk)
	defer meshCleanup()

	// echo 往返轮询（首次链路建立 + 出口拨号可达后成功；-race 下宽窗口）。
	payload := []byte("e2e-virtual-ip")
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := net.Dial("tcp", listenAddr)
		if derr == nil {
			_, werr := conn.Write(payload)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			got := make([]byte, len(payload))
			_, rerr := io.ReadFull(conn, got)
			_ = conn.Close()
			if werr == nil && rerr == nil && string(got) == string(payload) {
				t.Logf("虚拟 IP echo 往返成功: %s", payload)
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("mesh connect <vip>:<port> echo 往返超时（虚拟 IP 端到端链路未就绪）")
}

// TestE2E_MeshConnect_VirtualIP_UnannouncedPortRejected（C-1 安全红线 E2E）：
// mesh connect <vip>:<未宣告端口> 必须被出口拒绝。为区分"策略拒绝"与"端口本身不可达"
// （整体审查发现 1 加固）：在出口节点本机另起**真实 hidden 监听器**（echo，不宣告），
// 先验证直接 127.0.0.1:<hiddenPort> 可达（echo 成功），再断言经 <vip>:<hiddenPort> 不可达
// ——若出口策略误放行该端口，改写拨到本机 hidden 监听器会 echo 成功，测试立即失败。
func TestE2E_MeshConnect_VirtualIP_UnannouncedPortRejected(t *testing.T) {
	hubURL, ak, sk, hubCleanup := startHubSPROXY(t)
	defer hubCleanup()

	// 宣告的 echo 监听器（端口 P1，进白名单）。
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go echoAcceptLoop(echoLn)
	_, echoPort, _ := net.SplitHostPort(echoLn.Addr().String())

	// **不宣告**的 hidden echo 监听器（端口 P2）：真实监听，用于验证策略拒绝。
	hiddenLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hiddenLn.Close()
	go echoAcceptLoop(hiddenLn)
	_, hiddenPort, _ := net.SplitHostPort(hiddenLn.Addr().String())

	cleanupSvc := startSClientMeshNode(t, hubURL, "node-svc", "echo:127.0.0.1:"+echoPort, ak, sk)
	defer cleanupSvc()

	deadline := time.Now().Add(15 * time.Second)
	var vip string
	for time.Now().Before(deadline) {
		if vip = hubNodeVirtualIP(t, hubURL, "node-svc", ak, sk); vip != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if vip == "" {
		t.Fatal("node-svc 未在 hub 获得虚拟 IP")
	}

	payload := []byte("c1-reject")
	// 前置验证：hidden 监听器真实可达（直接 127.0.0.1:P2 echo 成功）——
	// 确保后续"经 vip 不可达"是因策略拒绝而非端口不存在。
	hconn, herr := net.Dial("tcp", "127.0.0.1:"+hiddenPort)
	if herr != nil {
		t.Fatalf("hidden 监听器不可达（测试前置失败）: %v", herr)
	}
	_, _ = hconn.Write(payload)
	_ = hconn.SetReadDeadline(time.Now().Add(2 * time.Second))
	hbuf := make([]byte, len(payload))
	_, hreadErr := io.ReadFull(hconn, hbuf)
	_ = hconn.Close()
	if hreadErr != nil || string(hbuf) != string(payload) {
		t.Fatalf("hidden 监听器 echo 失败（测试前置失败）: %v", hreadErr)
	}

	// mesh connect <vip>:<hiddenPort>（未宣告端口）→ 出口必须拒绝。
	listenAddr, meshCleanup := startSClientMeshConnect(t, hubURL, vip+":"+hiddenPort, ak, sk)
	defer meshCleanup()

	deadline = time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := net.Dial("tcp", listenAddr)
		if derr == nil {
			_, _ = conn.Write(payload)
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, len(payload))
			_, rerr := io.ReadFull(conn, buf)
			_ = conn.Close()
			if rerr == nil && string(buf) == string(payload) {
				t.Fatal("未宣告端口经虚拟 IP 不应可访问（C-1 安全红线：出口策略误放行导致 hidden 监听器 echo 成功）")
			}
			// 读失败（EOF/超时）= 出口策略拒绝（hidden 监听器存在仍被拒）→ 通过。
			t.Logf("未宣告端口 %s 经虚拟 IP 被出口拒绝（C-1 闭环，hidden 监听器存在仍拒绝）", hiddenPort)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("mesh connect <vip>:<hiddenPort=%s> 未在窗口内被拒绝（C-1 安全红线未闭环）", hiddenPort)
}

// echoAcceptLoop 循环 accept 并回显（E2E 测试辅助）。
func echoAcceptLoop(ln net.Listener) {
	for {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		go func(cn net.Conn) {
			defer cn.Close()
			_, _ = io.Copy(cn, cn)
		}(c)
	}
}
