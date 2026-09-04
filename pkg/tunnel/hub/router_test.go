// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// pipeXfer 返回一对经过内存管道互连的 xfer 拨号函数。
// client 拨号产生客户端 Conn；serverConn 返回服务端 Conn。
func pipeXfer() (dial func(ctx context.Context) (xfer.Conn, error), serverConn func() xfer.Conn, cleanup func()) {
	serverCh := make(chan xfer.Conn, 4)
	serverConn = func() xfer.Conn {
		select {
		case c := <-serverCh:
			return c
		case <-time.After(3 * time.Second):
			return nil
		}
	}
	dial = func(ctx context.Context) (xfer.Conn, error) {
		a, b := xfertest.Pipe()
		serverCh <- b
		return a, nil
	}
	cleanup = func() {}
	return dial, serverConn, cleanup
}

// TestHubServer_RegisterReadFailure_SilentNoRegErr（P1-7 回归）：
// 未读到注册帧（超时/WS 网络错误）时 hub 不得回发 REG_ERR——否则客户端
// isTerminalRelayError 把纯网络抖动判为终态，relay 守护进程永久退出。
// testRegFrameJSON 构造带 AK/proof/ts/nonce 的注册帧 JSON（extra 为可选额外字段 JSON 片段，
// 如 capabilities/meta）。每次调用生成唯一 nonce，满足 Authenticator 的 nonce 去重。
func testRegFrameJSON(t *testing.T, nodeID, extra string) string {
	t.Helper()
	proof, ts, nonce := testRegCred(t, nodeID)
	extraField := ""
	if extra != "" {
		extraField = "," + extra
	}
	return fmt.Sprintf(`{"node_id":%q,"access_key":%q,"access_key_proof":%q,"ts":%d,"nonce":%q%s}`, nodeID, testAK, proof, ts, nonce, extraField)
}

func TestHubServer_RegisterReadFailure_SilentNoRegErr(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), testutil.DiscardLogger())

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
			t.Fatalf("未读到注册帧不应回 REG_ERR（会被客户端误判终态），got %q", m)
		}
	}
}

func TestHubServerRegisterAndRemove(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), log)
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

	// 节点侧：先发一条注册帧（带 AK/proof+ts/nonce → JSON 帧；裸字节回退见 TestHubServerBareNodeID）
	proof, ts, nonce := testRegCred(t, "node-a")
	if err := clientConn.Send(ctx, NewRegisterFrame("node-a", testAK, proof, ts, nonce, Meta{})); err != nil {
		t.Fatal(err)
	}

	// 客户端侧验证 REG_OK 帧内容（I49）：未声明 per-node-secret 能力 → 纯 "REG_OK"
	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_OK frame, got error: %v", ackErr)
	}
	if string(ack) != RegisterAckOK {
		t.Fatalf("expected %q, got %q", RegisterAckOK, string(ack))
	}

	// 注册应已生效
	if !rt.Has("node-a") {
		t.Fatal("node-a not registered")
	}

	// 关闭客户端连接触发自动移除
	_ = clientConn.Close()
	select {
	case err := <-srvDone:
		if err != nil && !errors.Is(err, xfer.ErrConnClosed) && !errors.Is(err, context.Canceled) {
			t.Logf("HandleConn returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
	if rt.Has("node-a") {
		t.Fatal("node-a should have been removed")
	}
}

func TestHubServerBadToken(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), log)
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

	// 错误 proof：带有效 ts/nonce（通过新鲜度+去重），proof 故意错误 → ErrInvalidAccessKeyProof
	_, badTS, badNonce := testRegCred(t, "node-b")
	if err := clientConn.Send(ctx, fmt.Appendf(nil, `{"node_id":"node-b","access_key":%q,"access_key_proof":"deadbeef","ts":%d,"nonce":%q}`, testAK, badTS, badNonce)); err != nil {
		t.Fatal(err)
	}

	// 客户端侧验证 REG_ERR 帧内容（I49）：isTerminalRelayError 唯一采信的依据
	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_ERR frame, got error: %v", ackErr)
	}
	ackStr := string(ack)
	if !strings.HasPrefix(ackStr, RegisterAckErr) {
		t.Fatalf("expected REG_ERR prefix, got %q", ackStr)
	}
	if !strings.Contains(ackStr, "invalid access key") {
		t.Fatalf("expected 'invalid access key' in REG_ERR, got %q", ackStr)
	}

	// 出错场景：HubServer 读完注册帧并鉴权失败，应尽快返回
	select {
	case err := <-srvDone:
		if err == nil {
			t.Fatal("expected authentication error")
		}
		if !errors.Is(err, ErrInvalidAccessKeyProof) {
			t.Fatalf("expected ErrInvalidAccessKeyProof, got: %v", err)
		}
		if err := clientConn.Close(); err != nil {
			t.Logf("close client: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return on bad proof")
	}
	if rt.Has("node-b") {
		t.Fatal("node-b must not be registered")
	}
}

