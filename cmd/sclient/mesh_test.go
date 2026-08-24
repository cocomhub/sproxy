// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	"github.com/spf13/cobra"
)

func TestNewCmdMesh_Subcommands(t *testing.T) {
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, nil)
	if cmd.Use != "mesh" {
		t.Fatalf("expected Use 'mesh', got %q", cmd.Use)
	}
	subs := map[string]bool{"connect": false, "status": false}
	for _, c := range cmd.Commands() {
		if _, ok := subs[c.Name()]; ok {
			subs[c.Name()] = true
		}
	}
	for name, found := range subs {
		if !found {
			t.Errorf("missing subcommand: %s", name)
		}
	}
}

func TestNewCmdMesh_NodeSubcommand(t *testing.T) {
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, nil)
	var node *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Name() == "node" {
			node = c
			break
		}
	}
	if node == nil {
		t.Fatal("mesh 缺少 node 子命令")
	}
	if node.Use != "node" {
		t.Fatalf("unexpected node Use: %q", node.Use)
	}
	for _, name := range []string{"hub", "node-id", "service", "dial-allow", "dial-allow-cidr", "local", "webrtc", "discover", "discover-interval", "gateway-addr", "stun"} {
		if f := node.Flags().Lookup(name); f == nil {
			t.Errorf("node 缺少 flag: %s", name)
		}
	}
}

func TestNewCmdMeshConnect_ArgsAndFlags(t *testing.T) {
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, nil)
	connect := cmd.Commands()[0]
	if connect.Use != "connect <service> [-l :port]" {
		t.Fatalf("unexpected connect Use: %q", connect.Use)
	}
	for _, name := range []string{"listen", "webrtc", "hub", "node-id", "gateway"} {
		if f := connect.Flags().Lookup(name); f == nil {
			t.Errorf("connect 缺少 flag: %s", name)
		}
	}
}

// servicesHandler 构造一个可切换响应的 /api/hub/services mock。
func servicesHandler(hits *atomic.Int32, get func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(get()))
	}
}

