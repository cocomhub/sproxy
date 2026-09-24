// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"

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
	for _, name := range []string{"hub", "node-id", "service", "dial-allow", "dial-allow-cidr", "local", "webrtc", "discover", "discover-interval", "gateway-addr", "mdns", "signal-addr", "stun", "e2e-identity", "e2e-peer-fp"} {
		if f := node.Flags().Lookup(name); f == nil {
			t.Errorf("node 缺少 flag: %s", name)
		}
	}
}

func TestLoadE2EIdentity(t *testing.T) {
	// 空路径 = nil（纯 ECDH 模式）
	id, err := loadE2EIdentity("")
	if err != nil || id != nil {
		t.Fatalf("空路径应返回 (nil, nil)，got (%v, %v)", id, err)
	}
	// 临时身份文件：生成 → 加载成功
	dir := t.TempDir()
	path := dir + "/identity.json"
	gen, gerr := tunnel.GenerateIdentity()
	if gerr != nil {
		t.Fatal(gerr)
	}
	if serr := tunnel.SaveIdentity(gen, path); serr != nil {
		t.Fatal(serr)
	}
	id2, err2 := loadE2EIdentity(path)
	if err2 != nil {
		t.Fatalf("加载临时身份失败: %v", err2)
	}
	if id2 == nil {
		t.Fatal("加载成功但身份为 nil")
	}
	if id2.Fingerprint() != gen.Fingerprint() {
		t.Fatalf("指纹不一致: got %s want %s", id2.Fingerprint(), gen.Fingerprint())
	}
	// 不存在路径 = 错误（拒绝启动，禁静默降级）
	if _, err3 := loadE2EIdentity(dir + "/missing.json"); err3 == nil {
		t.Fatal("不存在路径应报错（拒绝启动）")
	}
}

func TestNewCmdMeshConnect_ArgsAndFlags(t *testing.T) {
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, nil)
	// 按名查找而非 `Commands()[0]`：cobra 的 Commands() 按名**排序**（EnableCommandSorting 默认开），
	// 新增子命令（如 acl）会让位置假设失效——那是测试脆弱，不是实现回归。
	var connect *cobra.Command
	for _, sub := range cmd.Commands() {
		if strings.HasPrefix(sub.Use, "connect ") {
			connect = sub
			break
		}
	}
	if connect == nil {
		t.Fatal("mesh 缺少 connect 子命令")
	}
	if connect.Use != "connect <service> [-l :port]" {
		t.Fatalf("unexpected connect Use: %q", connect.Use)
	}
	for _, name := range []string{"listen", "webrtc", "hub", "node-id", "gateway", "mdns"} {
		if f := connect.Flags().Lookup(name); f == nil {
			t.Errorf("connect 缺少 flag: %s", name)
		}
	}
}

// TestMeshConnect_MDNSDispatch：`mesh connect <svc> --mdns` 走纯 mDNS 路径
// （不经 hub/svc），错误消息含 "mDNS" 证明路由正确。
//
// 本用例是 cmd/sclient 内唯一真绑组播的用例，故前置 SetMDNSLoopbackOnly(true) 收敛到
// loopback：收敛路径在 Windows 上绑**单播回环地址**（见 mesh.listenMDNSLoopback）而非
// 通配地址，实测不触发防火墙授权弹窗，因此本地 Windows 与 CI 一样实跑，无需跳过门控。
func TestMeshConnect_MDNSDispatch(t *testing.T) {
	oldTimeout := mdnsLookupTimeout
	mdnsLookupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { mdnsLookupTimeout = oldTimeout })
	// mDNS 组播收敛 loopback（绑定地址见上方用例注释）。
	mesh.SetMDNSLoopbackOnly(true)
	t.Cleanup(func() { mesh.SetMDNSLoopbackOnly(false) })

	var out bytes.Buffer
	cmd := newCmdMeshConnect(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &out, ErrOut: io.Discard}, nil)
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("mdns", "true"); err != nil {
		t.Fatal(err)
	}
	// 断言收紧到哨兵错误的**真实价值**：`runMDNSConnect` 的启动失败路径返回的是
	// "mDNS 启动失败: %w"（同样含 "mDNS"），只查字符串会让"收敛路径绑定退化"时本用例
	// 仍然变绿；改用 ErrMDNSServiceNotFound 后，**绑定机制一旦退化（bind/入组/选项设置
	// 失败）即变红**，这就是该收紧的意义。
	//
	// 但**不要**据此认为本用例覆盖了组播投递：LookupService 只轮询本地 peers 缓存、
	// 从不碰 socket，超时即返回该哨兵错误，故它只证明「Start 未报错 + 窗口内无匹配对端」。
	// 「绑得上但收不到包」同样会走到这里。真正验证投递的是 pkg/tunnel/mesh 的真收发用例：
	// TestMDNSDiscovery_TwoNodes（同机双实例互收）、TestMDNSLookupService，以及
	// TestMeshNodeMDNS_* / TestMeshSocks5_Exit / TestMeshUDPMap_Bidirectional——
	// 删除或削弱那些用例前请先读这段。
	err := cmd.RunE(cmd, []string{"nosuchsvc"})
	if err == nil {
		t.Fatal("期望 mDNS 路径报错（未发现服务或 mDNS 不可用）")
	}
	if !errors.Is(err, mesh.ErrMDNSServiceNotFound) {
		t.Fatalf("期望 mDNS 未发现服务（ErrMDNSServiceNotFound, 证明发现链路可用）, got: %v", err)
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

// TestMeshStdioOnce_ShowsKindAndLatency：单次模式输出展示竞速结果可见性（T7）——
// Kind（实际路径）与 Latency（端到端建连耗时）都在连接提示行中，用户可验证
// SmartDial 选了哪条路、多快。
//
// 红灯依据：当前 meshStdioOnce 只输出 Kind（"已连接（%s）"），无 Latency ——
// 断言 "42ms" 必红。
func TestMeshStdioOnce_ShowsKindAndLatency(t *testing.T) {
	t.Parallel()

	svcList := `[{"name":"svc","node":"node-a","addr":"127.0.0.1:10022"}]`
	ts := httptest.NewServer(servicesHandler(&atomic.Int32{}, func() string { return svcList }))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	r := client.NewMeshTargetRefresher(svc, "svc")
	r.SetTTL(time.Hour)
	r.SetClock(fixedClock())

	// 注入 dial：返回真实连接（net.Pipe）承载 pump，Kind/Latency 模拟 SmartDial 竞速结果。
	pcRead, _ := net.Pipe()
	defer pcRead.Close()
	dial := func(_ context.Context, _ *client.FileClient, _ webrtc.Signaler, _ *client.MeshService, _ string) (*mesh.Result, error) {
		return &mesh.Result{Conn: pcRead, Kind: mesh.KindViaDirect, Latency: 42 * time.Millisecond}, nil
	}

	var out bytes.Buffer
	ios := cli.IOStreams{Out: &out, ErrOut: io.Discard, In: strings.NewReader("")}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := meshStdioOnce(cmd, svc, nil, dial, r, "local-node", ios); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, mesh.KindViaDirect) {
		t.Errorf("输出缺 Kind %q：\n%s", mesh.KindViaDirect, got)
	}
	if !strings.Contains(got, "42ms") {
		t.Errorf("输出缺 Latency（42ms）：\n%s", got)
	}
}

