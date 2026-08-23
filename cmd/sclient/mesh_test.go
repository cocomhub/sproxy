// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
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
	for _, name := range []string{"hub", "node-id", "token", "relay-token", "service", "dial-allow", "dial-allow-cidr", "local", "webrtc", "stun"} {
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
	for _, name := range []string{"listen", "webrtc", "hub", "token", "relay-token", "node-id"} {
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
