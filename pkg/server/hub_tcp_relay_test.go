// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin" // 注册内置 tcp 传输
)

// TestTCPRelay_NoWS_RelayDial 是子任务 5 的 DoD 测试：**无 WS 场景下 relay dial 通**。
//
// 拓扑（全部 in-process，127.0.0.1，无任何 WS 端点）：
//
//	caller(原始 TCP CONNECT 风格 ⇄ RelayStreamHandler)
//	   ⇄ hub 路由表（节点经裸 TCP 注册）
//	   ⇄ leaf mux（xfer/tcp 传输）
//	   ⇄ relay.Serve 出口拨号 ⇄ TCP echo
//
// 覆盖：叶子经裸 TCP 注册进 hub 路由表；caller 经 /api/relay/stream 拨号；
// hub 经 leaf 的 TCP mux 写 dial 帧；leaf 出口连接 echo 并回数据。
func TestTCPRelay_NoWS_RelayDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	// 1. TCP echo server（127.0.0.1 回环）
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

	// 2. hub：仅裸 TCP 传输（无 WS），SproxySig 准入
	const (
		ak = "sk-tcp-do-0000000000000000000000"
		sk = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	rt := hub.NewMeshRouteTable()
	hs := hub.NewHubServer(rt, hub.NewAuthenticator([]hub.AccessKey{{Key: ak, Secret: sk}}), testutil.DiscardLogger())
	ln, err := hs.ListenTCP(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = hs.AcceptTCP(ctx, ln) }()
	hubAddr := ln.(interface{ Addr() net.Addr }).Addr().String()

	// 3. leaf：经裸 TCP 注册 + mux + relay.Serve（出口模式，服务策略精确放行 echo）
	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered")
	}
	leafConn, err := tp.Dial(ctx, hubAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer leafConn.Close()
	ts := time.Now().UnixMilli()
	nonce := hub.NewRegisterNonce()
	proof, perr := hub.ComputeRegisterProof(sk, "leaf-tcp-do", ts, nonce)
	if perr != nil {
		t.Fatal(perr)
	}
	frame := hub.NewRegisterFrame("leaf-tcp-do", ak, proof, ts, nonce, hub.Meta{Addr: "127.0.0.1:0"}, hub.CapabilityPerNodeSecret)
	if serr := leafConn.Send(ctx, frame); serr != nil {
		t.Fatal(serr)
	}
	ack, rerr := leafConn.Receive(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if _, aerr := hub.ParseRegisterAck(string(ack)); aerr != nil {
		t.Fatal(aerr)
	}
	leafMux := mux.New(leafConn, mux.RoleListener)
	defer leafMux.Close()
	leafErr := make(chan error, 1)
	go func() {
		// 服务策略：精确放行宣告的 echo 地址（NewServiceDialPolicy 对 127.0.0.1:port
		// 精确命中，否则默认 DialAllowed 仅公网，回环会被拒绝）。
		leafErr <- relay.Serve(ctx, leafMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testutil.DiscardLogger(),
			relay.ServeOptions{DialPolicy: relay.NewServiceDialPolicy(nil, []string{echoAddr}), DialResultFrames: true})
	}()

	// 4. 等待节点注册进路由表
	deadline := time.Now().Add(3 * time.Second)
	for !rt.Has("leaf-tcp-do") {
		if time.Now().After(deadline) {
			t.Fatal("leaf-tcp-do not registered in time")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 5. caller 侧：RelayStreamHandler 服务 /api/relay/stream
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	tsrv := httptest.NewServer(h)
	defer tsrv.Close()

	// 6. 原始 TCP CONNECT 风格请求（模拟 FileClient.RelayStream）
	srvAddr := strings.TrimPrefix(tsrv.URL, "http://")
	conn, err := net.Dial("tcp", srvAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	body, _ := json.Marshal(RelayStreamRequest{Target: "leaf-tcp-do", Type: "tcp", Addr: echoAddr})
	reqLine := fmt.Sprintf("POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", srvAddr, len(body))
	if _, werr := io.WriteString(conn, reqLine); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := conn.Write(body); werr != nil {
		t.Fatal(werr)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, " 200 ") {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		t.Fatalf("hub 返回 %s%s", strings.TrimSpace(statusLine), rest)
	}
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			t.Fatal(rerr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// 7. 纯双向字节流：写 payload 读回 echo
	payload := []byte("no-ws-tcp-relay-dial-ok")
	if _, werr := conn.Write(payload); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(conn, got); rerr != nil {
		t.Fatalf("读失败: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo 不匹配: got %q want %q", got, payload)
	}

	cancel()
	select {
	case <-leafErr:
	case <-time.After(2 * time.Second):
		t.Fatal("relay.Serve 未退出")
	}
}

// TestTCPRelay_NoWS_ConcurrentRelayDial 验证多个调用方并发经 /api/relay/stream
// 拨号同一 TCP 注册叶子均成功（mux 多路复用 + TCP 传输写互斥下无串流/错位）。
func TestTCPRelay_NoWS_ConcurrentRelayDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// echo server
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
				_, _ = io.Copy(cn, cn)
			}(c)
		}
	}()
	echoAddr := echoLn.Addr().String()

	const (
		ak = "sk-tcp-conc-000000000000000000000"
		sk = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	rt := hub.NewMeshRouteTable()
	hs := hub.NewHubServer(rt, hub.NewAuthenticator([]hub.AccessKey{{Key: ak, Secret: sk}}), testutil.DiscardLogger())
	ln, err := hs.ListenTCP(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = hs.AcceptTCP(ctx, ln) }()
	hubAddr := ln.(interface{ Addr() net.Addr }).Addr().String()

	// leaf via TCP + relay.Serve
	tp := xfer.Get("tcp")
	leafConn, err := tp.Dial(ctx, hubAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer leafConn.Close()
	ts := time.Now().UnixMilli()
	nonce := hub.NewRegisterNonce()
	proof, perr := hub.ComputeRegisterProof(sk, "leaf-conc", ts, nonce)
	if perr != nil {
		t.Fatal(perr)
	}
	frame := hub.NewRegisterFrame("leaf-conc", ak, proof, ts, nonce, hub.Meta{Addr: "127.0.0.1:0"}, hub.CapabilityPerNodeSecret)
	if serr := leafConn.Send(ctx, frame); serr != nil {
		t.Fatal(serr)
	}
	ack, rerr := leafConn.Receive(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if _, aerr := hub.ParseRegisterAck(string(ack)); aerr != nil {
		t.Fatal(aerr)
	}
	leafMux := mux.New(leafConn, mux.RoleListener)
	defer leafMux.Close()
	go func() {
		_ = relay.Serve(ctx, leafMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testutil.DiscardLogger(),
			relay.ServeOptions{DialPolicy: relay.NewServiceDialPolicy(nil, []string{echoAddr}), DialResultFrames: true})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for !rt.Has("leaf-conc") {
		if time.Now().After(deadline) {
			t.Fatal("leaf-conc not registered in time")
		}
		time.Sleep(10 * time.Millisecond)
	}

	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	tsrv := httptest.NewServer(h)
	defer tsrv.Close()
	srvAddr := strings.TrimPrefix(tsrv.URL, "http://")

	// N 个并发调用方，每个拨号后发唯一 payload 并读回 echo
	const n = 6
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			conn, derr := net.Dial("tcp", srvAddr)
			if derr != nil {
				errCh <- derr
				return
			}
			defer conn.Close()
			body, _ := json.Marshal(RelayStreamRequest{Target: "leaf-conc", Type: "tcp", Addr: echoAddr})
			reqLine := fmt.Sprintf("POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", srvAddr, len(body))
			if _, werr := io.WriteString(conn, reqLine); werr != nil {
				errCh <- werr
				return
			}
			if _, werr := conn.Write(body); werr != nil {
				errCh <- werr
				return
			}
			br := bufio.NewReader(conn)
			statusLine, serr := br.ReadString('\n')
			if serr != nil {
				errCh <- serr
				return
			}
			if !strings.Contains(statusLine, " 200 ") {
				rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
				errCh <- fmt.Errorf("hub 返回 %s%s", strings.TrimSpace(statusLine), rest)
				return
			}
			for {
				line, herr := br.ReadString('\n')
				if herr != nil {
					errCh <- herr
					return
				}
				if line == "\r\n" || line == "\n" {
					break
				}
			}
			payload := fmt.Appendf(nil, "conc-dial-%d", i)
			if _, werr := conn.Write(payload); werr != nil {
				errCh <- werr
				return
			}
			got := make([]byte, len(payload))
			if _, rerr := io.ReadFull(conn, got); rerr != nil {
				errCh <- rerr
				return
			}
			if string(got) != string(payload) {
				errCh <- fmt.Errorf("echo 不匹配: got %q want %q", got, payload)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent relay dial failed: %v", err)
	}
}

// TestTCPRelay_NoWS_TargetNotFound 验证无 WS 场景下拨号不存在的目标节点返回 404
// （路由表按 mesh 隔离，目标未注册时对外不可见）。
func TestTCPRelay_NoWS_TargetNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	rt := hub.NewMeshRouteTable()
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	tsrv := httptest.NewServer(h)
	defer tsrv.Close()

	body, _ := json.Marshal(RelayStreamRequest{Target: "ghost", Type: "tcp", Addr: "127.0.0.1:1"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tsrv.URL+"/api/relay/stream", strings.NewReader(string(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
