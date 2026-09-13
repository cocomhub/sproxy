// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

// remote_dialer_test.go 钉住 Y 二期 A 侧的 **mesh 远端拨号器**（`remote.Dialer` 实现）：
//   - 服务发现按 (node, service) **精确命中**，未宣告即报错且**不回落其它节点**（授权按节点绑定）；
//   - 选路：打洞优先；`AllowRelayFallback=false`（`transport: webrtc`）时打洞失败**不回落中继**；
//     `=true`（`transport: auto`）时回落中继；
//   - 无 signaler（未配 WebRTC 信令）⇒ 纯中继；
//   - 编译期实现 `remote.Dialer`。
//
// 全部用替身（假 hub 客户端 + 可注入 Punch），**不触网**。

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/remote"
	hubpkg "github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// fakeHubClient 是 MeshRelayClient 替身：记录 MeshServices/RelayStream 调用。
type fakeHubClient struct {
	services []client.MeshService
	svcErr   error

	mu        sync.Mutex
	relays    []string // "node@addr"
	relayErr  error
	relayConn net.Conn
}

func (f *fakeHubClient) MeshServices(context.Context) ([]client.MeshService, error) {
	if f.svcErr != nil {
		return nil, f.svcErr
	}
	return f.services, nil
}

func (f *fakeHubClient) RelayStream(_ context.Context, target, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.relays = append(f.relays, target+"@"+addr)
	f.mu.Unlock()
	if f.relayErr != nil {
		return nil, f.relayErr
	}
	return f.relayConn, nil
}

func (f *fakeHubClient) relayCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.relays...)
}

// pipeConn 返回一对内存连接（测试用「已建立的连接」）。
func pipeConn(t *testing.T) net.Conn {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c2.Close() })
	return c1
}

// svcEntry 构造一条服务宣告。
func svcEntry(node, name, addr string) client.MeshService {
	return client.MeshService{Node: node, Name: name, Addr: addr}
}

// TestRemoteDialer_PunchFirst 钉住打洞优先：成功即返回，**不碰中继**。
func TestRemoteDialer_PunchFirst(t *testing.T) {
	punchConn := pipeConn(t)
	hub := &fakeHubClient{services: []client.MeshService{svcEntry("nodeB", remote.ServiceNameWrite, "127.0.0.1:19001")}}
	var punched []string

	d := NewRemoteDialer(RemoteDialerConfig{
		Client:  hub,
		Service: remote.ServiceNameWrite,
		Punch: func(_ context.Context, target *client.MeshService) (net.Conn, error) {
			punched = append(punched, target.Node+"@"+target.Addr)
			return punchConn, nil
		},
		AllowRelayFallback: true,
	})
	conn, err := d.Dial(context.Background(), "nodeB")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if conn != punchConn {
		t.Fatal("应返回打洞连接")
	}
	if len(punched) != 1 || punched[0] != "nodeB@127.0.0.1:19001" {
		t.Fatalf("打洞目标不符: %v", punched)
	}
	if got := hub.relayCalls(); len(got) != 0 {
		t.Fatalf("打洞成功后不得回落中继: %v", got)
	}
}

// TestRemoteDialer_NoFallbackForExplicitWebRTC 钉住 `transport: webrtc` 语义：
// 打洞失败**即报错**，绝不回落中继（否则「显式声明直连」被静默降级，掩盖配置/网络问题）。
func TestRemoteDialer_NoFallbackForExplicitWebRTC(t *testing.T) {
	punchErr := errors.New("stun 不可达")
	hub := &fakeHubClient{services: []client.MeshService{svcEntry("nodeB", "volread", "127.0.0.1:19000")}}

	d := NewRemoteDialer(RemoteDialerConfig{
		Client:  hub,
		Service: "volread",
		Punch: func(context.Context, *client.MeshService) (net.Conn, error) {
			return nil, punchErr
		},
		AllowRelayFallback: false,
	})
	_, err := d.Dial(context.Background(), "nodeB")
	if err == nil {
		t.Fatal("打洞失败且不允许回落时应报错")
	}
	if !errors.Is(err, punchErr) {
		t.Fatalf("应保留打洞错误（errors.Is 可判定）, got %v", err)
	}
	if got := hub.relayCalls(); len(got) != 0 {
		t.Fatalf("显式 webrtc 不得回落中继: %v", got)
	}
}

// TestRemoteDialer_FallbackForAuto 钉住 `transport: auto` 语义：打洞失败回落中继。
func TestRemoteDialer_FallbackForAuto(t *testing.T) {
	relayConn := pipeConn(t)
	hub := &fakeHubClient{
		services:  []client.MeshService{svcEntry("nodeB", "volread", "127.0.0.1:19000")},
		relayConn: relayConn,
	}
	d := NewRemoteDialer(RemoteDialerConfig{
		Client:  hub,
		Service: "volread",
		Punch: func(context.Context, *client.MeshService) (net.Conn, error) {
			return nil, errors.New("打洞失败")
		},
		AllowRelayFallback: true,
	})
	conn, err := d.Dial(context.Background(), "nodeB")
	if err != nil {
		t.Fatalf("auto 应回落中继并成功: %v", err)
	}
	if conn != relayConn {
		t.Fatal("应返回中继连接")
	}
	if got := hub.relayCalls(); len(got) != 1 || got[0] != "nodeB@127.0.0.1:19000" {
		t.Fatalf("中继目标不符: %v", got)
	}
}

