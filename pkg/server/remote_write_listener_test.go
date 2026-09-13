// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// remote_write_listener_test.go 是 Y 二期写面（P3-b2）的**端到端中测**：在 loopback 上起
// **真** RemoteWriteListener，A 侧经 net.Conn → xfer 桥 → mux → tunnel.NewTunnel 建**真握手、
// 真加密、真 pin** 的链路，跑 write/mkdir/delete 全链路；并钉住「只读对端连写面握手都过不了」
// （写通路物理隔离，不是「连上再拒」）。

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// remoteWriteDualEnd 是写面中测的双端上下文：B 侧真 listener + 两个对端身份
// （aID=scope rw 应能写；cID=scope read 应连不上写面）。
type remoteWriteDualEnd struct {
	cfg *Config
	aID *tunnel.Identity
	cID *tunnel.Identity
	bFP string // B 侧身份指纹（A 侧用 pin 它 + 派生静态密钥）
	ln  *RemoteWriteListener
}

// startRemoteWriteDualEnd 装配 B 侧真写 listener 与两个对端身份。
func startRemoteWriteDualEnd(t *testing.T) remoteWriteDualEnd {
	t.Helper()
	bID, bErr := tunnel.GenerateIdentity()
	if bErr != nil {
		t.Fatal(bErr)
	}
	aID, aErr := tunnel.GenerateIdentity()
	if aErr != nil {
		t.Fatal(aErr)
	}
	cID, cErr := tunnel.GenerateIdentity()
	if cErr != nil {
		t.Fatal(cErr)
	}
	dir := t.TempDir()
	aFP, cFP, bFP := aID.Fingerprint(), cID.Fingerprint(), bID.Fingerprint()

	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Hub.XferIdentityFile = filepath.Join(dir, "b-identity.json")
	if sErr := tunnel.SaveIdentity(bID, cfg.Hub.XferIdentityFile); sErr != nil {
		t.Fatal(sErr)
	}
	cfg.Volumes = []VolumeConfig{{Name: testDualVol, Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testDualOwner},
		MeshReaders: []VolumeMeshReaderConfig{
			{Node: testReaderNodeA, Fingerprint: aFP, Owner: testDualOwner, Scope: volume.MeshScopeRW},
			{Node: "nodeC", Fingerprint: cFP, Owner: testDualOwner, Scope: volume.MeshScopeRead},
		},
	}}}
	cfg.RemoteWrite.Enabled = true
	cfg.RemoteWrite.Listen = "127.0.0.1:0" // 端口由 OS 分配
	cfg.RemoteWrite.HandshakeTimeout = 10 * time.Second
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg: %v", err)
	}

	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	rctx, rcancel := context.WithCancel(t.Context())
	ln, err := StartRemoteWriteListener(rctx, cfg, h, testutil.DiscardLogger())
	if err != nil {
		t.Fatalf("StartRemoteWriteListener: %v", err)
	}
	t.Cleanup(func() {
		rcancel()
		_ = ln.Close()
	})
	return remoteWriteDualEnd{cfg: cfg, aID: aID, cID: cID, bFP: bFP, ln: ln}
}

