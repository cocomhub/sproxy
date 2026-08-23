// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
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
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
	"github.com/spf13/cobra"
)

func TestNewCmdMesh_Subcommands(t *testing.T) {
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
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

func TestNewCmdMeshConnect_ArgsAndFlags(t *testing.T) {
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
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

// TestMeshSignalToken 验证信令 token 选择：显式 --token 优先，否则复用 FileClient 的
// auth token（--auth-token / 配置 auth_token）。
func TestMeshSignalToken(t *testing.T) {
	svc := client.NewFileClient("http://127.0.0.1:1", client.WithAuthToken("cfg-token"))

	// 显式 --token 优先
	if got := meshSignalToken("flag-token", svc); got != "flag-token" {
		t.Fatalf("meshSignalToken(flag) = %q, want flag-token", got)
	}
	// 空 flag → 回落 FileClient auth token
	if got := meshSignalToken("", svc); got != "cfg-token" {
		t.Fatalf("meshSignalToken(empty) = %q, want cfg-token", got)
	}
	// 两者皆空 → 空串
	plain := client.NewFileClient("http://127.0.0.1:1")
	if got := meshSignalToken("", plain); got != "" {
		t.Fatalf("meshSignalToken(both empty) = %q, want empty", got)
	}
}

// TestMeshRelayToken 验证自动注册 token 选择（I37 子决策 A fallback 链）：
// --relay-token → --token → auth_token。
func TestMeshRelayToken(t *testing.T) {
	svc := client.NewFileClient("http://127.0.0.1:1", client.WithAuthToken("cfg-token"))

	// 显式 --relay-token 优先
	if got := meshRelayToken("relay-token", "flag-token", svc); got != "relay-token" {
		t.Fatalf("meshRelayToken(relay) = %q, want relay-token", got)
	}
	// 空 relay → 回落信令 token（--token）
	if got := meshRelayToken("", "flag-token", svc); got != "flag-token" {
		t.Fatalf("meshRelayToken(empty relay) = %q, want flag-token", got)
	}
	// 空 relay + 空 token → 回落 auth_token
	if got := meshRelayToken("", "", svc); got != "cfg-token" {
		t.Fatalf("meshRelayToken(both empty) = %q, want cfg-token", got)
	}
	// 全空 → 空串
	plain := client.NewFileClient("http://127.0.0.1:1")
	if got := meshRelayToken("", "", plain); got != "" {
		t.Fatalf("meshRelayToken(all empty) = %q, want empty", got)
	}
}

// TestNormalizeHubEndpoints 验证 hub scheme 归一（I40）：
// ws(s):// → http(s):// 信令基址 + ws(s)://host/ws 注册端点。
func TestNormalizeHubEndpoints(t *testing.T) {
	tests := []struct {
		name         string
		hubURL       string
		serverURL    string
		wantHTTPBase string
		wantWSURL    string
		wantErr      bool
	}{
		{"http", "http://hub:18083", "", "http://hub:18083", "ws://hub:18083/ws", false},
		{"https", "https://hub:18083", "", "https://hub:18083", "wss://hub:18083/ws", false},
		{"ws", "ws://hub:18084/ws", "", "http://hub:18084", "ws://hub:18084/ws", false},
		{"wss", "wss://hub:18084/ws", "", "https://hub:18084", "wss://hub:18084/ws", false},
		{"http with path", "http://hub:18083/ws", "", "http://hub:18083", "ws://hub:18083/ws", false},
		{"empty fallback", "", "http://fallback:18083", "http://fallback:18083", "ws://fallback:18083/ws", false},
		{"both empty", "", "", "", "", true},
		{"bad scheme", "ftp://hub", "", "", "", true},
		{"malformed url", "://bad", "", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			httpBase, wsURL, err := normalizeHubEndpoints(tc.hubURL, tc.serverURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got httpBase=%q wsURL=%q", httpBase, wsURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if httpBase != tc.wantHTTPBase || wsURL != tc.wantWSURL {
				t.Fatalf("normalizeHubEndpoints(%q, %q) = (%q, %q), want (%q, %q)",
					tc.hubURL, tc.serverURL, httpBase, wsURL, tc.wantHTTPBase, tc.wantWSURL)
			}
		})
	}
}

// TestDefaultMeshDial_FallsBackToRelay 验证选路：webrtc 打洞失败（目标无 p2p listen
// 或不可达）时回落 hub 中继。
func TestDefaultMeshDial_FallsBackToRelay(t *testing.T) {
	// I69：host-only 候选（零 STUN 依赖），gathering 秒级完成，CI 沙箱稳定。
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })

	// 模拟 hub：/api/relay/stream 返回 502（中继也失败）→ 最终返回错误
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/relay/stream" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	target := &client.MeshService{Name: "svc", Node: "node-a", Addr: "127.0.0.1:22"}
	// signaler 指向不可达 hub（webrtc 打洞必然失败，验证回落）
	signaler := hub.NewHubSignaler("http://127.0.0.1:1", "", "local-node")

	_, err := defaultMeshDial(context.Background(), svc, signaler, target, "local-node")
	if err == nil {
		t.Fatal("expected error: webrtc 打洞失败且中继失败应报错")
	}
	// 错误应来自中继（webrtc 已回落），而非 webrtc 本身
	if !strings.Contains(err.Error(), "502") && !strings.Contains(err.Error(), "hub") {
		t.Fatalf("expected relay fallback error, got: %v", err)
	}
}

