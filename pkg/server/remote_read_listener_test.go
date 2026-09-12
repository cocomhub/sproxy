// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// 本文件是 Y 一期「中测」：在 loopback 上起**真** RemoteReadListener，A 侧经
// net.Conn → builtin.FromNetConn → mux → tunnel.NewTunnel 建**真握手、真加密、真 pin**
// 的链路，跑 list/stat/download 全链路。不是 httptest 直调 handler（那已在
// remote_read_test.go 覆盖），故本文件额外钉住：握手产出的对端指纹确实流入授权判定、
// 错 pin 时握手 fail-closed 且不返回任何数据。

const (
	testDualOwner = "alice"
	testDualVol   = "main"
)

// startRemoteReadDualEnd 装配 B 侧真 listener 与 A 侧身份，返回 (cfg, A 身份, B 指纹, listener)。
//
// B 的身份（Ed25519）写入临时目录并配 Hub.XferIdentityFile；B 的静态密钥由该身份指纹
// 派生（DeriveRemoteStaticKey），A 侧用同一 bFP 派生 ⇒ 两端一致。
func startRemoteReadDualEnd(t *testing.T) (*Config, *tunnel.Identity, string, *RemoteReadListener) {
	t.Helper()
	bID, bErr := tunnel.GenerateIdentity()
	if bErr != nil {
		t.Fatal(bErr)
	}
	aID, aErr := tunnel.GenerateIdentity()
	if aErr != nil {
		t.Fatal(aErr)
	}
	dir := t.TempDir()
	aFP, bFP := aID.Fingerprint(), bID.Fingerprint()

	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Hub.XferIdentityFile = filepath.Join(dir, "b-identity.json")
	if err := tunnel.SaveIdentity(bID, cfg.Hub.XferIdentityFile); err != nil {
		t.Fatal(err)
	}
	cfg.Volumes = []VolumeConfig{{Name: testDualVol, Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testDualOwner},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: aFP, Owner: testDualOwner,
		}},
	}}}
	cfg.RemoteRead.Enabled = true
	cfg.RemoteRead.Listen = "127.0.0.1:0" // 端口由 OS 分配，测试读 ln.Addr()
	cfg.RemoteRead.HandshakeTimeout = 10 * time.Second
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg: %v", err)
	}

	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	// 可取消 ctx：teardown 时用它让 B 侧 Serve 的 accept 循环立即收敛（不必等 TCP
	// 读错误重试退避），从而 ln.Close() 的 wg.Wait() 快速返回。
	rctx, rcancel := context.WithCancel(t.Context())
	ln, err := StartRemoteReadListener(rctx, cfg, h, testutil.DiscardLogger())
	if err != nil {
		t.Fatalf("StartRemoteReadListener: %v", err)
	}
	t.Cleanup(func() {
		rcancel()
		_ = ln.Close()
	})
	return cfg, aID, bFP, ln
}