func TestHubServer_TryHandleConn_MaxConns(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), log, 1)

	ctx := t.Context()

	// 第一个连接：信号量空，应被接受
	client1, server1 := xfertest.Pipe()
	if !srv.TryHandleConn(ctx, server1) {
		t.Fatal("expected first connection to be accepted")
	}

	// 第二个连接：信号量已满，应被拒绝（TryHandleConn 返回 false，调用方负责 Close）
	client2, server2 := xfertest.Pipe()
	if srv.TryHandleConn(ctx, server2) {
		t.Fatal("expected second connection to be rejected when maxConns=1")
	}
	_ = client2.Close()
	_ = server2.Close()

	// 关闭第一个连接，处理 goroutine 结束后应释放名额
	_ = client1.Close()
	_ = server1.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		client3, server3 := xfertest.Pipe()
		if srv.TryHandleConn(ctx, server3) {
			_ = client3.Close()
			_ = server3.Close()
			return
		}
		_ = client3.Close()
		_ = server3.Close()
		if time.Now().After(deadline) {
			t.Fatal("semaphore not released after conn close")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHubServer_TryHandleConn_NoLimit(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), log) // 不传上限 = 无上限
	ctx := t.Context()

	client, server := xfertest.Pipe()
	if !srv.TryHandleConn(ctx, server) {
		t.Fatal("expected connection accepted when maxConns unset")
	}
	_ = client.Close()
	_ = server.Close()
}

// TestHubServerBareNodeID 覆盖 readRegisterFrame 的裸字节回退分支（I48）+ 认证驱动 fail-closed：
// 非 JSON 裸字符串被当作 nodeID，但裸帧无 AK/proof 凭据 → 注册被拒绝（REG_ERR），节点不注册。
// （nil auth 现视为 fail-closed，见 NewHubServer；本测试用合法 accessKeys 验证拒绝路径。）
func TestHubServerBareNodeID(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), log)
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

	// 裸字节注册帧：非 JSON，readRegisterFrame 走裸字符串容错分支；但无 AK/proof 凭据 → 拒绝。
	if err := clientConn.Send(ctx, []byte("node-bare")); err != nil {
		t.Fatal(err)
	}

	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_ERR frame, got error: %v", ackErr)
	}
	if !strings.HasPrefix(string(ack), RegisterAckErr) {
		t.Fatalf("expected REG_ERR, got %q", string(ack))
	}
	if rt.Has("node-bare") {
		t.Fatal("node-bare should NOT be registered without access key proof")
	}

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

// TestHubServerRegisterSecretCapability 验证 per-node secret 下发（I1）：
// 声明 per-node-secret 能力的节点，REG_OK 携带 secret，且 LookupInfo 返回一致。
func TestHubServerRegisterSecretCapability(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), log)
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

	// 声明 per-node-secret 能力：REG_OK 应携带 "<base64url secret>"
	frame := testRegFrameJSON(t, "node-sec", `"capabilities":["per-node-secret"]`)
	if err := clientConn.Send(ctx, []byte(frame)); err != nil {
		t.Fatal(err)
	}

	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_OK frame, got error: %v", ackErr)
	}
	ackStr := string(ack)
	prefix := RegisterAckOK + registerAckSecretSep
	if !strings.HasPrefix(ackStr, prefix) {
		t.Fatalf("expected %q prefix, got %q", prefix, ackStr)
	}
	wantSecret := strings.TrimPrefix(ackStr, prefix)
	if wantSecret == "" {
		t.Fatal("expected non-empty per-node secret in REG_OK")
	}

	// LookupInfo 应返回同一下发的 secret
	info, ok := rt.LookupInfo("node-sec")
	if !ok {
		t.Fatal("node-sec should be registered")
	}
	if info.Secret == "" {
		t.Fatal("expected non-empty per-node secret in NodeInfo")
	}
	if info.Secret != wantSecret {
		t.Fatalf("secret mismatch: ack=%q lookup=%q", wantSecret, info.Secret)
	}

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

