// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// write_test.go 是 A 侧 `pkg/remote` **写方法**（Y 二期 P3-c）的测试：协议钉住（假对端）
// + 真双端端到端（真 listener、真握手、真授权、真落盘）。
//
// 两条链路的分工是本片的核心结构事实：
//   - 写操作走**写面**（独立服务名 `volwrite` / 独立 listener / 独立路由白名单）；
//   - `Rename`/`Delete` 需要的 checksum 前置条件走**读面**（`/remote/stat`）——A 侧必须先
//     `Stat` 取 checksum 再带进写请求（对端写面只有写 op，没有 stat）。
package remote_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// recordedReq 是假对端收到的一次请求（协议钉住用）。
type recordedReq struct {
	method  string
	path    string
	query   map[string]string
	headers map[string]string
	body    []byte
}

// fakeBEnd 是「协议钉住」用的假对端：在**内存管道**上跑真隧道（真握手、真加密），
// handler 记录每次请求并回预设响应。用它钉住 A→B 的线协议（路径/查询参数/头部/体）。
type fakeBEnd struct {
	bFP    string
	dialer remote.Dialer

	mu   sync.Mutex
	reqs []recordedReq
}

// requests 返回已记录请求的副本。
func (f *fakeBEnd) requests() []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedReq, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// newFakeBEnd 起假对端：aFP 是客户端身份指纹（假对端 pin 它）；handler 为对端路由，
// **nil 表示用内建「记录请求 + 200 {"success":true}」**（协议钉住用）。
func newFakeBEnd(t *testing.T, aFP string, handler http.Handler) *fakeBEnd {
	t.Helper()
	bID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBEnd{bFP: bID.Fingerprint()}
	if handler == nil {
		handler = f.recordAndOK()
	}
	f.dialer = remote.DialerFunc(func(ctx context.Context, _ string) (net.Conn, error) {
		clientSide, serverSide := net.Pipe()
		go func() {
			m := mux.New(builtin.FromNetConn(serverSide), mux.RoleListener)
			defer func() { _ = m.Close() }()
			tun := tunnel.NewTunnel(m, tunnel.DeriveRemoteStaticKey(bID.Fingerprint()),
				tunnel.WithIdentity(bID), tunnel.WithPeerFingerprints([]string{aFP}))
			_ = tun.Serve(ctx, handler)
			_ = serverSide.Close()
		}()
		return clientSide, nil
	})
	return f
}

// recordAndOK 记录请求并回 200 {"success":true}。
func (f *fakeBEnd) recordAndOK() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	})
}

// record 把一次请求记入 f.reqs（读 body/查询/关键头）。
func (f *fakeBEnd) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	q := map[string]string{}
	for k := range r.URL.Query() {
		q[k] = r.URL.Query().Get(k)
	}
	hdr := map[string]string{}
	for _, name := range []string{"X-File-Checksum", "X-File-MTime", "Content-Type"} {
		if v := r.Header.Get(name); v != "" {
			hdr[name] = v
		}
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, recordedReq{method: r.Method, path: r.URL.Path, query: q, headers: hdr, body: body})
	f.mu.Unlock()
}