// dialRemoteRead 建 A 侧真链路：loopback TCP → xfer 桥 → mux(RoleDialer) → Tunnel。
// pins 为 A 侧对本端所信任的对端指纹列表（双向 pin 的 A 侧一半）。
func dialRemoteRead(t *testing.T, ln *RemoteReadListener, aID *tunnel.Identity, bFP string, pins []string) *tunnel.Tunnel {
	t.Helper()
	conn, err := net.Dial("tcp", ln.Addr())
	if err != nil {
		t.Fatalf("net.Dial %s: %v", ln.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	m := mux.New(builtin.FromNetConn(conn), mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	return tunnel.NewTunnel(m, tunnel.DeriveRemoteStaticKey(bFP),
		tunnel.WithIdentity(aID), tunnel.WithPeerFingerprints(pins))
}

// tunDoRemoteRead 发一次隧道请求，返回响应（调用方负责读/关 body）。
func tunDoRemoteRead(t *testing.T, tun *tunnel.Tunnel, method, target string, mutate ...func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range mutate {
		f(req)
	}
	resp, err := tun.Do(req)
	if err != nil {
		t.Fatalf("隧道请求 %s %s: %v", method, target, err)
	}
	return resp
}

// readTunnelBody 读完并关闭响应体。
func readTunnelBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取隧道响应体: %v", err)
	}
	return b
}

// TestRemoteRead_DualEnd_ListStatDownload 端到端钉住：
//   - B 侧 listener 真监听 loopback、真做 Ed25519 双向 pin 握手、真 AES-GCM 加密；
//   - 握手产出的对端指纹确实流入授权判定（A 的指纹只出现在 B 的 mesh_readers 里，
//     list/stat/download 能 200 即证明该通路成立——指纹错/空都会 401/404）；
//   - 下载字节 SHA-256 与 **B 磁盘原件**全等（大文件跨 64 KiB 隧道分块帧）。
func TestRemoteRead_DualEnd_ListStatDownload(t *testing.T) {
	cfg, aID, bFP, ln := startRemoteReadDualEnd(t)

	body := bytes.Repeat([]byte("remote-read-"), 5000) // 60 000 B
	writeRemoteUserFile(t, cfg, testDualOwner, "docs/a.bin", string(body))
	// 第二份载荷刻意 > 64 KiB tunnel 分块帧上限，钉住多帧流式（而非单帧侥幸）。
	big := bytes.Repeat([]byte("x"), 140_000)
	writeRemoteUserFile(t, cfg, testDualOwner, "docs/big.bin", string(big))

	tun := dialRemoteRead(t, ln, aID, bFP, []string{bFP})

	// --- list ---
	resp := tunDoRemoteRead(t, tun, http.MethodGet, "/remote/list?volume="+testDualVol+"&path=/docs")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list 应 200, got %d body=%s", resp.StatusCode, readTunnelBody(t, resp))
	}
	var lr listResponse
	if err := json.Unmarshal(readTunnelBody(t, resp), &lr); err != nil {
		t.Fatalf("list 响应非法 JSON: %v", err)
	}
	if len(lr.Files) != 2 {
		t.Fatalf("list 应返回 2 个文件, got %+v", lr.Files)
	}
	for _, f := range lr.Files {
		if f.Volume != testDualVol {
			t.Fatalf("文件条目应带卷名 %q: %+v", testDualVol, f)
		}
	}

	// 握手已认证：A 侧看到 B 的身份指纹（非空 = 可作授权输入；空 = 未认证，见契约）。
	if got := tun.PeerFingerprint(); got != bFP {
		t.Fatalf("A 侧握手后应看到 B 的指纹: got %q want %q", got, bFP)
	}

	// --- stat（HEAD）---
	resp = tunDoRemoteRead(t, tun, http.MethodHead, "/remote/stat?volume="+testDualVol+"&path=/docs/a.bin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stat 应 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-File-Size"); got != strconv.Itoa(len(body)) {
		t.Fatalf("X-File-Size 不符: got %q want %d", got, len(body))
	}
	_ = resp.Body.Close()

	// --- download（与 B 磁盘原件逐字节全等）---
	for _, tc := range []struct {
		name string
		rel  string
		want []byte
	}{
		{"小文件", "docs/a.bin", body},
		{"跨分块大文件", "docs/big.bin", big},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subResp := tunDoRemoteRead(t, tun, http.MethodGet,
				"/remote/download?volume="+testDualVol+"&path=/"+tc.rel)
			if subResp.StatusCode != http.StatusOK {
				t.Fatalf("download 应 200, got %d", subResp.StatusCode)
			}
			got := readTunnelBody(t, subResp)
			// 先与 B 磁盘原件比对（同一份字节的两个来源：隧道 vs 文件系统）。
			disk, err := os.ReadFile(filepath.Join(cfg.StorageRoot, testDualOwner, "user", filepath.FromSlash(tc.rel)))
			if err != nil {
				t.Fatalf("读磁盘原件: %v", err)
			}
			if testutil.SHA256Hex(got) != testutil.SHA256Hex(disk) {
				t.Fatalf("隧道下载字节与 B 磁盘原件 SHA-256 不等: got len=%d sha=%s want len=%d sha=%s",
					len(got), testutil.SHA256Hex(got), len(disk), testutil.SHA256Hex(disk))
			}
			// 再与期望载荷比对，防「磁盘原件本身就被写坏」让上一条恒真。
			if testutil.SHA256Hex(got) != testutil.SHA256Hex(tc.want) {
				t.Fatalf("下载内容与期望载荷不符: got len=%d", len(got))
			}
		})
	}

	// --- Range ---
	resp = tunDoRemoteRead(t, tun, http.MethodGet,
		"/remote/download?volume="+testDualVol+"&path=/docs/a.bin",
		func(r *http.Request) { r.Header.Set("Range", "bytes=0-4") })
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range 应 206, got %d", resp.StatusCode)
	}
	if got := readTunnelBody(t, resp); string(got) != string(body[:5]) {
		t.Fatalf("Range 内容不符: %q", got)
	}

	// --- 未授权卷：经真链路也必须 404（授权判定作用于真实握手指纹） ---
	resp = tunDoRemoteRead(t, tun, http.MethodGet, "/remote/list?volume=nope&path=/docs")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未授权卷应 404, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestRemoteRead_DualEnd_WrongPinRejected 反向对照：A pin 一个错误指纹 → A 侧握手
// fail-closed，Do 返回错误且**不返回任何数据**；且此时 PeerFingerprint 必须为空串
// （契约：空串 = 未认证，调用方不得据此授权）。
func TestRemoteRead_DualEnd_WrongPinRejected(t *testing.T) {
	_, aID, bFP, ln := startRemoteReadDualEnd(t)

	wrongID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	tun := dialRemoteRead(t, ln, aID, bFP, []string{wrongID.Fingerprint()})

	req, err := http.NewRequest(http.MethodGet, "/remote/list?volume="+testDualVol+"&path=/docs", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tun.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("错 pin 必须握手失败（fail-closed），却成功返回响应")
	}
	if resp != nil {
		t.Fatalf("握手失败不得返回任何数据: %+v", resp)
	}
	if !errors.Is(err, tunnel.ErrPeerFingerprintMismatch) {
		t.Fatalf("应报 ErrPeerFingerprintMismatch, got %v", err)
	}
	if got := tun.PeerFingerprint(); got != "" {
		t.Fatalf("握手失败后 PeerFingerprint 必须为空串（未认证，不得据此授权）, got %q", got)
	}
}