// TestMeshWebRTCStream_WritesDialFrameOnMuxStream 回归测试（P0-1）：
// mesh webrtc 直连路径必须在 mux 流上写拨号帧，而不是把帧以裸字节写在
// DataChannel 上。
//
// 曾实测发现的 bug：defaultMeshDial 返回裸 *webrtc.Conn，meshForwardListen 直接把
// [4B len][{"dial":addr}] 以两次独立 Write（两条 SCTP 消息）写在 DataChannel 上；
// 对端 p2p listen 用 mux.New(webrtc.ConnAsXfer) 按帧消费 → frame length mismatch →
// 拆会话，直连数据面 100% 失败（且拨号"已成功"不回落中继，纯坏路径）。
// 本测试用进程内 webrtc 对复现对端消费方式，断言 meshWebRTCStream 返回的流上
// 对端能读到正确拨号帧。
func TestMeshWebRTCStream_WritesDialFrameOnMuxStream(t *testing.T) {
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })

	signal := webrtc.NewSignal()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 对端：p2p listen 等价物——mux RoleListener 消费帧，从流读拨号帧。
	frameCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := webrtc.Listen(signal)
		if err != nil {
			errCh <- err
			return
		}
		m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
		defer m.Close()
		stream, err := m.Accept(ctx)
		if err != nil {
			errCh <- err
			return
		}
		defer stream.Close()
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(stream, lenBuf); err != nil {
			errCh <- err
			return
		}
		meta := make([]byte, binary.BigEndian.Uint32(lenBuf))
		if _, err := io.ReadFull(stream, meta); err != nil {
			errCh <- err
			return
		}
		frameCh <- string(meta)
	}()

	// 本侧：mesh connect 等价物——meshWebRTCStream 开 mux 流并写拨号帧。
	conn, err := webrtc.Dial(signal)
	if err != nil {
		t.Fatalf("dial webrtc: %v", err)
	}
	defer conn.Close()
	res, err := meshWebRTCStream(ctx, conn, "127.0.0.1:22")
	if err != nil {
		t.Fatalf("meshWebRTCStream: %v", err)
	}
	if res.kind != "webrtc" {
		t.Fatalf("kind = %q, want webrtc", res.kind)
	}
	if _, ok := res.conn.(*muxStreamConn); !ok {
		t.Fatalf("webrtc 路径应返回 muxStreamConn（net.Conn 适配），got %T", res.conn)
	}

	select {
	case f := <-frameCh:
		var d hub.DialRequest
		if err := json.Unmarshal([]byte(f), &d); err != nil || d.Dial != "127.0.0.1:22" {
			t.Fatalf("对端收到的拨号帧 = %q, want {\"dial\":\"127.0.0.1:22\"}", f)
		}
	case err := <-errCh:
		t.Fatalf("对端读取拨号帧失败: %v", err)
	case <-ctx.Done():
		t.Fatal("等待对端读取拨号帧超时")
	}
}