// newWriteClient 构造「读面与写面共用同一假对端」的客户端（两者都指向 f.dialer）。
func newWriteClient(t *testing.T, f *fakeBEnd, aID *tunnel.Identity, withWrite bool) *remote.Client {
	t.Helper()
	opts := []remote.Option{remote.WithIdentity(aID), remote.WithPeerPin(testNodeA, f.bFP)}
	if withWrite {
		opts = append(opts, remote.WithWriteDialer(f.dialer))
	}
	c := remote.New(f.dialer, opts...)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestClient_WriteFile_ProtocolPins 钉住 A→B 的写请求线协议：
// `POST /remote/write?volume&path`，`X-File-Checksum` = **服务端自算**的 SHA-256，
// `X-File-MTime` 透传，body 为原始内容（不是 multipart）。
func TestClient_WriteFile_ProtocolPins(t *testing.T) {
	aID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("pinned-body")
	sum := sha256HexBytes(body)

	f := newFakeBEnd(t, aID.Fingerprint(), nil) // nil → 默认「记录 + 200 {"success":true}」
	c := newWriteClient(t, f, aID, true)

	ref, err := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/dir/a.bin")
	if err != nil {
		t.Fatal(err)
	}
	const mtime = int64(1_700_000_000) * 1e9
	if wErr := c.WriteFile(t.Context(), ref, bytes.NewReader(body), int64(len(body)), mtime); wErr != nil {
		t.Fatalf("WriteFile: %v", wErr)
	}

	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("应恰好一次写请求, got %d: %+v", len(reqs), reqs)
	}
	got := reqs[0]
	if got.method != http.MethodPost || got.path != "/remote/write" {
		t.Fatalf("请求行=%s %s want POST /remote/write", got.method, got.path)
	}
	if got.query["volume"] != testVol || got.query["path"] != "dir/a.bin" {
		t.Fatalf("查询参数=%v want volume=%s path=dir/a.bin", got.query, testVol)
	}
	if got.headers["X-File-Checksum"] != sum {
		t.Fatalf("X-File-Checksum=%q want %q（A 侧自算 SHA-256 后随请求带上）", got.headers["X-File-Checksum"], sum)
	}
	if got.headers["X-File-MTime"] == "" {
		t.Fatalf("X-File-MTime 应透传, headers=%v", got.headers)
	}
	if !bytes.Equal(got.body, body) {
		t.Fatalf("body=%q want %q（原始内容，不是 multipart）", got.body, body)
	}
}

