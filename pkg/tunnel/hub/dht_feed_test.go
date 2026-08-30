// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// TestHubServer_SetDHT_FeedsRegistration：HubServer 注入 DHT 后，节点注册时喂入 DHT
// （路由表仍权威，DHT 只作候选节点来源）。路由表与 DHT 都应包含该节点。
func TestHubServer_SetDHT_FeedsRegistration(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	dht := newMemoryDHT()
	srv := NewHubServer(rt, NewAuthenticator([]AccessKey{{Key: testAK, Secret: testSK}}), log)
	srv.SetDHT(dht)
	dial, serverConn, _ := pipeXfer()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	srvDone := make(chan error, 1)
	go func() {
		c := serverConn()
		if c == nil {
			srvDone <- context.DeadlineExceeded
			return
		}
		srvDone <- srv.HandleConn(ctx, c)
	}()
	clientConn, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	proof, ts, nonce := testRegCred(t, "node-a")
	if err := clientConn.Send(ctx, NewRegisterFrame("node-a", testAK, proof, ts, nonce, Meta{Addr: "192.168.1.1:9000"})); err != nil {
		t.Fatal(err)
	}
	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_OK, got error: %v", ackErr)
	}
	if string(ack) != RegisterAckOK {
		t.Fatalf("expected %q, got %q", RegisterAckOK, string(ack))
	}

	// 路由表权威（既有行为）。
	if !rt.Has("node-a") {
		t.Fatal("node-a 未注册到路由表")
	}
	// DHT 候选喂入。
	info, lerr := dht.Lookup(ctx, "node-a")
	if lerr != nil {
		t.Fatalf("DHT 应含 node-a（注册喂入）: %v", lerr)
	}
	if info.Meta["addr"] != "192.168.1.1:9000" {
		t.Errorf("DHT PeerInfo addr = %q, want 192.168.1.1:9000", info.Meta["addr"])
	}
	if info.Meta["mesh"] != "" {
		t.Errorf("DHT PeerInfo mesh = %q, want 默认 mesh", info.Meta["mesh"])
	}

	// 收尾：关闭客户端连接，HandleConn 应返回。
	_ = clientConn.Close()
	select {
	case err := <-srvDone:
		if err != nil && !errors.Is(err, xfer.ErrConnClosed) && !errors.Is(err, context.Canceled) {
			t.Logf("HandleConn returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
}

// TestHubServer_SetDHT_SkipsTransientNodes（安全审查 #2 回归）：瞬态临时身份
// （mesh-/disc-/p2p- 拨号临时 node-id）不应喂入 DHT——它们拨号后即注销且 DHT 无
// 移除路径，喂入会造成幽灵节点永久污染发现列表并挤占 k-bucket。
func TestHubServer_SetDHT_SkipsTransientNodes(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	dht := newMemoryDHT()
	srv := NewHubServer(rt, NewAuthenticator([]AccessKey{{Key: testAK, Secret: testSK}}), log)
	srv.SetDHT(dht)
	dial, serverConn, _ := pipeXfer()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	srvDone := make(chan error, 1)
	go func() {
		c := serverConn()
		if c == nil {
			srvDone <- context.DeadlineExceeded
			return
		}
		srvDone <- srv.HandleConn(ctx, c)
	}()
	clientConn, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	// 注册一个 mesh- 临时身份（ExactNode=false 的拨号方临时 node-id）。
	proof, ts, nonce := testRegCred(t, "mesh-tmp-1234abcd5678ef90")
	if err := clientConn.Send(ctx, NewRegisterFrame("mesh-tmp-1234abcd5678ef90", testAK, proof, ts, nonce, Meta{})); err != nil {
		t.Fatal(err)
	}
	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_OK, got error: %v", ackErr)
	}
	if string(ack) != RegisterAckOK {
		t.Fatalf("expected %q, got %q", RegisterAckOK, string(ack))
	}

	// 路由表应包含（临时节点也注册，转发寻址用）。
	if !rt.Has("mesh-tmp-1234abcd5678ef90") {
		t.Fatal("mesh-tmp-123 应注册到路由表")
	}
	// DHT 不应包含（跳过瞬态临时身份）。
	if _, lerr := dht.Lookup(ctx, "mesh-tmp-1234abcd5678ef90"); lerr == nil {
		t.Fatal("瞬态临时节点不应喂入 DHT")
	}

	_ = clientConn.Close()
	select {
	case <-srvDone:
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
}

// TestIsTransientNodeID：瞬态临时身份识别（S-4 收紧为完整形态 <prefix>-<base>-<16hex>，
// 避免误伤以 mesh-/p2p- 开头的合法稳定节点名如回落字面量 "mesh-node"）。
func TestIsTransientNodeID(t *testing.T) {
	// 完整形态临时身份 → true。
	for _, id := range []string{
		"disc-node-a-abc",                      // disc 前缀专用（注册时 ParseDiscNodeID 严格校验）
		"mesh-host-xyz-1234abcd5678ef90",       // mesh 拨号临时身份
		"p2p-node-123-1234abcd5678ef90",        // p2p 拨号临时身份
		"mesh-base-with-dash-1234abcd5678ef90", // base 可含 '-'，尾段 16 hex
	} {
		if !isTransientNodeID(id) {
			t.Errorf("isTransientNodeID(%q) 应为 true", id)
		}
	}
	// 非瞬态：稳定节点 / 尾段非 16 hex 的 mesh-/p2p- 前缀名（如回落字面量 "mesh-node"）。
	for _, id := range []string{
		"node-a", "mesh", "stable-node",
		"mesh-host-xyz", // 尾段非 16 hex → 稳定节点
		"p2p-node-123",  // 尾段非 16 hex → 稳定节点
		"mesh-node",     // 主机名不可解析的 mesh node 回落字面量
		"mesh-",         // 裸前缀不再判瞬态（完整形态才判）
	} {
		if isTransientNodeID(id) {
			t.Errorf("isTransientNodeID(%q) 应为 false", id)
		}
	}
}

// TestHubServer_NilDHT_NoFeed：未注入 DHT 时注册不 panic、路由表行为不变。
func TestHubServer_NilDHT_NoFeed(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator([]AccessKey{{Key: testAK, Secret: testSK}}), testutil.DiscardLogger())
	// 不调 SetDHT（默认 nil）。

	conn := &mockxfer.MockConn{
		ReceiveFn: func(context.Context) ([]byte, error) {
			return nil, errors.New("ws read timeout")
		},
	}
	var mu sync.Mutex
	var sent []string
	conn.SendFn = func(_ context.Context, msg []byte) error {
		mu.Lock()
		sent = append(sent, string(msg))
		mu.Unlock()
		return nil
	}
	_ = srv.HandleConn(context.Background(), conn)

	mu.Lock()
	defer mu.Unlock()
	for _, m := range sent {
		if strings.HasPrefix(m, RegisterAckErr) {
			t.Fatalf("未读到注册帧不应回 REG_ERR, got %q", m)
		}
	}
}
