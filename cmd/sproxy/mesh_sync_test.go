// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_sync_test.go 钉住 A 侧 mesh 载体（Y 二期 P3-d）**装配工厂**的行为：
//   - fail-closed 前置校验（缺 node/volume/pins/身份、未知 transport）；
//   - 只读面与写面**两条中继**分别用 volread / volwrite 服务名接线；
//   - 返回的 sync.FS 真能跑（假 B 端跑真隧道握手，断言请求真落到 read/write 路由上）。

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// meshRelayFake 是**单个服务名**上的中继替身：MeshServices 宣告自己托管该服务，
// RelayStream 返回「内存管道 + 真隧道服务端（假 B 端 handler）」，并记录本服务名收到的请求。
type meshRelayFake struct {
	node    string
	service string
	bID     *tunnel.Identity
	aFP     string
	handler http.Handler

	mu   sync.Mutex
	seen []string // "METHOD PATH"
}

func (f *meshRelayFake) MeshServices(context.Context) ([]client.MeshService, error) {
	return []client.MeshService{{Node: f.node, Name: f.service, Addr: "pipe"}}, nil
}

func (f *meshRelayFake) RelayStream(ctx context.Context, _, _ string) (net.Conn, error) {
	clientSide, serverSide := net.Pipe()
	go func() {
		m := mux.New(builtin.FromNetConn(serverSide), mux.RoleListener)
		defer func() { _ = m.Close() }()
		tun := tunnel.NewTunnel(m, tunnel.DeriveRemoteStaticKey(f.bID.Fingerprint()),
			tunnel.WithIdentity(f.bID), tunnel.WithPeerFingerprints([]string{f.aFP}))
		_ = tun.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.seen = append(f.seen, r.Method+" "+r.URL.Path)
			f.mu.Unlock()
			f.handler.ServeHTTP(w, r)
		}))
		_ = serverSide.Close()
	}()
	return clientSide, nil
}

func (f *meshRelayFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.seen))
	copy(out, f.seen)
	return out
}

// fakeBHandler 是假 B 端：读/写路由各一条，够断言「请求落到哪一面」。
func fakeBHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /remote/list", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[]}`))
	})
	mux.HandleFunc("POST /remote/mkdir", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	return mux
}

// meshFactoryFixture 装配工厂与两个按服务名缓存的替身。
type meshFactoryFixture struct {
	factory syncexec.MeshFSFactory
	aID     *tunnel.Identity
	node    string
	byName  map[string]*meshRelayFake
	pin     string
}

func newMeshFactoryFixture(t *testing.T) *meshFactoryFixture {
	t.Helper()
	aID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	bID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	fx := &meshFactoryFixture{
		aID:    aID,
		node:   "nodeB",
		byName: map[string]*meshRelayFake{},
		pin:    bID.Fingerprint(),
	}
	fx.factory = newMeshFSFactory(func(service string) remote.RelayClient {
		f := &meshRelayFake{
			node: fx.node, service: service, bID: bID,
			aFP: aID.Fingerprint(), handler: fakeBHandler(),
		}
		fx.byName[service] = f
		return f
	}, aID, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return fx
}

func TestMeshFSFactory_FailClosedPreconditions(t *testing.T) {
	fx := newMeshFactoryFixture(t)
	pin := "sha256:" + strings.Repeat("a", 64)

	cases := []struct {
		name string
		rc   syncmgr.RemoteConfig
		want string
	}{
		{"缺 node", syncmgr.RemoteConfig{Volume: "main", PeerPins: []string{pin}}, "node"},
		{"缺 volume", syncmgr.RemoteConfig{Node: "nodeB", PeerPins: []string{pin}}, "volume"},
		{"缺 peer_pins", syncmgr.RemoteConfig{Node: "nodeB", Volume: "main"}, "peer_pins"},
		{"未知 transport", syncmgr.RemoteConfig{Node: "nodeB", Volume: "main", PeerPins: []string{pin}, Transport: "quic"}, "transport"},
		{"webrtc 在本装配不受支持", syncmgr.RemoteConfig{Node: "nodeB", Volume: "main", PeerPins: []string{pin}, Transport: "webrtc"}, "webrtc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, closeFn, err := fx.factory(context.Background(), tc.rc)
			if err == nil {
				t.Fatalf("应被拒绝（%s），got fs=%v", tc.want, fs)
			}
			if closeFn != nil {
				t.Fatal("失败时不应返回 close")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误应提到 %q, got %v", tc.want, err)
			}
		})
	}

	t.Run("缺 A 侧身份", func(t *testing.T) {
		factoryNoID := newMeshFSFactory(func(string) remote.RelayClient { return nil }, nil, nil)
		if _, _, err := factoryNoID(context.Background(), syncmgr.RemoteConfig{
			Node: "nodeB", Volume: "main", PeerPins: []string{pin},
		}); err == nil {
			t.Fatal("缺 A 侧身份应被拒绝")
		}
	})
}