// TestHubServerRegisterNoCapabilityPlainAck 验证未声明能力的节点：
// REG_OK 为纯 "REG_OK"，NodeInfo.Secret 为空（行为不变）。
func TestHubServerRegisterNoCapabilityPlainAck(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), log)
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

	// 未声明能力：REG_OK 为纯 "REG_OK"
	frame := testRegFrameJSON(t, "node-plain", "")
	if err := clientConn.Send(ctx, []byte(frame)); err != nil {
		t.Fatal(err)
	}

	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_OK frame, got error: %v", ackErr)
	}
	if string(ack) != RegisterAckOK {
		t.Fatalf("expected %q, got %q", RegisterAckOK, string(ack))
	}

	info, ok := rt.LookupInfo("node-plain")
	if !ok {
		t.Fatal("node-plain should be registered")
	}
	if info.Secret != "" {
		t.Fatalf("expected empty secret for node without capability, got %q", info.Secret)
	}

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

// TestHubServerJSONMissingNodeID 覆盖 S2：合法 JSON 但缺 node_id 应报错并回发
// REG_ERR（而非把 JSON 垃圾整串当裸串回退）。
func TestHubServerJSONMissingNodeID(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), log)
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

	// 合法 JSON 但缺 node_id：readRegisterFrame 返回错误
	if err := clientConn.Send(ctx, []byte(`{"foo":"bar"}`)); err != nil {
		t.Fatal(err)
	}

	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_ERR frame, got error: %v", ackErr)
	}
	ackStr := string(ack)
	if !strings.HasPrefix(ackStr, RegisterAckErr) {
		t.Fatalf("expected REG_ERR prefix, got %q", ackStr)
	}
	if !strings.Contains(ackStr, "bad register frame") {
		t.Fatalf("expected 'bad register frame' in REG_ERR, got %q", ackStr)
	}

	select {
	case err := <-srvDone:
		if err == nil {
			t.Fatal("expected readRegisterFrame error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
	if rt.NodeCount("") != 0 {
		t.Fatalf("invalid frame must not register a node, count=%d", rt.NodeCount(""))
	}
}

var _ = mux.RoleDialer

// discProof 计算 mesh discovery 临时注册的 real_node_id 证明（与 hub 端校验同式）。
func discProof(secret, nodeID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(nodeID))
	return hex.EncodeToString(mac.Sum(nil))
}

// registerNodeAndGetSecret 注册一个声明 per-node-secret 能力的节点，返回下发 secret
// 与 keepAlive（调用方在断言完成后调用以关闭连接）。连接保持打开期间节点在 hub
// 路由表在线——否则 hub 在连接断开即 RemoveIfOwned 移除节点，后续 disc 注册找不到目标。
func registerNodeAndGetSecret(t *testing.T, srv *HubServer, nodeID string) (string, func()) {
	t.Helper()
	dial, serverConn, _ := pipeXfer()
	ctx := t.Context()
	go func() {
		c := serverConn()
		if c == nil {
			return
		}
		_ = srv.HandleConn(ctx, c)
	}()
	clientConn, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	frame := testRegFrameJSON(t, nodeID, `"capabilities":["per-node-secret"]`)
	if err := clientConn.Send(ctx, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("register %s: %v", nodeID, ackErr)
	}
	ackStr := string(ack)
	prefix := RegisterAckOK + registerAckSecretSep
	if !strings.HasPrefix(ackStr, prefix) {
		t.Fatalf("register %s: 期望 REG_OK, got %q", nodeID, ackStr)
	}
	return strings.TrimPrefix(ackStr, prefix), func() { _ = clientConn.Close() }
}

// registerRawFrame 发送原始注册帧并返回 ACK 串（REG_OK:... / REG_ERR:...）与
// keepAlive（注册成功且需断言节点在线时，调用方在断言完成后调用关闭连接）。
func registerRawFrame(t *testing.T, srv *HubServer, frame string) (string, func()) {
	t.Helper()
	dial, serverConn, _ := pipeXfer()
	ctx := t.Context()
	go func() {
		c := serverConn()
		if c == nil {
			return
		}
		_ = srv.HandleConn(ctx, c)
	}()
	clientConn, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientConn.Send(ctx, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("receive ack: %v", ackErr)
	}
	return string(ack), func() { _ = clientConn.Close() }
}

// TestHubServer_DiscIdentity_ValidProof：disc 临时注册携带真实节点 per-node secret
// 派生的 HMAC 证明 → 注册成功且 RealNodeID 记录。
func TestHubServer_DiscIdentity_ValidProof(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), testutil.DiscardLogger())
	secret, closeReal := registerNodeAndGetSecret(t, srv, "victim-a")
	defer closeReal()
	if secret == "" {
		t.Fatal("victim-a per-node secret 为空")
	}
	proof := discProof(secret, "victim-a")
	discNode := "disc-victim-a-12345678ab"
	ack, closeDisc := registerRawFrame(t, srv, testRegFrameJSON(t, discNode,
		fmt.Sprintf(`"meta":{"real_node_id":"victim-a","real_node_proof":%q},"capabilities":["per-node-secret"]`, proof)))
	defer closeDisc()
	if !strings.HasPrefix(ack, RegisterAckOK) {
		t.Fatalf("期望 REG_OK, got %q", ack)
	}
	info, ok := rt.LookupInfo("disc-victim-a-12345678ab")
	if !ok {
		t.Fatal("disc 临时节点未注册")
	}
	if info.RealNodeID != "victim-a" {
		t.Fatalf("RealNodeID = %q, want victim-a", info.RealNodeID)
	}
}