func fixedClock() func() time.Time {
	t := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// lockedBuffer 是并发安全的字节缓冲，供测试观察异步 ErrOut 输出。
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestMeshForwardListen_RefreshesTarget 集成验证 meshForwardListen 每连接用最新 target：
//
//	场景 1：服务在列表 → dial 收到正确 target；
//	场景 2：服务下线 + invalidate → 连接快速失败，ErrOut 报「不可用」，不再卡死。
func TestMeshForwardListen_RefreshesTarget(t *testing.T) {
	var mu sync.Mutex
	svcList := `[{"name":"svc","node":"node-a","addr":"127.0.0.1:10022"}]`
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		mu.Lock()
		defer mu.Unlock()
		return svcList
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	r := client.NewMeshTargetRefresher(svc, "svc")
	r.SetTTL(time.Hour)
	r.SetClock(fixedClock())

	// 注入 dial：记录收到的 target，返回错误触发 invalidate 路径（避免 pump 阻塞）。
	targets := make(chan *client.MeshService, 4)
	dial := func(_ context.Context, _ *client.FileClient, _ *hub.HubSignaler, target *client.MeshService, _ string) (*mesh.Result, error) {
		targets <- target
		return nil, fmt.Errorf("injected dial error")
	}

	errBuf := &lockedBuffer{}
	ios := cli.IOStreams{Out: io.Discard, ErrOut: errBuf}

	initial, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// 预留一个空闲端口（meshForwardListen 内部会再次绑定同一地址）
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenAddr := reserve.Addr().String()
	_ = reserve.Close()

	// I67：可取消 ctx + t.Cleanup(cancel)，测试结束触发 meshForwardListen 的
	// ctx 优雅停止（Accept 返回、listener 关闭、goroutine 退出），修 listener 泄漏。
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := &cobra.Command{}
	cmd.SetContext(ctx) // 未执行 Execute 的裸命令 Context() 为 nil，需显式设置
	go func() {
		// meshForwardListen 阻塞在 Accept，直到测试结束端口关闭
		_ = meshForwardListen(cmd, svc, nil, dial, r, initial, "local-node", listenAddr, ios)
	}()

	// 轮询拨号直到 meshForwardListen 的 listener 就绪（goroutine 启动有延迟）
	dialForward := func() (net.Conn, error) {
		var c net.Conn
		var derr error
		deadline := time.Now().Add(3 * time.Second)
		for {
			c, derr = net.Dial("tcp", listenAddr)
			if derr == nil {
				return c, nil
			}
			if time.Now().After(deadline) {
				return nil, derr
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 场景 1：服务在列表 → dial 收到 node-a
	c1, err := dialForward()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case target := <-targets:
		if target.Node != "node-a" || target.Addr != "127.0.0.1:10022" {
			t.Fatalf("dial target = %+v, want node-a/127.0.0.1:10022", target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dial not called for scenario 1")
	}
	_ = c1.Close()

	// 场景 2：服务下线 + invalidate → 连接被服务端快速关闭，ErrOut 报「不可用」
	mu.Lock()
	svcList = `[]`
	mu.Unlock()
	r.Invalidate("node-a")

	c2, err := dialForward()
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, rerr := c2.Read(make([]byte, 1)); rerr == nil {
		t.Fatal("expected connection closed by server (service offline)")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(errBuf.String(), "不可用") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(errBuf.String(), "不可用") {
		t.Fatalf("expected '不可用' error output, got: %q", errBuf.String())
	}
}

// mockGateway 构造一个按固定应答 JSON 回应的 mock 网关（测试 meshGatewayDial 选路）。
// resp 是应答帧 JSON 体（如 {"ok":true} 或 {"ok":false,"error":"no_peer_link"}）。
func mockGateway(t *testing.T, resp string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
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
				lenBuf := make([]byte, 4)
				if _, rerr := io.ReadFull(cn, lenBuf); rerr != nil {
					return
				}
				payload := make([]byte, binary.BigEndian.Uint32(lenBuf))
				if _, rerr := io.ReadFull(cn, payload); rerr != nil {
					return
				}
				respLen := make([]byte, 4)
				binary.BigEndian.PutUint32(respLen, uint32(len(resp)))
				_, _ = cn.Write(respLen)
				_, _ = cn.Write([]byte(resp))
			}(c)
		}
	}()
	return ln.Addr().String()
}

// TestMeshGatewayDial_UsesGatewayWhenOk：--gateway 且网关返回 ok → 复用已建链路
// （Kind=peer-link），不经常规拨号。
func TestMeshGatewayDial_UsesGatewayWhenOk(t *testing.T) {
	gatewayAddr := mockGateway(t, `{"ok":true}`)
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	dial := meshGatewayDial(gatewayAddr, "", ios)
	target := &client.MeshService{Name: "svc", Node: "node-b", Addr: "127.0.0.1:22"}
	res, err := dial(context.Background(), nil, nil, target, "local-node")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if res.Kind != mesh.KindPeerLink {
		t.Fatalf("kind = %q, want %q", res.Kind, mesh.KindPeerLink)
	}
	if res.Conn == nil {
		t.Fatal("conn 不应为 nil")
	}
	_ = res.Conn.Close()
}

// TestMeshGatewayDial_FallsBackWhenNoPeerLink：--gateway 但本地节点无已建链路
// （网关回 no_peer_link）→ 回落常规拨号（webrtc 跳过 → 中继失败报 RelayStream），
// 且 no_peer_link 属预期回落不写 ErrOut。
func TestMeshGatewayDial_FallsBackWhenNoPeerLink(t *testing.T) {
	gatewayAddr := mockGateway(t, `{"ok":false,"error":"no_peer_link","message":"mesh: no link"}`)
	errBuf := &lockedBuffer{}
	ios := cli.IOStreams{Out: io.Discard, ErrOut: errBuf}
	dial := meshGatewayDial(gatewayAddr, "", ios)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()
	svc := client.NewFileClient(ts.URL)
	target := &client.MeshService{Name: "svc", Node: "node-b", Addr: "127.0.0.1:22"}
	_, err := dial(context.Background(), svc, nil, target, "local-node")
	if err == nil || !strings.Contains(err.Error(), "RelayStream") {
		t.Fatalf("期望回落中继失败（RelayStream 错误）, got %v", err)
	}
	if strings.Contains(errBuf.String(), "本地网关路由失败") {
		t.Fatalf("no_peer_link 不应写 ErrOut, got %q", errBuf.String())
	}
}

// TestMeshStatus_GatewayTopology：mesh status --gateway 查询本地 mesh node 网关
// 拓扑（node-id + 服务宣告 + 已建直连链路/链路类型）。
func TestMeshStatus_GatewayTopology(t *testing.T) {
	resp := `{"node_id":"node-ap","services":[{"name":"echo","addr":"127.0.0.1:22"}],"peers":[{"peer":"node-svc","link":"webrtc-direct","since":"2026-08-24T00:00:00Z"}]}`
	gatewayAddr := mockGateway(t, resp)
	var out bytes.Buffer
	// 真实 FileClient（mock 工厂传 nil client 会让 svc.AccessKeySecret() nil 指针崩溃）。
	cmd := newCmdMeshStatus(clientfactory.NewMock(client.NewFileClient("http://127.0.0.1:1"), nil), cli.IOStreams{Out: &out, ErrOut: io.Discard})
	cmd.SetContext(context.Background()) // 未 Execute 的裸命令 Context() 为 nil
	if err := cmd.Flags().Set("gateway", gatewayAddr); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, []string{}); err != nil {
		t.Fatalf("mesh status: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"mesh 节点: node-ap",
		"服务宣告 (1):",
		"echo",
		"127.0.0.1:22",
		"已建直连链路 (1):",
		"node-svc",
		"webrtc-direct",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("mesh status 输出缺少 %q: %s", want, got)
		}
	}
}