// dialRemoteWrite 建 A 侧真链路（语义同只读面的 dialRemoteRead）。
func dialRemoteWrite(t *testing.T, ln *RemoteWriteListener, aID *tunnel.Identity, bFP string, pins []string) (*tunnel.Tunnel, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", ln.Addr())
	if err != nil {
		t.Fatalf("net.Dial %s: %v", ln.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	m := mux.New(builtin.FromNetConn(conn), mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	return tunnel.NewTunnel(m, tunnel.DeriveRemoteStaticKey(bFP),
		tunnel.WithIdentity(aID), tunnel.WithPeerFingerprints(pins)), conn
}

// tunDoRemoteWrite 经隧道发一次写请求；body 非 nil 时带上，checksum 非空时带 X-File-Checksum。
func tunDoRemoteWrite(t *testing.T, tun *tunnel.Tunnel, target string, body []byte, checksum string) (*http.Response, error) {
	t.Helper()
	var req *http.Request
	var err error
	if body != nil {
		req, err = http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	} else {
		req, err = http.NewRequest(http.MethodPost, target, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if checksum != "" {
		req.Header.Set(headerFileChecksum, checksum)
	}
	return tun.Do(req)
}

// TestRemoteWrite_ListenerEndToEnd 跑通「真握手 → 写授权 → 直调域方法 → 落盘」全链路，
// 并验证本地上传同一 checksum 台账/磁盘副作用。
func TestRemoteWrite_ListenerEndToEnd(t *testing.T) {
	e := startRemoteWriteDualEnd(t)
	cfg, ln := e.cfg, e.ln
	tun, _ := dialRemoteWrite(t, ln, e.aID, e.bFP, []string{e.bFP})

	const body = "listener-e2e-body"
	sum := sha256hex([]byte(body))

	// mkdir
	resp, err := tunDoRemoteWrite(t, tun, "/remote/mkdir?volume="+testDualVol+"&path=e2e", nil, "")
	if err != nil {
		t.Fatalf("mkdir 隧道请求: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mkdir 状态=%d", resp.StatusCode)
	}

	// write
	resp, err = tunDoRemoteWrite(t, tun, "/remote/write?volume="+testDualVol+"&path=e2e/a.txt", []byte(body), sum)
	if err != nil {
		t.Fatalf("write 隧道请求: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write 状态=%d", resp.StatusCode)
	}
	abs := filepath.Join(cfg.StorageRoot, testDualOwner, "user", "e2e", "a.txt")
	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("读落盘文件: %v", err)
	}
	if string(got) != body {
		t.Fatalf("落盘内容=%q want %q", got, body)
	}

	// delete（checksum 须匹配）
	resp, err = tunDoRemoteWrite(t, tun, "/remote/delete?volume="+testDualVol+"&path=e2e/a.txt", nil, sum)
	if err != nil {
		t.Fatalf("delete 隧道请求: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete 状态=%d", resp.StatusCode)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("删除后文件应消失: %v", err)
	}
}

// TestRemoteWrite_ListenerReadOnlyPeerCannotConnect 钉住**写通路物理隔离**：
// scope=read 的对端不在写面 pin 列表里，故其握手失败（拿不到任何写能力），而非「连上后被拒」。
func TestRemoteWrite_ListenerReadOnlyPeerCannotConnect(t *testing.T) {
	e := startRemoteWriteDualEnd(t)
	cfg, ln := e.cfg, e.ln

	// C 虽在 mesh_readers 里（scope=read），但不在写面 pin 列表 ⇒ 握手必然失败。
	tun, _ := dialRemoteWrite(t, ln, e.cID, e.bFP, []string{e.bFP})
	resp, err := tunDoRemoteWrite(t, tun, "/remote/write?volume="+testDualVol+"&path=x.txt", []byte("nope"), sha256hex([]byte("nope")))
	if err == nil {
		// 握手若被容忍，请求也必须是失败状态（双保险：不可写出任何字节）。
		if resp != nil {
			_ = resp.Body.Close()
			t.Fatalf("只读对端不应成功建链/写入, got status=%d", resp.StatusCode)
		}
		t.Fatal("只读对端不应成功建链")
	}
	// 关键断言：磁盘上什么都没发生。
	entries, _ := os.ReadDir(filepath.Join(cfg.StorageRoot, testDualOwner, "user"))
	for _, e := range entries {
		if e.Name() == "x.txt" {
			t.Fatal("只读对端不得写入任何文件")
		}
	}
}

// TestStartRemoteWriteListener_RefusesWithoutWriteScope 钉住 listener 自身的 fail-closed 守卫
// （纵深防御：即使绕过 Config.Validate 直接调用，无「授写」条目也拒绝启动）。
func TestStartRemoteWriteListener_RefusesWithoutWriteScope(t *testing.T) {
	bID, bErr := tunnel.GenerateIdentity()
	if bErr != nil {
		t.Fatal(bErr)
	}
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "vol-main")
	cfg.LogLevel = "error"
	cfg.Hub.XferIdentityFile = filepath.Join(dir, "b-identity.json")
	if sErr := tunnel.SaveIdentity(bID, cfg.Hub.XferIdentityFile); sErr != nil {
		t.Fatal(sErr)
	}
	// 只有 scope=read 的条目（故意绕过 Validate 直接调 Start）。
	cfg.Volumes = []VolumeConfig{{Name: testDualVol, Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testDualOwner},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: bID.Fingerprint(), Owner: testDualOwner, Scope: volume.MeshScopeRead,
		}},
	}}}
	cfg.RemoteWrite.Enabled = true
	cfg.RemoteWrite.Listen = "127.0.0.1:0"
	cfg.SetDefaults()

	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	ln, err := StartRemoteWriteListener(t.Context(), cfg, h, testutil.DiscardLogger())
	if err == nil {
		if ln != nil {
			_ = ln.Close()
		}
		t.Fatal("无授写条目时写面必须拒绝启动（fail-closed）")
	}
	if !strings.Contains(err.Error(), "fail-closed") {
		t.Fatalf("错误信息应说明 fail-closed, got %v", err)
	}
	// 未启用时返回 (nil, nil)。
	cfg.RemoteWrite.Enabled = false
	if ln2, err2 := StartRemoteWriteListener(t.Context(), cfg, h, testutil.DiscardLogger()); err2 != nil || ln2 != nil {
		t.Fatalf("未启用应返回 (nil, nil), got ln=%v err=%v", ln2, err2)
	}
}