// meshRegFrameJSON 构造带指定 AK 的注册帧 JSON（与 testRegFrameJSON 同构，
// 但 AK 可指定为带 mesh 的 access key，供 mesh 分表断言使用）。
func meshRegFrameJSON(t *testing.T, nodeID, ak string) string {
	t.Helper()
	proof, ts, nonce := testRegCred(t, nodeID)
	return fmt.Sprintf(`{"node_id":%q,"access_key":%q,"access_key_proof":%q,"ts":%d,"nonce":%q}`, nodeID, ak, proof, ts, nonce)
}

// TestHubServer_RegisterMeshFromAK（M-9）：注册 AK 解析出 mesh 并写入 NodeInfo.Mesh，
// 节点落入对应 mesh 的独立 RouteTable（跨 mesh 不可见）。
func TestHubServer_RegisterMeshFromAK(t *testing.T) {
	const (
		akA = "sk-mesh-a-0011223344556677" // AccessKeyMesh → "mesh-a"
		akB = "sk-mesh-b-8899aabbccddeeff" // AccessKeyMesh → "mesh-b"
	)
	log := testutil.DiscardLogger()
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing(accesskey.KeyPair{Key: akA, Secret: testSK}, accesskey.KeyPair{Key: akB, Secret: testSK})), log)

	ackA, closeA := registerRawFrame(t, srv, meshRegFrameJSON(t, "node-a", akA))
	defer closeA()
	if !strings.HasPrefix(ackA, RegisterAckOK) {
		t.Fatalf("node-a 注册失败: %q", ackA)
	}
	ackB, closeB := registerRawFrame(t, srv, meshRegFrameJSON(t, "node-b", akB))
	defer closeB()
	if !strings.HasPrefix(ackB, RegisterAckOK) {
		t.Fatalf("node-b 注册失败: %q", ackB)
	}

	infoA, ok := rt.LookupInfo("node-a")
	if !ok {
		t.Fatal("node-a 未注册")
	}
	if infoA.Mesh != "mesh-a" {
		t.Fatalf("node-a.Mesh = %q, want mesh-a（注册 AK 应解析出 mesh）", infoA.Mesh)
	}
	if infoB, ok := rt.LookupInfo("node-b"); !ok || infoB.Mesh != "mesh-b" {
		t.Fatalf("node-b.Mesh = %q, want mesh-b", infoB.Mesh)
	}
	// 各自 List(mesh) 只见本 mesh。
	meshAList := rt.List("mesh-a")
	if len(meshAList) != 1 || meshAList[0].ID != "node-a" {
		t.Fatalf("List(mesh-a) = %+v, want 仅 node-a", meshAList)
	}
	if got := rt.List("mesh-b"); len(got) != 1 || got[0].ID != "node-b" {
		t.Fatalf("List(mesh-b) = %+v, want 仅 node-b", got)
	}
	// 节点计数按 mesh 隔离。
	if rt.NodeCount("mesh-a") != 1 || rt.NodeCount("mesh-b") != 1 {
		t.Fatalf("NodeCount(mesh-a)=%d NodeCount(mesh-b)=%d, want 1/1", rt.NodeCount("mesh-a"), rt.NodeCount("mesh-b"))
	}
}