func TestMeshFSFactory_WiresReadAndWriteFaces(t *testing.T) {
	fx := newMeshFactoryFixture(t)

	fs, closeFn, err := fx.factory(context.Background(), syncmgr.RemoteConfig{
		Name: "r-mesh", Node: fx.node, Volume: "main", PeerPins: []string{fx.pin},
	})
	if err != nil {
		t.Fatalf("装配应成功: %v", err)
	}
	if closeFn == nil {
		t.Fatal("应返回 close（关闭链路）")
	}
	defer closeFn()

	// 读面：ListDir 走 volread 链路（真隧道 + 真握手）。
	if _, err := fs.ListDir(context.Background(), ""); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	// 写面：MakeDir 走 volwrite 链路（WithWriteDialer 已接线）。
	if err := fs.MakeDir(context.Background(), "d"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}

	// 两个服务名各自的中继都必须被用到，且请求落到**对应**的面对应路由上。
	readFake, ok := fx.byName[remote.ServiceName]
	if !ok {
		t.Fatalf("应请求 %s 服务名的中继, got %v", remote.ServiceName, keysOf(fx.byName))
	}
	writeFake, ok := fx.byName[remote.ServiceNameWrite]
	if !ok {
		t.Fatalf("应请求 %s 服务名的中继（写面漏接线）, got %v", remote.ServiceNameWrite, keysOf(fx.byName))
	}
	if got := readFake.requests(); len(got) != 1 || got[0] != "GET /remote/list" {
		t.Fatalf("读面应只收到 GET /remote/list, got %v", got)
	}
	if got := writeFake.requests(); len(got) != 1 || got[0] != "POST /remote/mkdir" {
		t.Fatalf("写面应只收到 POST /remote/mkdir, got %v", got)
	}
}

// keysOf 返回 map 键（错误信息用）。
func keysOf(m map[string]*meshRelayFake) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 编译期：工厂返回类型确实是 syncexec.MeshFSFactory（接口形状不变）。
var _ syncexec.MeshFSFactory = newMeshFSFactory(func(string) remote.RelayClient { return nil }, nil, nil)

// TestLocalSelfBaseURL 钉住本机 base URL 派生（中继入口）：
// host 归一（空/0.0.0.0/:: → 127.0.0.1）、scheme 随 TLS、非法 addr 报错。
func TestLocalSelfBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		tls     bool
		want    string
		wantErr bool
	}{
		{"监听任意地址 → loopback + http", ":18083", false, "http://127.0.0.1:18083", false},
		{"0.0.0.0 → loopback", "0.0.0.0:18083", false, "http://127.0.0.1:18083", false},
		{"[::] → loopback", "[::]:18083", false, "http://127.0.0.1:18083", false},
		{"显式 loopback 原样", "127.0.0.1:9999", false, "http://127.0.0.1:9999", false},
		{"TLS 开启 → https", ":18083", true, "https://127.0.0.1:18083", false},
		{"非法 addr 报错", "nonsense", false, "", true},
		{"缺端口报错", "127.0.0.1", false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := localSelfBaseURL(&server.Config{Addr: tc.addr, TLS: server.TLSConfig{Enabled: tc.tls}})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应报错, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("base URL=%q want %q", got, tc.want)
			}
		})
	}

	if _, err := localSelfBaseURL(nil); err == nil {
		t.Fatal("cfg 为 nil 应报错（fail-closed）")
	}
}

// TestNewLocalSelfClient_FailClosedOnMissingCredential 钉住：本机凭据缺失时**不构造客户端**
// （拿空凭据去签名只会得到 401，且掩盖「本机无可用凭据」这一装配事实）。
func TestNewLocalSelfClient_FailClosedOnMissingCredential(t *testing.T) {
	cfg := &server.Config{Addr: ":18083"}
	if _, err := newLocalSelfClient(cfg, "", "", ""); err == nil {
		t.Fatal("AK 为空应报错")
	}
	if _, err := newLocalSelfClient(cfg, "ak-x", "", ""); err == nil {
		t.Fatal("SK 为空应报错")
	}
	if _, err := newLocalSelfClient(cfg, "ak-x", strings.Repeat("a", 64), "skey-0123456789ab"); err != nil {
		t.Fatalf("凭据齐备应构造成功: %v", err)
	}
}

