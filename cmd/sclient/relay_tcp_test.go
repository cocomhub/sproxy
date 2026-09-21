// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin" // 注册内置 tcp 传输
)

// TestRelayStart_TCPTransport_NoWS_RelayDial 是子任务 5 DoD 的 CLI 级验证：
// 经真实 runRelayOnce（--transport tcp）注册叶子到**无 WS** 的 hub，再经
// relay dial 语义（/api/relay/stream）拨号 echo 服务成功。
//
// 拓扑：echo server ⇄ leaf(relay.Serve, 出口模式, 服务宣告 echo) ⇄ hub(裸 TCP)
// ⇄ RelayStreamHandler ⇄ caller(原始 TCP CONNECT 风格)。
func TestRelayStart_TCPTransport_NoWS_RelayDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// 1. echo server
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

	// 2. hub：仅裸 TCP（无 WS），SproxySig 准入
	const (
		ak = "ak-relay-tcp-000000000000000000000"
		sk = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	rt := hub.NewMeshRouteTable()
	hs := hub.NewHubServer(rt, hub.NewAuthenticator(accesskey.NewRingFromKeyPairs([]accesskey.KeyPair{{Key: ak, Secret: sk}})), testutil.DiscardLogger())
	ln, err := hs.ListenTCP(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = hs.AcceptTCP(ctx, ln) }()
	hubAddr := ln.(interface{ Addr() net.Addr }).Addr().String()

	// 3. leaf：真实 runRelayOnce，transport=tcp，声明 echo 服务 + 出口模式
	leafErr := make(chan error, 1)
	go func() {
		leafErr <- runRelayOnce(ctx, "tcp", "leaf-cli-tcp", hubAddr, "http://127.0.0.1:1",
			ak, sk, "", false, "", true, []string{"echo:" + echoAddr}, nil, hub.DefaultVirtualSubnet, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	// 4. 等待叶子注册进路由表
	testutil.WaitFor(t, 30*time.Second, func() bool { return rt.Has("leaf-cli-tcp") },
		"leaf-cli-tcp not registered in time")

	// 5. RelayStreamHandler + httptest（等价 relay dial 的 HTTP 面）
	h := server.NewRelayStreamHandler(rt, testutil.DiscardLogger())
	tsrv := httptest.NewServer(h)
	defer tsrv.Close()

	// 6. 原始 TCP CONNECT 风格拨号（等价 FileClient.RelayStream）
	srvAddr := strings.TrimPrefix(tsrv.URL, "http://")
	conn, err := net.Dial("tcp", srvAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	body, _ := json.Marshal(server.RelayStreamRequest{Target: "leaf-cli-tcp", Type: "tcp", Addr: echoAddr})
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

	// 7. 双向字节流：写 payload 读回 echo
	payload := []byte("cli-tcp-relay-dial-ok")
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
		t.Fatal("runRelayOnce 未退出")
	}
}

// TestRelayStartCmd_TransportFlag 验证 --transport flag 默认值与取值校验。
func TestRelayStartCmd_TransportFlag(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelayStart(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	f := cmd.Flags().Lookup("transport")
	if f == nil {
		t.Fatal("missing --transport flag")
	}
	if f.DefValue != "ws" {
		t.Fatalf("expected default transport 'ws', got %q", f.DefValue)
	}
	// 非法取值应报错
	cmd2 := NewCmdRelayStart(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	_ = cmd2.Flags().Set("transport", "udp")
	if err := cmd2.RunE(cmd2, nil); err == nil {
		t.Fatal("expected error for invalid transport value")
	}
}

// TestRelayStartCmd_TransportFlag_Quic 验证 --transport quic 是合法取值
// （QUIC 传输装配后合法值扩展为 ws/tcp/quic）。
func TestRelayStartCmd_TransportFlag_Quic(t *testing.T) {
	t.Parallel()
	// 注入已取消 ctx：runRelayOnce 的 access_key_secret 空错误会触发重连循环，
	// 取消 ctx 使其在首次返回时被 runRelayWithRetry 的 ctx.Err() 门控拦截立即返回
	// （验证 quic 通过白名单，而非卡在重连）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := NewCmdRelayStart(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	cmd.SetContext(ctx)
	_ = cmd.Flags().Set("transport", "quic")
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected error: quic transport requires access_key_secret (fail-closed)")
	}
	if strings.Contains(err.Error(), "未知传输层") {
		t.Fatalf("expected quic to be accepted by transport whitelist, got unknown transport: %v", err)
	}
	if !strings.Contains(err.Error(), "access_key_secret") {
		t.Fatalf("expected access_key_secret error for quic transport, got: %v", err)
	}
	// 非法取值仍报错
	cmd2 := NewCmdRelayStart(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	cmd2.SetContext(ctx)
	_ = cmd2.Flags().Set("transport", "sctp")
	if err := cmd2.RunE(cmd2, nil); err == nil {
		t.Fatal("expected error for invalid transport value sctp")
	}
}
