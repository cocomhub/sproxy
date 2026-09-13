// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// client_test.go 是**双端**端到端：真 B 侧 listener（真握手、真 AES-GCM、真双向 pin）
// × 真 A 侧 pkg/remote 客户端。
//
// 用 external test package（remote_test）导入 pkg/server 组装 B 侧：门禁 R4 只约束
// **非测试导入**，故测试可以引用装配层；这也与 Y-C 计划 T6 的「复用 pkg/server fixture」
// 一致。
package remote_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

const (
	testOwner = "alice"
	testVol   = "main"
	testNodeA = "nodeA"
)

// bEnd 是一次装配好的 B 侧只读面（真 listener）。
type bEnd struct {
	cfg  *server.Config
	addr string
	bFP  string
}

// startBEnd 装配 B 侧：单卷 + mesh_readers(nodeA, aFP, alice) 授权 + 只读 listener。
func startBEnd(t *testing.T, aFP string) *bEnd {
	t.Helper()
	bID, idErr := tunnel.GenerateIdentity()
	if idErr != nil {
		t.Fatal(idErr)
	}
	dir := t.TempDir()
	cfg := server.Default()
	cfg.StorageRoot = filepath.Join(dir, "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Hub.XferIdentityFile = filepath.Join(dir, "b-identity.json")
	if saveErr := tunnel.SaveIdentity(bID, cfg.Hub.XferIdentityFile); saveErr != nil {
		t.Fatal(saveErr)
	}
	cfg.Volumes = []server.VolumeConfig{{Name: testVol, Root: cfg.StorageRoot, ACL: &server.VolumeACLConfig{
		Mode:   server.VolumeACLAllow,
		Owners: []string{testOwner},
		MeshReaders: []server.VolumeMeshReaderConfig{{
			Node: testNodeA, Fingerprint: aFP, Owner: testOwner,
		}},
	}}}
	cfg.RemoteRead.Enabled = true
	cfg.RemoteRead.Listen = "127.0.0.1:0"
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
	ln, err := server.StartRemoteReadListener(ctx, cfg, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		cancel()
		t.Fatalf("启动 remote_read listener 失败: %v", err)
	}
	if ln == nil {
		cancel()
		t.Fatal("remote_read listener 为 nil（enabled 却未启动）")
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = h.Close()
		cancel()
	})
	return &bEnd{cfg: cfg, addr: ln.Addr(), bFP: bID.Fingerprint()}
}