// TestHubServer_DiscIdentity_ForgedRejected：伪造 real_node_id 证明（无真实节点
// per-node secret）或 real_node_id 与 disc base 不匹配 → hub fail-closed 拒绝注册
// （防冒充他人污染 accept 侧链路池）。
func TestHubServer_DiscIdentity_ForgedRejected(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), testutil.DiscardLogger())
	_, closeReal := registerNodeAndGetSecret(t, srv, "victim-a")
	defer closeReal()

	// 伪造证明（冒充 victim-a，但无其 per-node secret）。
	discNode1 := "disc-victim-a-99999999aa"
	ack, closeDisc := registerRawFrame(t, srv, testRegFrameJSON(t, discNode1,
		`"meta":{"real_node_id":"victim-a","real_node_proof":"deadbeef"},"capabilities":["per-node-secret"]`))
	defer closeDisc()
	if !strings.HasPrefix(ack, RegisterAckErr) {
		t.Fatalf("期望 REG_ERR（伪造证明被拒）, got %q", ack)
	}
	if rt.Has("disc-victim-a-99999999aa") {
		t.Fatal("伪造证明的 disc 节点不应注册")
	}

	// real_node_id 与 disc base 不匹配（base=victim-a 但声称 other-node）。
	discNode2 := "disc-victim-a-99999999bb"
	ack2, closeDisc2 := registerRawFrame(t, srv, testRegFrameJSON(t, discNode2,
		`"meta":{"real_node_id":"other-node","real_node_proof":"deadbeef"},"capabilities":["per-node-secret"]`))
	defer closeDisc2()
	if !strings.HasPrefix(ack2, RegisterAckErr) {
		t.Fatalf("期望 REG_ERR（real_node_id 不匹配）, got %q", ack2)
	}
	if rt.Has("disc-victim-a-99999999bb") {
		t.Fatal("real_node_id 不匹配的 disc 节点不应注册")
	}
}

// TestHubServer_RegisterAssignsVirtualIP 校验稳定节点注册后获得有效且位于虚拟子网内
// 的虚拟 IP，并写入路由表 NodeInfo。
func TestHubServer_RegisterAssignsVirtualIP(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, nil, testutil.DiscardLogger())
	m := newTestMux(t)
	defer m.Close()

	info, err := srv.registerNode(&RegisterFrame{NodeID: "node-a", AccessKey: testAK}, m)
	if err != nil {
		t.Fatalf("registerNode: %v", err)
	}
	if !info.VirtualIP.IsValid() || !info.VirtualIP.Is4() || !testVIPSubnet.Contains(info.VirtualIP) {
		t.Fatalf("VirtualIP = %v 无效或不在子网 %s 内", info.VirtualIP, testVIPSubnet)
	}
	got, ok := rt.LookupInfo("node-a")
	if !ok {
		t.Fatal("node-a 应已注册")
	}
	if got.VirtualIP != info.VirtualIP {
		t.Fatalf("路由表 VirtualIP = %v, registerNode 返回 %v", got.VirtualIP, info.VirtualIP)
	}
}

// TestHubServer_RegisterTransientNoVIP 校验瞬态节点（mesh-/p2p-/disc- 临时身份）
// 不分配虚拟 IP（防每次 mesh connect 分配/释放 VIP、vipTable 出现濒死条目）。
func TestHubServer_RegisterTransientNoVIP(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, nil, testutil.DiscardLogger())

	// disc- 临时注册需要 base 节点声明 per-node-secret 且证明持有其 secret。
	baseM := newTestMux(t)
	baseInfo, err := srv.registerNode(&RegisterFrame{
		NodeID:       "real-node",
		AccessKey:    testAK,
		Capabilities: []string{CapabilityPerNodeSecret},
	}, baseM)
	if err != nil {
		t.Fatalf("注册 base 节点: %v", err)
	}
	if baseInfo.Secret == "" {
		t.Fatal("base 节点应获得 per-node secret")
	}
	_ = baseM.Close()

	for _, id := range []string{"mesh-tmp-1234abcd5678ef90", "p2p-tmp-1234abcd5678ef90", "disc-real-node-1234abcd"} {
		m := newTestMux(t)
		frame := &RegisterFrame{NodeID: id, AccessKey: testAK}
		if strings.HasPrefix(id, "disc-") {
			frame.Meta.RealNodeID = "real-node"
			frame.Meta.RealNodeProof = discProof(baseInfo.Secret, "real-node")
		}
		info, err := srv.registerNode(frame, m)
		if err != nil {
			t.Fatalf("注册瞬态节点 %s: %v", id, err)
		}
		if info.VirtualIP.IsValid() {
			t.Fatalf("瞬态节点 %s 不应分配虚拟 IP, got %v", id, info.VirtualIP)
		}
		_ = m.Close()
	}
}