// TestRemoteDialer_ServiceDiscoveryIsExact 钉住服务发现语义：
//   - 目标节点未宣告该服务名 ⇒ 报错，且**不拨其它节点**（哪怕别的节点宣告了同名服务）；
//   - 目标节点不存在 ⇒ 报错。
func TestRemoteDialer_ServiceDiscoveryIsExact(t *testing.T) {
	hub := &fakeHubClient{services: []client.MeshService{
		svcEntry("nodeA", "volread", "127.0.0.1:19000"), // 只读面：不该被写面拨到
		svcEntry("nodeC", remote.ServiceNameWrite, "127.0.0.1:19001"),
	}}
	d := NewRemoteDialer(RemoteDialerConfig{Client: hub, Service: remote.ServiceNameWrite})

	if _, err := d.Dial(context.Background(), "nodeA"); err == nil {
		t.Fatal("nodeA 未宣告 volwrite 应报错（不得借 nodeC 的同名服务）")
	}
	if _, err := d.Dial(context.Background(), "nodeZ"); err == nil {
		t.Fatal("未知节点应报错")
	}
	if got := hub.relayCalls(); len(got) != 0 {
		t.Fatalf("未命中服务时不得发起任何中继: %v", got)
	}
}

// TestRemoteDialer_NoSignalerMeansRelayOnly 钉住「未配 WebRTC 信令 ⇒ 纯中继」：
// 此时 Punch 为 nil（无 signaler 即无打洞），直接走中继（与现状 relay 载体等价）。
func TestRemoteDialer_NoSignalerMeansRelayOnly(t *testing.T) {
	relayConn := pipeConn(t)
	hub := &fakeHubClient{
		services:  []client.MeshService{svcEntry("nodeB", "volread", "127.0.0.1:19000")},
		relayConn: relayConn,
	}
	d := NewRemoteDialer(RemoteDialerConfig{Client: hub, Service: "volread"}) // 无 Signaler ⇒ 无 Punch
	conn, err := d.Dial(context.Background(), "nodeB")
	if err != nil {
		t.Fatalf("无信令时应走中继: %v", err)
	}
	if conn != relayConn {
		t.Fatal("应返回中继连接")
	}
}

// TestRemoteDialer_EmptyServiceDefaultsToReadFace 钉住缺省服务名 = 只读面
// （调用方漏配时不至于把读当写；写面必须显式声明）。
func TestRemoteDialer_EmptyServiceDefaultsToReadFace(t *testing.T) {
	relayConn := pipeConn(t)
	hub := &fakeHubClient{
		services:  []client.MeshService{svcEntry("nodeB", remote.ServiceName, "127.0.0.1:19000")},
		relayConn: relayConn,
	}
	d := NewRemoteDialer(RemoteDialerConfig{Client: hub})
	if d.service != remote.ServiceName {
		t.Fatalf("缺省服务名应为 %q, got %q", remote.ServiceName, d.service)
	}
	if _, err := d.Dial(context.Background(), "nodeB"); err != nil {
		t.Fatalf("缺省服务名应命中只读面: %v", err)
	}
}

// TestRemoteDialer_DiscoveryErrorPropagates 钉住服务发现失败原样上抛（不吞、不改语义）。
func TestRemoteDialer_DiscoveryErrorPropagates(t *testing.T) {
	boom := errors.New("hub 不可达")
	hub := &fakeHubClient{svcErr: boom}
	d := NewRemoteDialer(RemoteDialerConfig{Client: hub, Service: "volread"})
	_, err := d.Dial(context.Background(), "nodeB")
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("应保留服务发现错误, got %v", err)
	}
	if !strings.Contains(err.Error(), "服务发现") {
		t.Fatalf("错误应带上下文（服务发现）, got %v", err)
	}
}

// TestRemoteDialer_TypedNilSignalerGoesRelay 钉住 **typed nil 陷阱**的回归：
// `var s *hub.HubSignaler = nil; cfg.Signaler = s`（接口非 nil、指针为 nil）时，
// 拨号器**必须**走中继而不是「以为有信令」去解引用 nil 而 panic。
//
// 背景：本片（S4a）把信令入参放宽为 `webrtc.Signaler` 接口后，`cmd/sproxy` 一处 deps 字段仍是
// `*hub.HubSignaler`（未配置 = nil 指针）⇒ 传进接口即 typed nil ⇒ 真拨号路径 panic（实测踩到）。
// 守卫 `signalerUsable` 同时排除 nil 接口与 typed nil。
func TestRemoteDialer_TypedNilSignalerGoesRelay(t *testing.T) {
	relayConn := pipeConn(t)
	hub := &fakeHubClient{
		services:  []client.MeshService{svcEntry("nodeB", "volread", "127.0.0.1:19000")},
		relayConn: relayConn,
	}
	var typedNil *hubpkg.HubSignaler // nil 指针（与包名同名的局部变量 hub 会遮蔽包名，故用别名）
	d := NewRemoteDialer(RemoteDialerConfig{
		Client:   hub,
		Service:  "volread",
		Signaler: typedNil, // ⇒ 接口非 nil（陷阱）
		// AllowRelayFallback 无所谓：压根不该尝试打洞。
	})
	conn, err := d.Dial(context.Background(), "nodeB")
	if err != nil {
		t.Fatalf("typed nil 信令应退化为纯中继而非报错/panic: %v", err)
	}
	if conn != relayConn {
		t.Fatal("应返回中继连接")
	}
	if got := hub.relayCalls(); len(got) != 1 {
		t.Fatalf("应走中继一次: %v", got)
	}
}