// TestRemoteRead_DualEnd_UnpinnedPeerRejectedAtHandshake 钉住**双向 pin 的 B 侧一半**：
// 用一个不在任何 mesh_readers 里的身份连入，B 必须在**握手阶段**就拒绝（fail-closed），
// 而不是「握手放行、靠 handler 的 401/404 兜底」。
//
// 为什么必须钉住：tunnel.DeriveRemoteStaticKey 由**公开的 listener 身份指纹**派生，
// 且在 Tunnel 中兼作 dialer 侧握手失败时的回退加密密钥；只有两端都**真的**配置了
// WithPeerFingerprints，「静态密钥回退不可达」才成立。若 listener 漏传 pin，对端握手
// 会成功，本用例将拿到一个正常的 404 响应（resp 非 nil）而非错误，从而变红。
func TestRemoteRead_DualEnd_UnpinnedPeerRejectedAtHandshake(t *testing.T) {
	_, _, bFP, ln := startRemoteReadDualEnd(t)

	strangerID, sErr := tunnel.GenerateIdentity()
	if sErr != nil {
		t.Fatal(sErr)
	}
	// A 侧 pin 正确（B 的身份），但 A 自己的身份不在 B 的 mesh_readers 里。
	tun := dialRemoteRead(t, ln, strangerID, bFP, []string{bFP})

	req, rErr := http.NewRequest(http.MethodGet, "/remote/list?volume="+testDualVol+"&path=/docs", nil)
	if rErr != nil {
		t.Fatal(rErr)
	}
	resp, err := tun.Do(req)
	if err == nil {
		code := resp.StatusCode
		_ = resp.Body.Close()
		t.Fatalf("未被 pin 的对端必须在握手阶段被拒（B 侧 fail-closed），却拿到响应 %d（说明 listener 漏配 WithPeerFingerprints）", code)
	}
	if resp != nil {
		t.Fatalf("握手被拒时不得返回任何数据: %+v", resp)
	}
}

// TestStartRemoteReadListener_DisabledIsNil 钉住零回归：未启用时不起监听、返回 (nil, nil)。
func TestStartRemoteReadListener_DisabledIsNil(t *testing.T) {
	cfg := Default()
	cfg.RemoteRead.Enabled = false
	ln, err := StartRemoteReadListener(t.Context(), cfg, nil, nil)
	if err != nil {
		t.Fatalf("未启用不应报错: %v", err)
	}
	if ln != nil {
		_ = ln.Close()
		t.Fatal("未启用必须返回 nil listener（零回归：不起任何监听）")
	}
}

// TestStartRemoteReadListener_NoPinsRefused 钉住 listener 自身的第二道 fail-closed 门禁
// （纵深防御，绕过 Validate 直接构造 cfg 也拦得住）：无任何 mesh_readers 指纹即拒绝启动。
//
// **不得删除本校验**：tunnel.DeriveRemoteStaticKey 由公开的 listener 身份指纹派生，
// 且该值在 Tunnel 中兼作 dialer 侧握手失败时的回退加密密钥；B 侧「无 pin 不接受任何
// 对端」是「静态密钥回退不可达」这一安全论证的必要条件。删掉它就会留下「无 pin 也能跑」
// 的路径。
func TestStartRemoteReadListener_NoPinsRefused(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "vol-main")
	cfg.LogLevel = "error"
	cfg.Hub.XferIdentityFile = filepath.Join(dir, "b-identity.json")
	cfg.Volumes = []VolumeConfig{{Name: testDualVol, Root: cfg.StorageRoot}}
	cfg.RemoteRead.Enabled = true
	cfg.RemoteRead.Listen = "127.0.0.1:0"
	cfg.SetDefaults()

	// 前提自检：Validate 已拒绝（本用例刻意绕过它，单独钉住 listener 自己的防线）。
	if err := cfg.Validate(); err == nil {
		t.Fatal("前提不成立：Validate 应已拒绝无 mesh_readers 的配置")
	}

	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	ln, err := StartRemoteReadListener(t.Context(), cfg, h, testutil.DiscardLogger())
	if err == nil {
		_ = ln.Close()
		t.Fatal("无 mesh_readers 指纹时 listener 必须拒绝启动（fail-closed，不留无 pin 路径）")
	}
	if ln != nil {
		_ = ln.Close()
		t.Fatalf("拒绝启动时不得返回 listener: %+v", ln)
	}
	if !strings.Contains(err.Error(), "mesh_readers") {
		t.Fatalf("错误信息应指明无 mesh_readers 指纹: %v", err)
	}
}