// TestHubServer_RegisterVIPInAck 校验声明 virtual-ip 能力的节点收到 REG_OK 携带本节点
// 虚拟 IP（防 Discover=false 的出口节点静默失效），且与路由表一致。
func TestHubServer_RegisterVIPInAck(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), testutil.DiscardLogger())
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
	frame := testRegFrameJSON(t, "node-vip", `"capabilities":["per-node-secret","virtual-ip"]`)
	if err := clientConn.Send(ctx, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("Receive REG_OK: %v", ackErr)
	}
	ackStruct, perr := ParseRegisterAckFull(string(ack))
	if perr != nil {
		t.Fatalf("解析 REG_OK: %v", perr)
	}
	if !ackStruct.VirtualIP.IsValid() || !testVIPSubnet.Contains(ackStruct.VirtualIP) {
		t.Fatalf("REG_OK 应携带虚拟 IP, got %+v", ackStruct)
	}
	info, ok := rt.LookupInfo("node-vip")
	if !ok {
		t.Fatal("node-vip 应已注册")
	}
	if info.VirtualIP != ackStruct.VirtualIP {
		t.Fatalf("REG_OK vip=%v 与路由表 vip=%v 不一致", ackStruct.VirtualIP, info.VirtualIP)
	}

	_ = clientConn.Close()
	select {
	case <-srvDone:
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
}

// TestHubServer_DisconnectKeepsVIP 校验连接断开（RemoveIfOwned）**不**释放虚拟 IP：
// 同 node-id 重连复用旧地址（稳定），消除"断开释放/并发重注册"重复分配竞态。
func TestHubServer_DisconnectKeepsVIP(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, nil, testutil.DiscardLogger())

	m1 := newTestMux(t)
	info1, err := srv.registerNode(&RegisterFrame{NodeID: "node-x", AccessKey: testAK}, m1)
	if err != nil {
		t.Fatalf("首次注册: %v", err)
	}
	if !info1.VirtualIP.IsValid() {
		t.Fatal("node-x 应获得虚拟 IP")
	}
	// 连接断开路径（RemoveIfOwned）：节点移除但虚拟 IP 保留。
	if !rt.RemoveIfOwned("node-x", m1) {
		t.Fatal("RemoveIfOwned 应成功")
	}
	_ = m1.Close()

	// 同 node-id 重连 → 复用旧虚拟 IP。
	m2 := newTestMux(t)
	info2, err := srv.registerNode(&RegisterFrame{NodeID: "node-x", AccessKey: testAK}, m2)
	if err != nil {
		t.Fatalf("重连注册: %v", err)
	}
	if info2.VirtualIP != info1.VirtualIP {
		t.Fatalf("重连后虚拟 IP 漂移: got %v, want %v", info2.VirtualIP, info1.VirtualIP)
	}
	_ = m2.Close()
}

// TestHubServer_RemoveReleasesVIP 校验管理端踢出（MeshRouteTable.Remove）回收虚拟 IP，
// 新节点可复用被释放的地址。
func TestHubServer_RemoveReleasesVIP(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, nil, testutil.DiscardLogger())

	m1 := newTestMux(t)
	info1, err := srv.registerNode(&RegisterFrame{NodeID: "node-x", AccessKey: testAK}, m1)
	if err != nil {
		t.Fatalf("注册 node-x: %v", err)
	}
	if !rt.Remove("node-x") {
		t.Fatal("Remove(node-x) 应成功")
	}
	_ = m1.Close()

	m2 := newTestMux(t)
	info2, err := srv.registerNode(&RegisterFrame{NodeID: "node-y", AccessKey: testAK}, m2)
	if err != nil {
		t.Fatalf("注册 node-y: %v", err)
	}
	if info2.VirtualIP != info1.VirtualIP {
		t.Fatalf("node-y 应复用被踢出 node-x 释放的 %v, got %v", info1.VirtualIP, info2.VirtualIP)
	}
	_ = m2.Close()
}

