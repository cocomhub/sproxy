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
