// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"golang.org/x/net/dns/dnsmessage"
)

// testMDNSLogger 返回输出到 io.Discard 的 slog.Logger（测试静音）。
func testMDNSLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testMDNSLoopback 开启 mDNS 组播 loopback 收敛（组播只在本机 loopback 上收发），
// 测试结束自动恢复。含组播的测试开头调用。
//
// 收敛路径同时规避了 Windows 防火墙授权弹窗：Windows 上它绑的是**单播回环地址**而非
// 通配地址（见 listenMDNSLoopback / mdns_loopback_windows.go），实测不弹窗。因此这些
// 用例在本地 Windows 与 CI 一样实跑，**不需要任何跳过门控**。
func testMDNSLoopback(t *testing.T) {
	t.Helper()
	SetMDNSLoopbackOnly(true)
	t.Cleanup(func() { SetMDNSLoopbackOnly(false) })
}

// mdnsUnavailable 报告「mDNS 组播不可用」在当前环境下是否应升级为 FAIL：
// **仅 Windows CI 下为 true**（不静默跳过，否则收敛路径的回归会被绿掉的 SKIP 掩盖，
// 架空「Windows CI 依旧运行」的承诺）；其余为 false（Linux CI 容器常无组播路由，
// 跳过是合理语义）。
func mdnsUnavailable() bool {
	return runtime.GOOS == "windows" && os.Getenv("CI") != ""
}

// startMDNSOrSkip 启动 mDNS 服务器，失败按 mdnsUnavailable 分流：Windows CI 下
// **t.Fatal**，其余 **t.Skipf**。
//
// 为什么这里不能一律 skip：「同机多实例绑同一端口 + 组播互收」正是本轮收敛机制的
// 核心不变式——若 SO_REUSEADDR 或 JoinGroup 退化，Start 会报错，此时 SKIP（绿）会把
// 回归掩盖成"环境不支持"，必须让 Windows CI 红灯。非 Windows 保留 ubuntu 容器
// 「无组播路由 → skip」的既有语义。
func startMDNSOrSkip(t *testing.T, name string, s *MDNSServer, ctx context.Context) {
	t.Helper()
	if err := s.Start(ctx); err != nil {
		if mdnsUnavailable() {
			t.Fatalf("%s 启动 mDNS 失败（Windows CI 下不静默跳过）: %v", name, err)
		}
		t.Skipf("%s 启动 mDNS 失败: %v", name, err)
	}
}

// probeMDNSLoopback 探测 mDNS 组播可用性：调用**与用例完全相同**的 listenMDNSLoopback
// （同一份平台策略）并在成功后立即关闭。探测与用例同源，故"探测通过却跑不起来"不会
// 发生；反过来 loopback 组播真不可用时两者会一起失败，故失败必须可见（见下）。
//
// 失败时按 mdnsUnavailable 分流——**Windows CI 下 t.Fatal**；其余情形 t.Skipf（Linux CI
// 容器常无组播路由，跳过是合理语义）。
//
// 该探测不会触发 Windows 防火墙授权弹窗：它走收敛路径（Windows 上绑单播回环地址），
// 而非生产路径的 ListenMulticastUDP（内部绑通配地址，正是弹窗来源）。
func probeMDNSLoopback(t *testing.T, port int) {
	t.Helper()
	probe, _, err := listenMDNSLoopback(context.Background(), &net.UDPAddr{IP: net.ParseIP(mDNSIPv4), Port: port})
	if err != nil {
		if mdnsUnavailable() {
			t.Fatalf("mDNS 组播不可用（Windows CI 下不静默跳过）: %v", err)
		}
		t.Skipf("mDNS 组播不可用: %v", err)
	}
	_ = probe.Close()
}