// TestHasMeshRemote 钉住「按 remote.kind 判定是否装配 mesh 载体」：direct/空 kind 不触发，
// 仅 mesh 触发（空 kind = direct，旧配置零行为变更）。
func TestHasMeshRemote(t *testing.T) {
	cases := []struct {
		name    string
		remotes []server.SyncRemoteConfig
		want    bool
	}{
		{"空列表", nil, false},
		{"仅 direct（显式）", []server.SyncRemoteConfig{{Name: "a", Kind: "direct"}}, false},
		{"仅 direct（空 kind = 旧配置）", []server.SyncRemoteConfig{{Name: "a"}}, false},
		{"含 mesh", []server.SyncRemoteConfig{{Name: "a"}, {Name: "b", Kind: "mesh"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasMeshRemote(tc.remotes); got != tc.want {
				t.Fatalf("hasMeshRemote=%v want %v", got, tc.want)
			}
		})
	}
}

// TestSetupMeshFSFactory 钉住装配判定：只在「配了 mesh 远端」且前置齐备时注入工厂；
// 其余情况**不注入**（保持 fail-closed，不回落 direct）。
func TestSetupMeshFSFactory(t *testing.T) {
	newExec := func() *syncexec.Executor { return syncexec.NewExecutor(nil, nil) }
	newHandlers := func(t *testing.T, withCreds bool) *server.Handlers {
		t.Helper()
		cfg := server.Default()
		cfg.StorageRoot = t.TempDir()
		cfg.LogLevel = "error"
		var cfgPtr2 atomic.Pointer[server.Config]
		cfgPtr2.Store(cfg)
		opts := server.RegisterRoutesOpts{
			Mux:     http.NewServeMux(),
			CfgPtr:  &cfgPtr2,
			Version: "test",
			BuildAt: "test",
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if withCreds {
			opts.CredentialRing = accesskey.NewRingFromKeyPairs([]accesskey.KeyPair{{
				Key: "ak-" + strings.Repeat("a", 32), Secret: strings.Repeat("b", 64),
			}})
		}
		h := server.RegisterRoutes(t.Context(), opts)
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
	meshCfg := func(t *testing.T) *server.Config {
		t.Helper()
		cfg := server.Default()
		cfg.StorageRoot = t.TempDir()
		cfg.LogLevel = "error"
		cfg.Hub.XferIdentityFile = filepath.Join(t.TempDir(), "identity.json")
		cfg.SyncRemotes = []server.SyncRemoteConfig{{
			Name: "r-mesh", Kind: "mesh", Node: "nodeB", Volume: "main",
			PeerPins: []string{"sha256:" + strings.Repeat("a", 64)},
		}}
		return cfg
	}

	t.Run("无 mesh 远端 → 不注入", func(t *testing.T) {
		cfg := server.Default()
		cfg.StorageRoot = t.TempDir()
		cfg.SyncRemotes = []server.SyncRemoteConfig{{Name: "r1", URL: "https://example.com"}}
		exec := newExec()
		setupMeshFSFactory(exec, cfg, newHandlers(t, true), discardLoggerMain())
		if exec.MeshFS != nil {
			t.Fatal("无 mesh 远端时不应注入工厂")
		}
	})

	t.Run("有 mesh 远端但无凭据 → 不注入（告警）", func(t *testing.T) {
		exec := newExec()
		setupMeshFSFactory(exec, meshCfg(t), newHandlers(t, false), discardLoggerMain())
		if exec.MeshFS != nil {
			t.Fatal("无本机凭据时不得注入工厂（fail-closed）")
		}
	})

	t.Run("有 mesh 远端且凭据齐备 → 注入", func(t *testing.T) {
		exec := newExec()
		cfg := meshCfg(t)
		setupMeshFSFactory(exec, cfg, newHandlers(t, true), discardLoggerMain())
		if exec.MeshFS == nil {
			t.Fatal("凭据齐备时应注入工厂")
		}
	})

	t.Run("前置参数缺失 → 不 panic 不注入", func(t *testing.T) {
		setupMeshFSFactory(nil, nil, nil, discardLoggerMain())
		exec := newExec()
		setupMeshFSFactory(exec, meshCfg(t), nil, discardLoggerMain())
		if exec.MeshFS != nil {
			t.Fatal("h 为 nil 时不应注入")
		}
	})
}

// discardLoggerMain 是 cmd/sproxy 测试用的丢弃日志器。
func discardLoggerMain() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