// TestMeshStdioOnce_ZeroLatencyKeepsOriginalFormat：Latency=0（单路径 Dial，非 SmartDial
// 竞速）时保持原格式 `已连接（%s）`——不显示 `, 0s` 后缀。
//
// 零回归依据：T7 实现改为 `res.Latency > 0` 才显示 Latency；若未来误删该判断恒显示
// `已连接（via-direct, 0s）`，本用例断言输出不含 ", " 即红。
func TestMeshStdioOnce_ZeroLatencyKeepsOriginalFormat(t *testing.T) {
	t.Parallel()

	svcList := `[{"name":"svc","node":"node-a","addr":"127.0.0.1:10022"}]`
	ts := httptest.NewServer(servicesHandler(&atomic.Int32{}, func() string { return svcList }))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	r := client.NewMeshTargetRefresher(svc, "svc")
	r.SetTTL(time.Hour)
	r.SetClock(fixedClock())

	// 注入 dial：Latency=0（单路径拨号结果）——断言输出**不含** ", "（即无 `, 0s` 后缀）。
	pcRead, _ := net.Pipe()
	defer pcRead.Close()
	dial := func(_ context.Context, _ *client.FileClient, _ webrtc.Signaler, _ *client.MeshService, _ string) (*mesh.Result, error) {
		return &mesh.Result{Conn: pcRead, Kind: mesh.KindViaDirect, Latency: 0}, nil
	}

	var out bytes.Buffer
	ios := cli.IOStreams{Out: &out, ErrOut: io.Discard, In: strings.NewReader("")}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := meshStdioOnce(cmd, svc, nil, dial, r, "local-node", ios); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if strings.Contains(got, ", ") {
		t.Errorf("Latency=0 不应显示 Latency 后缀（原格式零回归），实际输出含 \", \"：\n%s", got)
	}
	if !strings.Contains(got, mesh.KindViaDirect) {
		t.Errorf("输出缺 Kind %q：\n%s", mesh.KindViaDirect, got)
	}
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
	dial := func(_ context.Context, _ *client.FileClient, _ webrtc.Signaler, target *client.MeshService, _ string) (*mesh.Result, error) {
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
		// 重试拨号直到 listener 就绪（原 deadline + sleep 循环 → 条件等待，带内部轮询间隔）
		if !testutil.WaitForBool(3*time.Second, func() bool {
			c, derr = net.Dial("tcp", listenAddr)
			return derr == nil
		}) {
			return nil, derr
		}
		return c, nil
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
	// 等错误输出出现（原 deadline + sleep 轮询 → 条件等待）
	testutil.WaitForBool(30*time.Second, func() bool { return strings.Contains(errBuf.String(), "不可用") })
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

// TestMeshConnect_HasSmartFlag：mesh connect 提供 --smart 自动选路开关（默认关，
// 开启走 SmartDial 多路径竞速择优；默认关 = 现有固定顺序零回归）。
func TestMeshConnect_HasSmartFlag(t *testing.T) {
	t.Parallel()
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, nil)
	var connect *cobra.Command
	for _, sub := range cmd.Commands() {
		// 精确匹配 Use 避免 HasPrefix 误命中（如未来新增 connect-xxx 子命令）。
		if sub.Use == "connect <service> [-l :port]" {
			connect = sub
			break
		}
	}
	if connect == nil {
		t.Fatal("mesh 缺少 connect 子命令")
	}
	flag := connect.Flags().Lookup("smart")
	if flag == nil {
		t.Fatal("mesh connect 缺 --smart flag")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--smart 默认值 = %s, want false", flag.DefValue)
	}
}