func TestMDNSInstanceLabel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"nodeA", "nodeA"},
		{"my node", "my-node"},
		{"节点-1", "---1"}, // 非 ASCII 每 rune 替换为 `-`（"节点" 两个 CJK → 两个 `-`）
		{"", "mesh-node"},
		{"a/b?c", "a-b-c"},
	}
	for _, tc := range tests {
		if got := mdnsInstanceLabel(tc.in); got != tc.want {
			t.Errorf("mdnsInstanceLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// 超长标签截断到 63。
	long := make([]byte, 100)
	for i := range long {
		long[i] = 'x'
	}
	if got := mdnsInstanceLabel(string(long)); len(got) != 63 {
		t.Errorf("超长标签长度 = %d, want 63", len(got))
	}
}

func TestMDNSTXTRoundtrip(t *testing.T) {
	svc := []hub.Service{{Name: "echo", Addr: "127.0.0.1:2222"}, {Name: "app", Addr: "10.0.0.1:8080"}}
	srv, err := NewMDNS(MDNSConfig{
		NodeID:     "node-a",
		SignalAddr: "192.168.1.10:40001",
		Services:   svc,
		VirtualIP:  netip.MustParseAddr("100.64.0.5"),
	})
	if err != nil {
		t.Fatalf("NewMDNS: %v", err)
	}
	pairs := srv.txtPairs()
	got := map[string]bool{}
	for _, str := range pairs {
		got[str] = true
	}
	want := map[string]bool{
		"node=node-a":                true,
		"saddr=192.168.1.10%3A40001": true, // url.QueryEscape 转义 `:`
		"vip=100.64.0.5":             true, // S-3：虚拟 IP 广播进 TXT
		"svc.echo=127.0.0.1%3A2222":  true,
		"svc.app=10.0.0.1%3A8080":    true,
	}
	for k := range want {
		if !got[k] {
			t.Errorf("txtPairs 缺 %q（实际 %v）", k, pairs)
		}
	}
}

// TestMDNSAnnouncementRoundtrip 覆盖"构造宣告报文 → 解析 → 应用到对端缓存"全链路
// （无需组播，确定性单测）。
func TestMDNSAnnouncementRoundtrip(t *testing.T) {
	srv, err := NewMDNS(MDNSConfig{
		NodeID:     "node-a",
		SignalAddr: "192.168.1.10:40001",
		Services:   []hub.Service{{Name: "echo", Addr: "192.168.1.10:2222"}},
		IPs:        []net.IP{net.ParseIP("192.168.1.10")},
		VirtualIP:  netip.MustParseAddr("100.64.0.5"),
	})
	if err != nil {
		t.Fatalf("NewMDNS: %v", err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true})
	if serr := b.StartAnswers(); serr != nil {
		t.Fatalf("StartAnswers: %v", serr)
	}
	srv.appendRecords(&b)
	msg, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// 对端（BrowseOnly）解析该宣告并应发现 node-a。
	recv, err := NewMDNS(MDNSConfig{NodeID: "node-b", BrowseOnly: true})
	if err != nil {
		t.Fatalf("NewMDNS(recv): %v", err)
	}
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		t.Fatalf("解析报文: %v", err)
	}
	if !h.Response {
		t.Fatal("宣告报文应为响应")
	}
	for {
		if _, qerr := p.Question(); qerr != nil {
			if qerr == dnsmessage.ErrSectionDone {
				break
			}
			t.Fatalf("Question: %v", qerr)
		}
	}
	answers, err := p.AllAnswers()
	if err != nil {
		t.Fatalf("AllAnswers: %v", err)
	}
	for _, a := range answers {
		recv.applyAnswer(a)
	}

	peers := recv.Peers()
	if len(peers) != 1 {
		t.Fatalf("对端发现 %d 个节点, want 1（peers=%+v）", len(peers), peers)
	}
	got := peers[0]
	if got.NodeID != "node-a" {
		t.Errorf("NodeID = %q, want node-a", got.NodeID)
	}
	if got.SignalAddr != "192.168.1.10:40001" {
		t.Errorf("SignalAddr = %q, want 192.168.1.10:40001", got.SignalAddr)
	}
	if len(got.Services) != 1 || got.Services[0].Name != "echo" || got.Services[0].Addr != "192.168.1.10:2222" {
		t.Errorf("Services = %+v, want [{echo 192.168.1.10:2222}]", got.Services)
	}
	if len(got.IPs) != 1 || !got.IPs[0].Equal(net.ParseIP("192.168.1.10")) {
		t.Errorf("IPs = %v, want [192.168.1.10]", got.IPs)
	}
	if got.VirtualIP != netip.MustParseAddr("100.64.0.5") {
		t.Errorf("VirtualIP = %v, want 100.64.0.5（TXT vip= 往返）", got.VirtualIP)
	}
}

// TestMDNSSecretAuth（安全审查 D 回归）：配置共享密钥后，mDNS TXT 携带 HMAC 签名；
// 浏览方用正确密钥发现对端，错误密钥忽略（防广告伪造/MITM），无密钥 = LAN 信任放行。
// 用确定性 roundtrip（构造宣告 → 解析 → 应用到各密钥浏览方），不依赖组播。
func TestMDNSSecretAuth(t *testing.T) {
	svc := []hub.Service{{Name: "echo", Addr: "192.168.1.10:2222"}}
	srvA, err := NewMDNS(MDNSConfig{
		NodeID: "node-a", SignalAddr: "192.168.1.10:40001",
		Services: svc, Secret: "S",
	})
	if err != nil {
		t.Fatalf("NewMDNS(A): %v", err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true})
	if serr := b.StartAnswers(); serr != nil {
		t.Fatalf("StartAnswers: %v", serr)
	}
	srvA.appendRecords(&b)
	msg, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	var p dnsmessage.Parser
	if _, serr := p.Start(msg); serr != nil {
		t.Fatalf("解析: %v", serr)
	}
	for {
		if _, qerr := p.Question(); qerr != nil {
			if qerr == dnsmessage.ErrSectionDone {
				break
			}
			t.Fatalf("Question: %v", qerr)
		}
	}
	answers, err := p.AllAnswers()
	if err != nil {
		t.Fatalf("AllAnswers: %v", err)
	}
	applyTo := func(secret string) []MDNSPeer {
		recv, rerr := NewMDNS(MDNSConfig{NodeID: "node-x", BrowseOnly: true, Secret: secret})
		if rerr != nil {
			t.Fatalf("NewMDNS(recv): %v", rerr)
		}
		for _, a := range answers {
			recv.applyAnswer(a)
		}
		return recv.Peers()
	}
	// 正确密钥 → 发现 A。
	if peers := applyTo("S"); len(peers) != 1 || peers[0].NodeID != "node-a" {
		t.Fatalf("正确密钥应发现 node-a, got %+v", peers)
	}
	// 错误密钥 → 忽略（签名不匹配）。
	if peers := applyTo("T"); len(peers) != 0 {
		t.Fatalf("错误密钥不应发现 node-a, got %+v", peers)
	}
	// 无密钥（LAN 信任）→ 放行。
	if peers := applyTo(""); len(peers) != 1 {
		t.Fatalf("无密钥 LAN 信任应发现 node-a, got %+v", peers)
	}
}

// TestMDNSIgnoreOwnAnnouncement：节点不应把自身的宣告计入对端列表。
func TestMDNSIgnoreOwnAnnouncement(t *testing.T) {
	srv, err := NewMDNS(MDNSConfig{NodeID: "node-a", SignalAddr: "192.168.1.10:40001"})
	if err != nil {
		t.Fatalf("NewMDNS: %v", err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true})
	if serr := b.StartAnswers(); serr != nil {
		t.Fatalf("StartAnswers: %v", serr)
	}
	srv.appendRecords(&b)
	msg, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	var p dnsmessage.Parser
	if _, serr := p.Start(msg); serr != nil {
		t.Fatalf("解析: %v", serr)
	}
	for {
		if _, qerr := p.Question(); qerr != nil {
			if qerr == dnsmessage.ErrSectionDone {
				break
			}
			t.Fatalf("Question: %v", qerr)
		}
	}
	answers, err := p.AllAnswers()
	if err != nil {
		t.Fatalf("AllAnswers: %v", err)
	}
	for _, a := range answers {
		srv.applyAnswer(a)
	}
	if peers := srv.Peers(); len(peers) != 0 {
		t.Fatalf("自身宣告不应计入对端: %+v", peers)
	}
}

// waitMDNSPeer 轮询直到 s 发现 nodeID（或超时）。
func waitMDNSPeer(s *MDNSServer, nodeID string, timeout time.Duration) (MDNSPeer, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, p := range s.Peers() {
			if p.NodeID == nodeID {
				return p, true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return MDNSPeer{}, false
}

// TestMDNSDiscovery_TwoNodes 是 mDNS 局域网互发现的集成测试：同机两个实例加入同一
// 组播组，互相发现对方（node-id + 服务 + 信令端点）。组播在部分 CI/容器不可用时跳过。
func TestMDNSDiscovery_TwoNodes(t *testing.T) {
	testMDNSLoopback(t)
	port := 15353 // 测试专用端口，避免占用标准 5353
	probeMDNSLoopback(t, port)

	logger := testMDNSLogger()
	srvA, err := NewMDNS(MDNSConfig{
		NodeID: "node-a", SignalAddr: "192.168.1.10:40001",
		Services: []hub.Service{{Name: "echo", Addr: "192.168.1.10:2222"}},
		Port:     port, Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewMDNS(A): %v", err)
	}
	srvB, err := NewMDNS(MDNSConfig{
		NodeID: "node-b", SignalAddr: "192.168.1.11:40002",
		Services: []hub.Service{{Name: "ssh", Addr: "192.168.1.11:22"}},
		Port:     port, Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewMDNS(B): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	startMDNSOrSkip(t, "srvA", srvA, ctx)
	defer srvA.Close()
	startMDNSOrSkip(t, "srvB", srvB, ctx)
	defer srvB.Close()

	pa, ok := waitMDNSPeer(srvA, "node-b", 15*time.Second)
	if !ok {
		t.Fatal("srvA 未在超时内发现 node-b")
	}
	if pa.SignalAddr != "192.168.1.11:40002" {
		t.Errorf("node-b SignalAddr = %q, want 192.168.1.11:40002", pa.SignalAddr)
	}
	if len(pa.Services) != 1 || pa.Services[0].Name != "ssh" {
		t.Errorf("node-b Services = %+v, want [ssh]", pa.Services)
	}

	pb, ok := waitMDNSPeer(srvB, "node-a", 15*time.Second)
	if !ok {
		t.Fatal("srvB 未在超时内发现 node-a")
	}
	if pb.SignalAddr != "192.168.1.10:40001" {
		t.Errorf("node-a SignalAddr = %q, want 192.168.1.10:40001", pb.SignalAddr)
	}
	if len(pb.Services) != 1 || pb.Services[0].Name != "echo" {
		t.Errorf("node-a Services = %+v, want [echo]", pb.Services)
	}
}

// TestMDNSLookupService：LookupService 返回宣告指定服务的对端。
func TestMDNSLookupService(t *testing.T) {
	testMDNSLoopback(t)
	port := 15354
	probeMDNSLoopback(t, port)

	logger := testMDNSLogger()
	srv, err := NewMDNS(MDNSConfig{
		NodeID: "node-a", SignalAddr: "192.168.1.10:40001",
		Services: []hub.Service{{Name: "echo", Addr: "192.168.1.10:2222"}},
		Port:     port, Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewMDNS: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	startMDNSOrSkip(t, "srv", srv, ctx)
	defer srv.Close()

	// 查询不存在的服务应返回 ErrMDNSServiceNotFound。
	if _, err := srv.LookupService(ctx, "no-such", 500*time.Millisecond); err != ErrMDNSServiceNotFound {
		t.Fatalf("LookupService(不存在) = %v, want ErrMDNSServiceNotFound", err)
	}
}