// TestClient_RenameDelete_StatFirstChecksum 钉住「**先 Stat 取 checksum**」这一前置条件：
// 改名/删除请求必须携带**读面 Stat 报出的 checksum**（对端写面没有 stat，A 侧不得凭空猜）。
func TestClient_RenameDelete_StatFirstChecksum(t *testing.T) {
	aID, _ := tunnel.GenerateIdentity()
	const checksum = "sha256:" + "ab"
	// 读面（stat）：回 X-File-* 头；写面：记录请求并回成功。
	readHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/remote/stat" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-File-Size", "7")
		w.Header().Set("X-File-MTime", "1700000000000000000")
		w.Header().Set("X-File-Checksum", checksum)
		w.WriteHeader(http.StatusOK)
	})

	bID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var writeReqs []recordedReq
	writeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := map[string]string{}
		for k := range r.URL.Query() {
			q[k] = r.URL.Query().Get(k)
		}
		mu.Lock()
		writeReqs = append(writeReqs, recordedReq{
			method: r.Method, path: r.URL.Path, query: q,
			headers: map[string]string{"X-File-Checksum": r.Header.Get("X-File-Checksum")},
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	// 两个假对端：读面给 readHandler，写面给 writeHandler（身份/密钥相同即可）。
	dialerPair := func(h http.Handler) remote.Dialer {
		return remote.DialerFunc(func(ctx context.Context, _ string) (net.Conn, error) {
			clientSide, serverSide := net.Pipe()
			go func() {
				m := mux.New(builtin.FromNetConn(serverSide), mux.RoleListener)
				defer func() { _ = m.Close() }()
				tun := tunnel.NewTunnel(m, tunnel.DeriveRemoteStaticKey(bID.Fingerprint()),
					tunnel.WithIdentity(bID), tunnel.WithPeerFingerprints([]string{aID.Fingerprint()}))
				_ = tun.Serve(ctx, h)
				_ = serverSide.Close()
			}()
			return clientSide, nil
		})
	}
	bFP := bID.Fingerprint()
	c := remote.New(dialerPair(readHandler),
		remote.WithIdentity(aID),
		remote.WithPeerPin(testNodeA, bFP),
		remote.WithWriteDialer(dialerPair(writeHandler)),
	)
	t.Cleanup(func() { _ = c.Close() })

	// 删除：应先 stat 再 delete（带 checksum）
	delRef, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/gone.bin")
	if err := c.Delete(t.Context(), delRef); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 改名：应先 stat 源再 rename（带 checksum）
	fromRef, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/a.bin")
	toRef, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/b.bin")
	if err := c.Rename(t.Context(), fromRef, toRef); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(writeReqs) != 2 {
		t.Fatalf("应有 2 次写请求（delete + rename）, got %d: %+v", len(writeReqs), writeReqs)
	}
	if writeReqs[0].path != "/remote/delete" || writeReqs[1].path != "/remote/rename" {
		t.Fatalf("写请求路径不符: %+v", writeReqs)
	}
	for i, req := range writeReqs {
		if req.headers["X-File-Checksum"] != checksum {
			t.Fatalf("写请求[%d] 应携带读面 Stat 报出的 checksum %q, got %q", i, checksum, req.headers["X-File-Checksum"])
		}
	}
	if writeReqs[1].query["from"] != "a.bin" || writeReqs[1].query["to"] != "b.bin" {
		t.Fatalf("rename 查询参数=%v want from=a.bin to=b.bin", writeReqs[1].query)
	}
}

// TestClient_WriteOps_RequireWriteDialer 钉住 fail-closed：未配置写面拨号器时写操作必须
// **明确报错**（不是静默成功、也不是假装只读 unsupported），且不发出任何请求。
func TestClient_WriteOps_RequireWriteDialer(t *testing.T) {
	aID, _ := tunnel.GenerateIdentity()
	f := newFakeBEnd(t, aID.Fingerprint(), nil)
	c := newWriteClient(t, f, aID, false) // 不配写面

	ref, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/x.bin")
	if err := c.WriteFile(t.Context(), ref, strings.NewReader("x"), 1, 0); err == nil {
		t.Fatal("未配置写面时 WriteFile 必须报错")
	}
	if err := c.MakeDir(t.Context(), ref); err == nil {
		t.Fatal("未配置写面时 MakeDir 必须报错")
	}
	if err := c.Delete(t.Context(), ref); err == nil {
		t.Fatal("未配置写面时 Delete 必须报错")
	}
	if len(f.requests()) != 0 {
		t.Fatalf("未配置写面时不得发出请求: %+v", f.requests())
	}
}

// startBEndWrite 装配 B 侧**读+写**两个真 listener（同一配置、同一身份、scope=rw），
// 返回只读地址、写地址、B 指纹与 cfg。
func startBEndWrite(t *testing.T, aFP string) (readAddr, writeAddr, bFP string, cfg *server.Config) {
	t.Helper()
	bID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg = server.Default()
	cfg.StorageRoot = filepath.Join(dir, "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Hub.XferIdentityFile = filepath.Join(dir, "b-identity.json")
	if sErr := tunnel.SaveIdentity(bID, cfg.Hub.XferIdentityFile); sErr != nil {
		t.Fatal(sErr)
	}
	cfg.Volumes = []server.VolumeConfig{{Name: testVol, Root: cfg.StorageRoot, ACL: &server.VolumeACLConfig{
		Mode:   server.VolumeACLAllow,
		Owners: []string{testOwner},
		MeshReaders: []server.VolumeMeshReaderConfig{{
			Node: testNodeA, Fingerprint: aFP, Owner: testOwner, Scope: "rw",
		}},
	}}}
	cfg.RemoteRead.Enabled = true
	cfg.RemoteRead.Listen = "127.0.0.1:0"
	cfg.RemoteWrite.Enabled = true
	cfg.RemoteWrite.Listen = "127.0.0.1:0"
	if vErr := cfg.Validate(); vErr != nil {
		t.Fatalf("B 侧配置校验失败: %v", vErr)
	}

	var cfgPtr atomic.Pointer[server.Config]
	cfgPtr.Store(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	h := server.RegisterRoutes(ctx, server.RegisterRoutesOpts{
		Mux:                   http.NewServeMux(),
		CfgPtr:                &cfgPtr,
		Logger:                slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		AllowInsecureLoopback: true,
	})
	rl, err := server.StartRemoteReadListener(ctx, cfg, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		cancel()
		t.Fatalf("启动只读 listener: %v", err)
	}
	wl, err := server.StartRemoteWriteListener(ctx, cfg, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		cancel()
		t.Fatalf("启动写 listener: %v", err)
	}
	if rl == nil || wl == nil {
		cancel()
		t.Fatal("两个 listener 都应启动")
	}
	t.Cleanup(func() {
		_ = rl.Close()
		_ = wl.Close()
		_ = h.Close()
		cancel()
	})
	return rl.Addr(), wl.Addr(), bID.Fingerprint(), cfg
}

// TestClient_WriteOps_EndToEnd 真双端：读+写两个 listener、真握手、真授权、真落盘。
// 覆盖 sync.FS 的 4 个写方法（MakeDir/WriteFile/Rename/Delete）与写入后的 Stat 可读回。
func TestClient_WriteOps_EndToEnd(t *testing.T) {
	aID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	readAddr, writeAddr, bFP, cfg := startBEndWrite(t, aID.Fingerprint())

	dialTo := func(addr string) remote.Dialer {
		return remote.DialerFunc(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		})
	}
	c := remote.New(dialTo(readAddr),
		remote.WithIdentity(aID),
		remote.WithPeerPin(testNodeA, bFP),
		remote.WithWriteDialer(dialTo(writeAddr)),
	)
	t.Cleanup(func() { _ = c.Close() })

	ref, err := remote.ParseRef("remote://" + testNodeA + "/" + testVol)
	if err != nil {
		t.Fatal(err)
	}
	fs := c.FS(ref)
	ctx := t.Context()

	// MakeDir
	if mErr := fs.MakeDir(ctx, "d"); mErr != nil {
		t.Fatalf("MakeDir: %v", mErr)
	}
	if fi, sErr := os.Stat(filepath.Join(cfg.StorageRoot, testOwner, "user", "d")); sErr != nil || !fi.IsDir() {
		t.Fatalf("目录未落盘: %v", sErr)
	}

	// WriteFile（内容 + mtime）
	body := []byte("remote-fs-write")
	const mtime = int64(1_700_000_000)
	if wErr := fs.WriteFile(ctx, "d/a.bin", bytes.NewReader(body), int64(len(body)), mtime); wErr != nil {
		t.Fatalf("WriteFile: %v", wErr)
	}
	abs := filepath.Join(cfg.StorageRoot, testOwner, "user", "d", "a.bin")
	got, rErr := os.ReadFile(abs)
	if rErr != nil {
		t.Fatalf("读落盘文件: %v", rErr)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("落盘内容=%q want %q", got, body)
	}
	if fi, sErr := os.Stat(abs); sErr != nil || fi.ModTime().Unix() != mtime {
		t.Fatalf("落盘 mtime 不符: %v (err=%v)", fi.ModTime().Unix(), sErr)
	}

	// Rename（A 侧先 Stat 取 checksum）
	if rErr2 := fs.Rename(ctx, "d/a.bin", "d/b.bin"); rErr2 != nil {
		t.Fatalf("Rename: %v", rErr2)
	}
	if _, sErr := os.Stat(filepath.Join(cfg.StorageRoot, testOwner, "user", "d", "a.bin")); !os.IsNotExist(sErr) {
		t.Fatalf("改名后源应消失: %v", sErr)
	}
	if got, rErr3 := os.ReadFile(filepath.Join(cfg.StorageRoot, testOwner, "user", "d", "b.bin")); rErr3 != nil || !bytes.Equal(got, body) {
		t.Fatalf("改名后目标不符: %q (err=%v)", got, rErr3)
	}

	// Delete（A 侧先 Stat 取 checksum）
	if dErr := fs.Delete(ctx, "d/b.bin"); dErr != nil {
		t.Fatalf("Delete: %v", dErr)
	}
	if _, sErr := os.Stat(filepath.Join(cfg.StorageRoot, testOwner, "user", "d", "b.bin")); !os.IsNotExist(sErr) {
		t.Fatalf("删除后文件应消失: %v", sErr)
	}
	// 删除后 Stat 应 (nil, nil)
	st, stErr := fs.Stat(ctx, "d/b.bin")
	if stErr != nil || st != nil {
		t.Fatalf("删除后 Stat 应 (nil,nil): (%+v, %v)", st, stErr)
	}
}

// sha256HexBytes 计算 SHA-256 十六进制（与 B 侧断言同一口径）。
func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