// TestMeshRouteTable_VirtualIPLookup 校验 VirtualIPOf / NodeByVirtualIP 反查与 mesh 隔离。
func TestMeshRouteTable_VirtualIPLookup(t *testing.T) {
	rt := NewMeshRouteTable()
	vipA := netip.MustParseAddr("100.64.0.2")
	vipB := netip.MustParseAddr("100.64.0.3")
	rt.Add("mesh-a", NodeInfo{ID: "node-a", VirtualIP: vipA}, nil)
	rt.Add("mesh-b", NodeInfo{ID: "node-b", VirtualIP: vipB}, nil)

	if got := rt.VirtualIPOf("node-a"); got != vipA {
		t.Fatalf("VirtualIPOf(node-a) = %v, want %v", got, vipA)
	}
	id, ok := rt.NodeByVirtualIP("mesh-a", vipA)
	if !ok || id != "node-a" {
		t.Fatalf("NodeByVirtualIP(mesh-a, %v) = %q, %v", vipA, id, ok)
	}
	// mesh 隔离：mesh-a 查不到 mesh-b 的虚拟 IP。
	if _, ok := rt.NodeByVirtualIP("mesh-a", vipB); ok {
		t.Fatal("跨 mesh 虚拟 IP 不应可反查")
	}
	// 无效地址反查失败。
	if _, ok := rt.NodeByVirtualIP("mesh-a", netip.Addr{}); ok {
		t.Fatal("无效地址不应可反查")
	}
}

// TestHubServer_CrossMeshReRegisterReleasesOldVIP（S-2）校验节点从 mesh-a 换注册到
// mesh-b 时，旧 mesh 的虚拟 IP 被释放（不再占旧 mesh 的地址空间）。
func TestHubServer_CrossMeshReRegisterReleasesOldVIP(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, nil, testutil.DiscardLogger())
	alloc := srv.Allocator().(*hubAllocator)

	const akMeshA = "sk-mesh-a-0011223344556677" // AccessKeyMesh → "mesh-a"
	const akMeshB = "sk-mesh-b-8899aabbccddeeff" // AccessKeyMesh → "mesh-b"

	mA := newTestMux(t)
	infoA, err := srv.registerNode(&RegisterFrame{NodeID: "node-x", AccessKey: akMeshA}, mA)
	if err != nil {
		t.Fatalf("注册到 mesh-a: %v", err)
	}
	if !infoA.VirtualIP.IsValid() {
		t.Fatal("mesh-a 注册应获得虚拟 IP")
	}
	if rt.MeshOf("node-x") != "mesh-a" {
		t.Fatalf("MeshOf = %q, want mesh-a", rt.MeshOf("node-x"))
	}
	_ = mA.Close()

	// 换 mesh-b 注册同一 node-id（Add 触发跨 mesh 移动：移除旧表 + 释放旧 VIP）。
	mB := newTestMux(t)
	infoB, err := srv.registerNode(&RegisterFrame{NodeID: "node-x", AccessKey: akMeshB}, mB)
	if err != nil {
		t.Fatalf("注册到 mesh-b: %v", err)
	}
	if !infoB.VirtualIP.IsValid() {
		t.Fatal("mesh-b 注册应获得虚拟 IP")
	}
	if rt.MeshOf("node-x") != "mesh-b" {
		t.Fatalf("MeshOf = %q, want mesh-b", rt.MeshOf("node-x"))
	}
	// 旧 mesh-a 的 VIP 应已释放（assigned[mesh-a\0node-x] 不存在）。
	alloc.mu.Lock()
	_, stillHeld := alloc.assigned[allocKey("mesh-a", "node-x")]
	alloc.mu.Unlock()
	if stillHeld {
		t.Fatal("跨 mesh 重注册后旧 mesh-a 的虚拟 IP 应已释放")
	}
	_ = mB.Close()
}