// writeBFile 在 B 侧卷上写 `<owner>/user/<rel>`。
func writeBFile(t *testing.T, cfg *server.Config, rel string, body []byte) {
	t.Helper()
	abs := filepath.Join(cfg.StorageRoot, testOwner, "user", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newAClient 构造指向该 B 端（直连 Dialer，绕过 mesh 服务发现——本包测的是客户端行为）。
//
// aID 必须**显式传入**：它要与 B 侧 mesh_readers 里登记的指纹一致，否则握手 pinning 必失败
// （这里刻意不做「自动生成」，避免测试写出「自己和自己握手」的假绿）。
func newAClient(t *testing.T, b *bEnd, aID *tunnel.Identity, pins ...string) *remote.Client {
	t.Helper()
	if len(pins) == 0 {
		pins = []string{b.bFP}
	}
	opts := []remote.Option{remote.WithIdentity(aID)}
	for _, p := range pins {
		opts = append(opts, remote.WithPeerPin(testNodeA, p))
	}
	dialer := remote.DialerFunc(func(ctx context.Context, node string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", b.addr)
	})
	c := remote.New(dialer, opts...)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestClient_ListStatOpen_EndToEnd 端到端钉住：真握手 + 真加密 + 授权生效 + 下载字节全等。
func TestClient_ListStatOpen_EndToEnd(t *testing.T) {
	aID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	b := startBEnd(t, aID.Fingerprint())
	bodyA := []byte("remote client e2e content A")
	big := bytes.Repeat([]byte("y"), 140_000) // > 64 KiB 隧道帧上限：多帧流式
	writeBFile(t, b.cfg, "docs/a.bin", bodyA)
	writeBFile(t, b.cfg, "docs/big.bin", big)

	c := newAClient(t, b, aID)
	ref, err := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/docs")
	if err != nil {
		t.Fatal(err)
	}

	// list
	infos, err := c.List(context.Background(), ref)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("List 应返回 2 个条目, got %+v", infos)
	}
	for _, fi := range infos {
		if fi.Volume != testVol {
			t.Fatalf("条目应带卷名 %q: %+v", testVol, fi)
		}
	}

	// stat（存在 / 不存在 / 目录）
	fileRef, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/docs/a.bin")
	st, err := c.Stat(context.Background(), fileRef)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st == nil || st.Size != int64(len(bodyA)) || st.IsDir {
		t.Fatalf("Stat 结果不符: %+v", st)
	}
	wantCS := sha256.Sum256(bodyA)
	if st.Checksum != hex.EncodeToString(wantCS[:]) {
		t.Fatalf("Stat checksum=%q want %q", st.Checksum, hex.EncodeToString(wantCS[:]))
	}
	missingRef, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/docs/nope.bin")
	if got, err := c.Stat(context.Background(), missingRef); err != nil || got != nil {
		t.Fatalf("不存在的路径应 (nil,nil)，got (%+v, %v)", got, err)
	}
	dirRef, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/docs")
	if got, err := c.Stat(context.Background(), dirRef); err != nil || got == nil || !got.IsDir {
		t.Fatalf("目录 stat 应 IsDir=true，got (%+v, %v)", got, err)
	}

	// open（小文件与大文件：后者跨隧道多帧）
	for _, tc := range []struct {
		rel  string
		want []byte
	}{{"docs/a.bin", bodyA}, {"docs/big.bin", big}} {
		r, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/" + tc.rel)
		rc, err := c.Open(context.Background(), r)
		if err != nil {
			t.Fatalf("Open(%s): %v", tc.rel, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("读取 %s: %v", tc.rel, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("%s 内容不符: got %d 字节 want %d 字节", tc.rel, len(got), len(tc.want))
		}
	}
}

// TestClient_UnpinnedPeer_FailsClosed 钉住不 TOFU：未配 pin 立即拒，且**不发起连接**。
func TestClient_UnpinnedPeer_FailsClosed(t *testing.T) {
	aID, _ := tunnel.GenerateIdentity()
	b := startBEnd(t, aID.Fingerprint())
	writeBFile(t, b.cfg, "docs/a.bin", []byte("x"))

	dialed := false
	aID2, _ := tunnel.GenerateIdentity()
	c := remote.New(remote.DialerFunc(func(context.Context, string) (net.Conn, error) {
		dialed = true
		return nil, io.EOF
	}), remote.WithIdentity(aID2)) // 故意不配 WithPeerPin
	t.Cleanup(func() { _ = c.Close() })

	ref, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/docs")
	if _, err := c.List(context.Background(), ref); err == nil {
		t.Fatal("未配 pin 应 fail-closed 报错")
	}
	if dialed {
		t.Fatal("未配 pin 时不得发起连接（应在拨号前拒绝）")
	}
}

// TestClient_WrongPin_FailsClosed 钉住指纹不符时握手失败（不降级、不回落明文）。
func TestClient_WrongPin_FailsClosed(t *testing.T) {
	aID, _ := tunnel.GenerateIdentity()
	b := startBEnd(t, aID.Fingerprint())
	writeBFile(t, b.cfg, "docs/a.bin", []byte("x"))

	otherID, _ := tunnel.GenerateIdentity()
	c := newAClient(t, b, aID, otherID.Fingerprint()) // pin 成别人的指纹（A 的身份仍是 B 登记的那个）

	ref, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/docs")
	if _, err := c.List(context.Background(), ref); err == nil {
		t.Fatal("pin 不符应握手失败")
	}
}

// TestClient_MissingIdentity_FailsClosed 钉住未配身份即拒（无身份无法双向 pin）。
func TestClient_MissingIdentity_FailsClosed(t *testing.T) {
	b := &bEnd{addr: "127.0.0.1:1", bFP: "sha256:" + strings.Repeat("a", 64)}
	c := remote.New(remote.DialerFunc(func(context.Context, string) (net.Conn, error) {
		t.Fatal("未配身份时不得发起连接")
		return nil, io.EOF
	}), remote.WithPeerPin(testNodeA, b.bFP))
	t.Cleanup(func() { _ = c.Close() })

	ref, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/docs")
	if _, err := c.List(context.Background(), ref); err == nil {
		t.Fatal("未配身份应 fail-closed 报错")
	}
}

// TestClient_LinkReuse 钉住「一个节点一条链路」：多次调用复用同一 mux（握手只跑一次）。
func TestClient_LinkReuse(t *testing.T) {
	aID, _ := tunnel.GenerateIdentity()
	b := startBEnd(t, aID.Fingerprint())
	writeBFile(t, b.cfg, "docs/a.bin", []byte("reuse"))

	var dialCount int
	dialer := remote.DialerFunc(func(ctx context.Context, node string) (net.Conn, error) {
		dialCount++
		var d net.Dialer
		return d.DialContext(ctx, "tcp", b.addr)
	})
	c := remote.New(dialer, remote.WithIdentity(aID), remote.WithPeerPin(testNodeA, b.bFP))
	t.Cleanup(func() { _ = c.Close() })

	ref, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol + "/docs")
	for i := range 3 {
		if _, err := c.List(context.Background(), ref); err != nil {
			t.Fatalf("第 %d 次 List: %v", i, err)
		}
	}
	if dialCount != 1 {
		t.Fatalf("同节点多次调用应只拨号 1 次（复用链路），got %d", dialCount)
	}
}

// TestRemoteFS_ReadAndUnsupportedWrites 钉住 sync.FS 形态：读可用、写明确 unsupported。
func TestRemoteFS_ReadAndUnsupportedWrites(t *testing.T) {
	aID, _ := tunnel.GenerateIdentity()
	b := startBEnd(t, aID.Fingerprint())
	writeBFile(t, b.cfg, "docs/a.bin", []byte("fs content"))

	c := newAClient(t, b, aID)
	ref, _ := remote.ParseRef("remote://" + testNodeA + "/" + testVol)
	fs := c.FS(ref)
	ctx := context.Background()

	// 读：ListDir 返回完整相对路径（FS 契约）
	entries, err := fs.ListDir(ctx, "docs")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "docs/a.bin" {
		t.Fatalf("ListDir 的 Path 必须是 FS 根相对完整路径: %+v", entries)
	}
	// 读：Stat 存在/不存在
	st, err := fs.Stat(ctx, "docs/a.bin")
	if err != nil || st == nil || st.Size != int64(len("fs content")) {
		t.Fatalf("FS.Stat 不符: (%+v, %v)", st, err)
	}
	if got, sErr := fs.Stat(ctx, "docs/none"); sErr != nil || got != nil {
		t.Fatalf("FS.Stat 不存在应 (nil,nil): (%+v, %v)", got, sErr)
	}
	// 读：卷根 Stat 以目录条目表达
	if got, sErr := fs.Stat(ctx, ""); sErr != nil || got == nil || !got.IsDir {
		t.Fatalf("卷根 Stat 应为目录: (%+v, %v)", got, sErr)
	}
	// 读：OpenRead
	rc, err := fs.OpenRead(ctx, "docs/a.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "fs content" {
		t.Fatalf("OpenRead 内容=%q", got)
	}

	// 写：四个方法都必须明确报 unsupported（不得静默成功）
	for name, err := range map[string]error{
		"WriteFile": fs.WriteFile(ctx, "docs/new.bin", strings.NewReader("x"), 1, time.Now().UnixNano()),
		"Rename":    fs.Rename(ctx, "docs/a.bin", "docs/b.bin"),
		"Delete":    fs.Delete(ctx, "docs/a.bin"),
		"MakeDir":   fs.MakeDir(ctx, "docs/sub"),
	} {
		if err == nil {
			t.Fatalf("%s 应返回 unsupported 错误（不得静默成功）", name)
		}
		// 必须可用 errors.Is 判定（哨兵错误经 %w 包装）——调用方据此区分「不支持」与真实失败。
		if !errors.Is(err, remote.ErrUnsupported) {
			t.Fatalf("%s 的错误应包装 remote.ErrUnsupported: %v", name, err)
		}
	}
}
