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
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestLeaf_DetectE2E_E2EDialFrameDecrypts 验证 e2e dial 帧（DialRequest.E2E=true）
// 在 leaf.go dOK 分支走端到端解密路径：E2EServe 注入时，出口拨号后先解密再 pump
// 到目标服务（echo 回显），客户端明文往返一致。
//
// E2EServe 用「透传 + 前缀标记」模拟解密（relay 包测试不能 import mesh——包级环）：
// 闭包返回的「解密流」在 Read 时把密文帧还原为明文（此处简化：直接透传，验证
// dOK 分支确实调用了 E2EServe 且其返回流被 pump）。
func TestLeaf_DetectE2E_E2EDialFrameDecrypts(t *testing.T) {
	t.Parallel()
	echoAddr := startEchoServer(t)

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 标记 E2EServe 被调用（可观测断言）。
	called := make(chan struct{}, 1)
	// 模拟解密：把密文流（conn）包一层「解密流」——直接透传（简化），记录被调用。
	e2eServe := func(_ context.Context, conn io.ReadWriteCloser, _ *tunnel.Identity, _ []string, _ []byte) (net.Conn, error) {
		select {
		case called <- struct{}{}:
		default:
		}
		// 透传流：把 io.ReadWriteCloser 包成 net.Conn（模拟 ServeE2EStream 返回的解密流）。
		return &passthruConn{ReadWriteCloser: conn}, nil
	}

	policy := func(addr string) (string, bool) { return addr, true }
	go func() {
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testLogger(),
			ServeOptions{DialPolicy: policy, E2EServe: e2eServe})
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	// 写 e2e dial 帧（E2E=true）。
	dialMeta, _ := json.Marshal(hub.DialRequest{Dial: echoAddr, E2E: true})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(dialMeta)))
	if _, werr := clientStream.Write(lenBuf); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := clientStream.Write(dialMeta); werr != nil {
		t.Fatal(werr)
	}

	// 写数据 + 半关闭 → 应经 E2EServe 解密流 pump 到 echo 回显。
	payload := []byte("e2e-payload-through-tunnel")
	if _, werr := clientStream.Write(payload); werr != nil {
		t.Fatal(werr)
	}
	_ = clientStream.CloseWrite()

	// 断言 E2EServe 被调用（e2e 路径生效）。
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("E2EServe 未被调用（e2e dial 帧未走端到端解密路径）")
	}

	// 读回显。
	got := make([]byte, len(payload))
	if err := readFullWithTimeout(ctx, clientStream, got, "回显数据"); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("expected echo %q, got %q", payload, got)
	}
}

// TestLeaf_DetectE2E_LegacyFrameCompat 验证旧 dial 帧（无 E2E 标记）走现有裸 pump
// 路径（兼容，不调用 E2EServe）。
func TestLeaf_DetectE2E_LegacyFrameCompat(t *testing.T) {
	t.Parallel()
	echoAddr := startEchoServer(t)

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// E2EServe 不应被调用（旧帧走裸 pump）。
	var called atomic.Bool
	e2eServe := func(_ context.Context, conn io.ReadWriteCloser, _ *tunnel.Identity, _ []string, _ []byte) (net.Conn, error) {
		called.Store(true)
		return &passthruConn{ReadWriteCloser: conn}, nil
	}

	policy := func(addr string) (string, bool) { return addr, true }
	go func() {
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testLogger(),
			ServeOptions{DialPolicy: policy, E2EServe: e2eServe})
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	// 普通 dial 帧（无 E2E）。
	dialMeta, _ := json.Marshal(hub.DialRequest{Dial: echoAddr})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(dialMeta)))
	if _, werr := clientStream.Write(lenBuf); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := clientStream.Write(dialMeta); werr != nil {
		t.Fatal(werr)
	}

	payload := []byte("legacy-payload")
	if _, werr := clientStream.Write(payload); werr != nil {
		t.Fatal(werr)
	}
	_ = clientStream.CloseWrite()

	// 读回显（旧帧应正常走裸 pump）。
	got := make([]byte, len(payload))
	if err := readFullWithTimeout(ctx, clientStream, got, "回显数据"); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("expected echo %q, got %q", payload, got)
	}
	if called.Load() {
		t.Fatal("旧 dial 帧不应调用 E2EServe")
	}
}

// TestLeaf_DetectE2E_NoE2EServeFailsClosed 验证 e2e 帧但 E2EServe 未注入时
// fail-closed：日志告警 + 流被关闭（拒绝明文处理，禁静默降级）。
func TestLeaf_DetectE2E_NoE2EServeFailsClosed(t *testing.T) {
	t.Parallel()
	echoAddr := startEchoServer(t)

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	policy := func(addr string) (string, bool) { return addr, true }
	go func() {
		// 不注入 E2EServe。
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testLogger(),
			ServeOptions{DialPolicy: policy})
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	// 写 e2e dial 帧。
	dialMeta, _ := json.Marshal(hub.DialRequest{Dial: echoAddr, E2E: true})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(dialMeta)))
	if _, werr := clientStream.Write(lenBuf); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := clientStream.Write(dialMeta); werr != nil {
		t.Fatal(werr)
	}

	// fail-closed：流被关闭（拒绝明文处理）——读侧应很快 EOF。
	expectStreamClosed(t, clientStream, "e2e 帧但未装配 E2EServe", 5*time.Second)
}

// passthruConn 把 io.ReadWriteCloser 适配为 net.Conn（透传读写）。
type passthruConn struct {
	io.ReadWriteCloser
}

func (passthruConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (passthruConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (passthruConn) SetDeadline(_ time.Time) error      { return nil }
func (passthruConn) SetReadDeadline(_ time.Time) error  { return nil }
func (passthruConn) SetWriteDeadline(_ time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "e2e-test" }
func (dummyAddr) String() string  { return "e2e-test" }
