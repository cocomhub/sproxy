// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"golang.org/x/net/dns/dnsmessage"
)

// testMDNSLogger 返回输出到 io.Discard 的 slog.Logger（测试静音）。
func testMDNSLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testMDNSLoopback 开启 mDNS 组播 loopback 收敛（避免 Windows 防火墙弹窗），
// 测试结束自动恢复。含组播的测试开头调用。
func testMDNSLoopback(t *testing.T) {
	t.Helper()
	SetMDNSLoopbackOnly(true)
	t.Cleanup(func() { SetMDNSLoopbackOnly(false) })
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
	// 探测组播可用性：先试绑定，失败则跳过（CI 容器常无组播路由）。
	probe, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(mDNSIPv4), Port: port})
	if err != nil {
		t.Skipf("mDNS 组播不可用: %v", err)
	}
	probe.Close()

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
	if err := srvA.Start(ctx); err != nil {
		t.Skipf("srvA 加入组播失败（可能端口被占）: %v", err)
	}
	defer srvA.Close()
	if err := srvB.Start(ctx); err != nil {
		t.Skipf("srvB 加入组播失败（同机双实例可能不被支持）: %v", err)
	}
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
	probe, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(mDNSIPv4), Port: port})
	if err != nil {
		t.Skipf("mDNS 组播不可用: %v", err)
	}
	probe.Close()

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
	if err := srv.Start(ctx); err != nil {
		t.Skipf("加入组播失败: %v", err)
	}
	defer srv.Close()

	// 查询不存在的服务应返回 ErrMDNSServiceNotFound。
	if _, err := srv.LookupService(ctx, "no-such", 500*time.Millisecond); err != ErrMDNSServiceNotFound {
		t.Fatalf("LookupService(不存在) = %v, want ErrMDNSServiceNotFound", err)
	}
}
