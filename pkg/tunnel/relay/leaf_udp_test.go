// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestServe_UDPMapDialPolicyRejected（F3 负向，SSRF）：UDP 映射帧目标未通过拨号策略
// 时，出口不转发、流被关闭（防 --dial-allow 节点被当任意内网 UDP 转发代理）。
func TestServe_UDPMapDialPolicyRejected(t *testing.T) {
	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// 策略一律拒绝（含 loopback/私网）。
	policy := func(addr string) (string, bool) { return "", false }
	go func() {
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testLogger(), ServeOptions{DialPolicy: policy})
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	udpMeta, _ := json.Marshal(hub.UDPRequest{UDP: "127.0.0.1:9"})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(udpMeta)))
	_, _ = clientStream.Write(lenBuf)
	_, _ = clientStream.Write(udpMeta)
	_ = clientStream.CloseWrite()

	expectStreamClosed(t, clientStream, "策略拒绝的 UDP 映射流", 5*time.Second)
}

// TestServe_UDPMapBidirectional（relay 级）：UDP 映射流经 relay 双向转发——数据报经
// clientMux 到出口，出口转发到 UDP echo，响应经 relay 回传 clientMux。
func TestServe_UDPMapBidirectional(t *testing.T) {
	// UDP echo 服务（出口转发目标）。
	udpEcho, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udpEcho.Close()
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, rerr := udpEcho.ReadFromUDP(buf)
			if rerr != nil {
				return
			}
			_, _ = udpEcho.WriteToUDP(buf[:n], addr)
		}
	}()
	echoAddr := udpEcho.LocalAddr().String()

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// 策略精确放行 echo 地址（对齐 NewServiceDialPolicy 的宣告服务地址）。
	policy := func(addr string) (string, bool) {
		if addr == echoAddr {
			return echoAddr, true
		}
		return "", false
	}
	go func() {
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testLogger(), ServeOptions{DialPolicy: policy})
	}()

	// 打开 UDP 映射控制流。
	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()
	udpMeta, _ := json.Marshal(hub.UDPRequest{UDP: echoAddr})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(udpMeta)))
	_, _ = clientStream.Write(lenBuf)
	_, _ = clientStream.Write(udpMeta)

	// 客户端 datagram handler 收响应。
	recv := make(chan string, 4)
	clientMux.SetDatagramHandler(func(flowID uint32, data []byte) {
		recv <- string(data)
	})

	// 发数据报 → echo 响应回传（重试容忍出口 setup 时序：handler 未设置时数据报被丢）。
	payload := []byte("udp-relay-hello")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := clientMux.SendDatagram(0, payload); err != nil {
			t.Fatalf("SendDatagram: %v", err)
		}
		select {
		case got := <-recv:
			if got != string(payload) {
				t.Fatalf("echo = %q, want %q", got, payload)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("未收到 echo 响应")
		}
	}
}

// TestServe_UDPMapMultiSourceResponse（H6）：出口用 ListenUDP+WriteToUDP（非连接
// socket），目标从不同源端口回包也能收到响应（TFTP/游戏协议/多 A 记录场景）。
func TestServe_UDPMapMultiSourceResponse(t *testing.T) {
	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	responder, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()
	targetAddr := target.LocalAddr().String()
	// 目标收到 relay 数据报 → 从 responder（不同源端口）回包。
	go func() {
		buf := make([]byte, 65535)
		for {
			n, src, rerr := target.ReadFromUDP(buf)
			if rerr != nil {
				return
			}
			_, _ = responder.WriteToUDP(buf[:n], src)
		}
	}()

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	policy := func(addr string) (string, bool) {
		if addr == targetAddr {
			return targetAddr, true
		}
		return "", false
	}
	go func() {
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testLogger(), ServeOptions{DialPolicy: policy})
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()
	udpMeta, _ := json.Marshal(hub.UDPRequest{UDP: targetAddr})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(udpMeta)))
	_, _ = clientStream.Write(lenBuf)
	_, _ = clientStream.Write(udpMeta)

	recv := make(chan string, 4)
	clientMux.SetDatagramHandler(func(flowID uint32, data []byte) {
		recv <- string(data)
	})

	// 重试容忍出口 setup 时序（handler 未设置时数据报被丢）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := clientMux.SendDatagram(0, []byte("multi-source")); err != nil {
			t.Fatalf("SendDatagram: %v", err)
		}
		select {
		case got := <-recv:
			if got != "multi-source" {
				t.Fatalf("响应 = %q, want multi-source", got)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("未收到多源响应")
		}
	}
}

// TestIsUDPMomentaryErr（H7）：瞬时 UDP 错误识别（目标 ICMP 拒绝/重置/超长/路由不可达）。
func TestIsUDPMomentaryErr(t *testing.T) {
	for _, err := range []error{
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EMSGSIZE,
		syscall.EHOSTUNREACH, syscall.ENETUNREACH,
		&net.OpError{Op: "read", Err: syscall.ECONNREFUSED},
	} {
		if !isUDPMomentaryErr(err) {
			t.Errorf("isUDPMomentaryErr(%v) 应为 true", err)
		}
	}
	if isUDPMomentaryErr(io.EOF) {
		t.Error("EOF 不应判为瞬时 UDP 错误")
	}
}

// TestServe_FrameDisambiguation（H10）：帧同时携带 udp 与 method 字段 → 按 HTTP 处理
// （不劫持为 UDP 映射）。观察：若误判为 UDP 且 policy 放行 udp 目标，流会保持打开
// （handleUDPMap 阻塞）；按 HTTP 处理（localAddr 不可达）流会关闭。
func TestServe_FrameDisambiguation(t *testing.T) {
	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// policy 放行一切（若误判为 UDP 映射，流保持打开）。
	policy := func(addr string) (string, bool) { return addr, true }
	go func() {
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testLogger(), ServeOptions{DialPolicy: policy})
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()
	meta := []byte(`{"method":"GET","url":"/x","udp":"8.8.8.8:53"}`)
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(meta)))
	_, _ = clientStream.Write(lenBuf)
	_, _ = clientStream.Write(meta)
	_ = clientStream.CloseWrite()

	// 流应按 HTTP 处理（localAddr 不可达 → 关闭），而非保持打开（UDP 映射）。
	expectStreamClosed(t, clientStream, "双字段帧应按 HTTP 处理（流关闭）", 5*time.Second)
}