// TestHubServer_RemoveReRegister_NoDupVIP（I-1 回归）并发执行 Remove（管理端踢出）
// 与 registerNode（重注册），最终分配器的 assigned 表不得出现两个 key 共享同一 VIP。
func TestHubServer_RemoveReRegister_NoDupVIP(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, nil, testutil.DiscardLogger())
	alloc := srv.Allocator().(*hubAllocator)

	var wg sync.WaitGroup
	for range 60 {
		wg.Go(func() {
			m := newTestMux(t)
			defer m.Close()
			_, _ = srv.registerNode(&RegisterFrame{NodeID: "node-x", AccessKey: testAK}, m)
			_ = rt.Remove("node-x")
		})
	}
	wg.Wait()

	// 最终分配表不变量：同一 VIP 不得被两个不同 key 共享。
	alloc.mu.Lock()
	defer alloc.mu.Unlock()
	owner := make(map[netip.Addr]string, len(alloc.assigned))
	for k, vip := range alloc.assigned {
		if prev, dup := owner[vip]; dup && prev != k {
			t.Fatalf("虚拟 IP %v 被 %s 与 %s 共享（重复分配竞态）", vip, prev, k)
		}
		owner[vip] = k
	}
}

// TestHubServer_RegisterStableMeshPrefixedNodeGetsVIP（S-4 回归）校验以 mesh- 开头的
// 稳定节点名（如主机名不可解析时 mesh node 回落字面量 "mesh-node"）仍能获得虚拟 IP，
// 不被瞬态前缀过滤误伤（静默失效）。
func TestHubServer_RegisterStableMeshPrefixedNodeGetsVIP(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, nil, testutil.DiscardLogger())
	m := newTestMux(t)
	defer m.Close()

	info, err := srv.registerNode(&RegisterFrame{NodeID: "mesh-node", AccessKey: testAK}, m)
	if err != nil {
		t.Fatalf("registerNode: %v", err)
	}
	if !info.VirtualIP.IsValid() {
		t.Fatal("稳定节点名 mesh-node 应获得虚拟 IP（非瞬态身份）")
	}
}

// TestHubServer_RegisterVIPInAck_NoSecret 校验仅声明 virtual-ip 能力（不声明
// per-node-secret）的节点，REG_OK 携带虚拟 IP 且格式为 "REG_OK::<vip>"（secret 空
// 占位），向后兼容解析正确。
func TestHubServer_RegisterVIPInAck_NoSecret(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, NewAuthenticator(testRing()), testutil.DiscardLogger())
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
	frame := testRegFrameJSON(t, "node-vip-nosec", `"capabilities":["virtual-ip"]`)
	if err := clientConn.Send(ctx, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("Receive REG_OK: %v", ackErr)
	}
	if !strings.HasPrefix(string(ack), RegisterAckOK+"::") {
		t.Fatalf("仅声明 virtual-ip 的节点应收到 REG_OK::<vip> 格式, got %q", string(ack))
	}
	ackFull, perr := ParseRegisterAckFull(string(ack))
	if perr != nil {
		t.Fatalf("解析 REG_OK: %v", perr)
	}
	if ackFull.Secret != "" {
		t.Fatalf("未声明 per-node-secret 不应有 secret, got %q", ackFull.Secret)
	}
	if !ackFull.VirtualIP.IsValid() || !testVIPSubnet.Contains(ackFull.VirtualIP) {
		t.Fatalf("REG_OK 应携带有效虚拟 IP, got %+v", ackFull)
	}

	_ = clientConn.Close()
	select {
	case <-srvDone:
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
}

// TestHubServer_RemoveReRegister_NoDupVIPInRouteTable（整体审查发现 2 回归）：
// 并发 Remove + registerNode 后，**路由表内**不得出现两个节点共享同一虚拟 IP
// （分配器侧由 TestHubServer_RemoveReRegister_NoDupVIP 锁定，本测试补路由表侧不变量）。
func TestHubServer_RemoveReRegister_NoDupVIPInRouteTable(t *testing.T) {
	rt := NewMeshRouteTable()
	srv := NewHubServer(rt, nil, testutil.DiscardLogger())

	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			m := newTestMux(t)
			defer m.Close()
			_, _ = srv.registerNode(&RegisterFrame{NodeID: "node-x", AccessKey: testAK}, m)
			_ = rt.Remove("node-x")
		})
	}
	wg.Wait()

	// 路由表侧唯一性不变量：同 mesh 内两节点不得共享同一 VIP。
	seen := make(map[netip.Addr]string)
	for _, mesh := range rt.AllMeshes() {
		for _, n := range rt.List(mesh) {
			if !n.VirtualIP.IsValid() {
				continue
			}
			if prev, dup := seen[n.VirtualIP]; dup {
				t.Fatalf("路由表两节点共享虚拟 IP %v: %s vs %s（重复分配竞态）", n.VirtualIP, prev, n.ID)
			}
			seen[n.VirtualIP] = string(n.ID)
		}
	}
}