// ---- meshTargetRefresher 单元测试 ----

// servicesHandler 构造一个可切换响应的 /api/hub/services mock。
func servicesHandler(hits *atomic.Int32, get func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(get()))
	}
}

func fixedClock() (time.Time, func() time.Time) {
	t := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	return t, func() time.Time { return t }
}

// TestMeshTargetRefresher_FreshHitAndConcurrentCache 验证：首次 resolve 打一次 HTTP；
// TTL 内并发 resolve 全部走缓存（单飞 + 缓存命中），不再打 HTTP（-race 验证并发安全）。
func TestMeshTargetRefresher_FreshHitAndConcurrentCache(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		return `[{"name":"svc","node":"node-a","addr":"127.0.0.1:10022"}]`
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	r := newMeshTargetRefresher(svc, "svc")
	r.ttl = time.Hour
	_, r.now = fixedClock()

	target, err := r.resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if target.Node != "node-a" || target.Addr != "127.0.0.1:10022" {
		t.Fatalf("unexpected target: %+v", target)
	}

	const n = 10
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.resolve(context.Background())
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("resolve[%d] error: %v", i, e)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1 (cache hit after first resolve)", got)
	}
}

// TestMeshTargetRefresher_SingleFlightDuringRefresh 验证单飞：刷新在途时，
// 并发 resolve 共享同一次刷新（不再打 HTTP），全部拿到结果。
func TestMeshTargetRefresher_SingleFlightDuringRefresh(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"svc","node":"node-a","addr":"127.0.0.1:10022"}]`))
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	r := newMeshTargetRefresher(svc, "svc")
	r.ttl = time.Hour
	_, r.now = fixedClock()

	// 第一个 resolve 承担刷新（阻塞在 handler）
	firstDone := make(chan error, 1)
	go func() {
		_, err := r.resolve(context.Background())
		firstDone <- err
	}()

	// 等 handler 被命中（刷新已在途）
	deadline := time.Now().Add(3 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("refresh did not reach handler")
	}

	// 刷新在途时并发等待者：应等待同一刷新，不再打 HTTP
	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.resolve(context.Background())
		}(i)
	}
	// 给等待者一点时间进入 resolve（命中与否均不影响断言，仅提高单飞分支覆盖）
	time.Sleep(50 * time.Millisecond)

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("waiter[%d] error: %v", i, e)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1 (single-flight)", got)
	}
}

// TestMeshTargetRefresher_TTLExpiry 验证：TTL 过期后重新拉取，服务消失时报「不可用」。
func TestMeshTargetRefresher_TTLExpiry(t *testing.T) {
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
	r := newMeshTargetRefresher(svc, "svc")
	r.ttl = 3 * time.Second
	cur := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return cur }

	if _, err := r.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 推进时钟超过 TTL，且服务列表改为空 → resolve 重新拉取并报不可用
	cur = cur.Add(4 * time.Second)
	mu.Lock()
	svcList = `[]`
	mu.Unlock()
	if _, err := r.resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "不可用") {
		t.Fatalf("expected unavailable after TTL expiry, got: %v", err)
	}
}

// TestMeshTargetRefresher_ServiceAbsent 验证服务不在列表时报「不可用」。
func TestMeshTargetRefresher_ServiceAbsent(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string { return `[]` }))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	r := newMeshTargetRefresher(svc, "missing")
	if _, err := r.resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "不可用") {
		t.Fatalf("expected unavailable, got: %v", err)
	}
}

// TestMeshTargetRefresher_FetchError 验证 hub 查询失败时返回明确错误。
func TestMeshTargetRefresher_FetchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	r := newMeshTargetRefresher(svc, "svc")
	if _, err := r.resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "查询 mesh 服务失败") {
		t.Fatalf("expected fetch error, got: %v", err)
	}
}

// TestMeshTargetRefresher_InvalidateRefetch 验证 invalidate 后立即重取，TTL 内命中缓存。
func TestMeshTargetRefresher_InvalidateRefetch(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		return `[{"name":"svc","node":"node-a","addr":"127.0.0.1:10022"}]`
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	r := newMeshTargetRefresher(svc, "svc")
	r.ttl = time.Hour
	_, r.now = fixedClock()

	if _, err := r.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.invalidate("node-a")
	if _, err := r.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits = %d, want 2 (invalidate forces refetch, then cache hit)", got)
	}
}

// TestMeshTargetRefresher_FailoverSkipsDeadNode（P1-13 回归）：
// 同名服务多候选时，拨号失败节点被 invalidate 记录后，下一次 resolve 应优先选择
// 其他健康候选——旧实现恒取 node-ID 字典序首个，死节点永久遮蔽健康副本。
func TestMeshTargetRefresher_FailoverSkipsDeadNode(t *testing.T) {
	// 多候选：node-a（字典序首，模拟死节点）与 node-b（健康）。
	ts := httptest.NewServer(servicesHandler(&atomic.Int32{}, func() string {
		return `[{"name":"svc","node":"node-a","addr":"10.0.0.1:22"},{"name":"svc","node":"node-b","addr":"10.0.0.2:22"}]`
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	r := newMeshTargetRefresher(svc, "svc")
	r.ttl = time.Hour
	_, r.now = fixedClock()

	// 首次 resolve：node-a（字典序首）。
	t1, err := r.resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if t1.Node != "node-a" {
		t.Fatalf("首次 resolve 应取 node-a，got %q", t1.Node)
	}

	// node-a 拨号失败 → invalidate(node-a)。
	r.invalidate(t1.Node)

	// 下一次 resolve 应跳过 node-a 选 node-b（P1-13 核心断言）。
	t2, err := r.resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if t2.Node != "node-b" {
		t.Fatalf("invalidate(node-a) 后应跳过死节点选 node-b，got %q", t2.Node)
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
	r := newMeshTargetRefresher(svc, "svc")
	r.ttl = time.Hour
	_, r.now = fixedClock()

	// 注入 dial：记录收到的 target，返回错误触发 invalidate 路径（避免 pump 阻塞）
	targets := make(chan *client.MeshService, 4)
	dial := func(_ context.Context, _ *client.FileClient, _ *hub.HubSignaler, target *client.MeshService, _ string) (*meshDialResult, error) {
		targets <- target
		return nil, fmt.Errorf("injected dial error")
	}

	errBuf := &lockedBuffer{}
	ios := cli.IOStreams{Out: io.Discard, ErrOut: errBuf}

	initial, err := r.resolve(context.Background())
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
	r.invalidate("node-a")

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

// TestMeshAutoRegister_GetsSecretAndCleanup 验证自动注册（I37）：
//   - mesh connect 连接前自持临时注册连接，从 hub 拿到 per-node secret；
//   - HubSignaler 信令请求携带 X-Node-Secret / X-Node-ID（B2/B3 身份校验前置）；
//   - closer 确定性关闭注册连接，hub 移除临时节点（防 WS 泄漏）。
func TestMeshAutoRegister_GetsSecretAndCleanup(t *testing.T) {
	rt := hub.NewRouteTable()
	srv := hub.NewHubServer(rt, hub.NewAuthenticator("relay-token"), discardLogger())

	muxHTTP := http.NewServeMux()
	wsNode := ws.NewHandlerNode()
	wsNode.AddToMux(muxHTTP, "/ws")
	// 信令端点：记录 X-Node-Secret 头（验证 B2 携带 secret）。
	var mu sync.Mutex
	var gotSecret, gotNodeID string
	muxHTTP.HandleFunc("/api/signal/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotSecret = r.Header.Get("X-Node-Secret")
		gotNodeID = r.Header.Get("X-Node-ID")
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	ts := httptest.NewServer(muxHTTP)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// hub 侧：接受 ws 连接并交给 HandleConn 注册。
	go func() {
		for {
			c, aerr := wsNode.Accept(ctx)
			if aerr != nil {
				return
			}
			go func(cc xfer.Conn) {
				_ = srv.HandleConn(ctx, cc)
			}(c)
		}
	}()

	svc := client.NewFileClient(ts.URL)
	reg, err := meshAutoRegister(ctx, svc, ts.URL, "relay-token", "signal-token", "node-a")
	if err != nil {
		t.Fatal(err)
	}
	// 显式断言 closer 后节点移除；中途失败用 Cleanup 兜底（mux.Close 幂等）。
	t.Cleanup(func() { _ = reg.closer() })

	// 断言：临时 node_id 唯一前缀 + 节点已注册且拿到非空 secret。
	if !strings.HasPrefix(reg.tempNode, "mesh-node-a-") {
		t.Fatalf("temp node %q should start with mesh-node-a-", reg.tempNode)
	}
	info, ok := rt.LookupInfo(hub.NodeID(reg.tempNode))
	if !ok {
		t.Fatalf("temp node %q not registered", reg.tempNode)
	}
	if info.Secret == "" {
		t.Fatal("per-node secret should not be empty")
	}

	// 断言信令请求携带 X-Node-Secret（B2）+ X-Node-ID（I37 核心：不再 403 秒败）。
	if err := reg.signaler.SendOffer("peer-node", "sdp"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	secret, nodeID := gotSecret, gotNodeID
	mu.Unlock()
	if secret != info.Secret {
		t.Fatalf("X-Node-Secret = %q, want %q", secret, info.Secret)
	}
	if nodeID != reg.tempNode {
		t.Fatalf("X-Node-ID = %q, want %q", nodeID, reg.tempNode)
	}

	// 断言 closer 后 hub 移除临时节点（确定性关闭防 WS 泄漏）。
	if err := reg.closer(); err != nil {
		t.Fatal(err)
	}
	removeDeadline := time.Now().Add(3 * time.Second)
	for rt.Has(hub.NodeID(reg.tempNode)) && time.Now().Before(removeDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rt.Has(hub.NodeID(reg.tempNode)) {
		t.Fatal("temp node should be removed after closer")
	}
}

// waitNodeRemoved 轮询等待节点被 hub 移除（closer 后 RemoveIfOwned 异步生效）。
func waitNodeRemoved(rt *hub.RouteTable, node string) error {
	deadline := time.Now().Add(3 * time.Second)
	for rt.Has(hub.NodeID(node)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rt.Has(hub.NodeID(node)) {
		return fmt.Errorf("node %q should be removed after closer", node)
	}
	return nil
}

// TestAutoRegister_TempAndExact 验证 autoRegister（B17）的两种 node 生成模式：
//   - temp（exactNode=false，p2p connect / mesh connect）：临时 node_id = <prefix>-<base>-<nano>，
//     唯一且防踢长驻 relay 注册；对端无需预知本端 ID（Answer 回给 offerFrom）。
//   - exact（exactNode=true，p2p listen）：注册成 nodeID 原样，供 p2p connect --peer <id> 寻址。
//
// 两种模式都声明 per-node-secret 能力、拿到 secret，signaler 信令携带
// X-Node-Secret / X-Node-ID（B3 服务端身份校验前置）。
func TestAutoRegister_TempAndExact(t *testing.T) {
	rt := hub.NewRouteTable()
	srv := hub.NewHubServer(rt, hub.NewAuthenticator("relay-token"), discardLogger())

	muxHTTP := http.NewServeMux()
	wsNode := ws.NewHandlerNode()
	wsNode.AddToMux(muxHTTP, "/ws")
	// 信令端点：记录 X-Node-Secret 头（验证携带 secret）。
	var mu sync.Mutex
	var gotSecret, gotNodeID string
	muxHTTP.HandleFunc("/api/signal/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotSecret = r.Header.Get("X-Node-Secret")
		gotNodeID = r.Header.Get("X-Node-ID")
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	ts := httptest.NewServer(muxHTTP)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// hub 侧：接受 ws 连接并交给 HandleConn 注册。
	go func() {
		for {
			c, aerr := wsNode.Accept(ctx)
			if aerr != nil {
				return
			}
			go func(cc xfer.Conn) {
				_ = srv.HandleConn(ctx, cc)
			}(c)
		}
	}()

	// temp 模式：prefix=p2p，exactNode=false → 临时 node_id（唯一前缀 + 非原始 ID）。
	regTemp, err := autoRegister(ctx, autoRegisterParams{
		hubURL: ts.URL, relayToken: "relay-token", signalToken: "signal-token",
		nodeID: "node-a", prefix: "p2p", exactNode: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = regTemp.closer() })
	if !strings.HasPrefix(regTemp.tempNode, "p2p-node-a-") {
		t.Fatalf("temp node %q should start with p2p-node-a-", regTemp.tempNode)
	}
	if regTemp.tempNode == "node-a" {
		t.Fatal("temp 模式不应注册成原始 node_id")
	}
	infoTemp, ok := rt.LookupInfo(hub.NodeID(regTemp.tempNode))
	if !ok {
		t.Fatalf("temp node %q not registered", regTemp.tempNode)
	}
	if infoTemp.Secret == "" {
		t.Fatal("temp 模式 per-node secret should not be empty")
	}
	// 信令携带 X-Node-Secret / X-Node-ID（I37 核心：不再 403 秒败）。
	if err := regTemp.signaler.SendOffer("peer-node", "sdp"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	secret, nodeID := gotSecret, gotNodeID
	mu.Unlock()
	if secret != infoTemp.Secret {
		t.Fatalf("X-Node-Secret = %q, want %q", secret, infoTemp.Secret)
	}
	if nodeID != regTemp.tempNode {
		t.Fatalf("X-Node-ID = %q, want %q", nodeID, regTemp.tempNode)
	}
	if err := regTemp.closer(); err != nil {
		t.Fatal(err)
	}
	if err := waitNodeRemoved(rt, regTemp.tempNode); err != nil {
		t.Fatal(err)
	}

	// exact 模式：nodeID 原样注册（p2p listen 被寻址方）。
	regExact, err := autoRegister(ctx, autoRegisterParams{
		hubURL: ts.URL, relayToken: "relay-token", signalToken: "signal-token",
		nodeID: "node-b", prefix: "p2p", exactNode: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = regExact.closer() })
	if regExact.tempNode != "node-b" {
		t.Fatalf("exact node %q should be registered as-is, got %q", "node-b", regExact.tempNode)
	}
	infoExact, ok := rt.LookupInfo(hub.NodeID("node-b"))
	if !ok {
		t.Fatal("exact node-b not registered")
	}
	if infoExact.Secret == "" {
		t.Fatal("exact 模式 per-node secret should not be empty")
	}
	if err := regExact.closer(); err != nil {
		t.Fatal(err)
	}
	if err := waitNodeRemoved(rt, "node-b"); err != nil {
		t.Fatal(err)
	}
}
