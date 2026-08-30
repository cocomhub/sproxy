// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestFileClient_IdentityAndPeerFingerprints 验证 WithIdentity / WithPeerFingerprints
// 选项应用到 FileClient，且默认（未配置）时保持零值（现状兼容）。
func TestFileClient_IdentityAndPeerFingerprints(t *testing.T) {
	id, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	c := NewFileClient("https://127.0.0.1:18083",
		WithIdentity(id),
		WithPeerFingerprints([]string{id.Fingerprint()}))
	if c.identity != id {
		t.Error("WithIdentity 未生效")
	}
	if len(c.peerFingerprints) != 1 || c.peerFingerprints[0] != id.Fingerprint() {
		t.Errorf("WithPeerFingerprints 未生效: %v", c.peerFingerprints)
	}

	// 未配置时保持零值（行为与现状完全一致）。
	c2 := NewFileClient("https://127.0.0.1:18083")
	if c2.identity != nil {
		t.Error("默认 identity 应为 nil")
	}
	if len(c2.peerFingerprints) != 0 {
		t.Error("默认 peerFingerprints 应为空")
	}
}

// TestFileClient_TunnelOptsNil 验证未配置身份/pin 时 tunnelOpts 为空切片，
// NewTunnel 行为与旧签名完全一致。
func TestFileClient_TunnelOptsNil(t *testing.T) {
	c := NewFileClient("https://127.0.0.1:18083")
	opts := c.tunnelOpts()
	if len(opts) != 0 {
		t.Fatalf("tunnelOpts 应为空, got %d", len(opts))
	}
}

// registerPipeXfer 注册一个测试 xfer 传输：Dial 返回内存 pipe 的一侧，
// 另一侧在 goroutine 中作为 mux listener 运行带身份的 NewTunnel（真实执行 performHandshake）。
// 返回传输层名字，供 WithXfer 使用。
func registerPipeXfer(t *testing.T, idServer *tunnel.Identity, hexKey string) string {
	t.Helper()
	name := fmt.Sprintf("pipepin-%d", time.Now().UnixNano())
	key, err := tunnel.ParseKey(hexKey)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	xfer.Register(&xfer.Transport{
		Name: name,
		Dial: func(ctx context.Context, _ string) (xfer.Conn, error) {
			a, b := xfertest.Pipe()
			go func() {
				m := mux.New(b, mux.RoleListener)
				defer m.Close()
				tun := tunnel.NewTunnel(m, key, tunnel.WithIdentity(idServer))
				_ = tun.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(w, r.Body)
				}))
			}()
			return a, nil
		},
	})
	return name
}

// TestFileClient_XferTunnel_PinMismatch 端到端验证 H-1 接线：
// FileClient WithXfer + WithIdentity + WithPeerFingerprints(错误指纹) 时，
// TunnelDo 走 xfer/mux 握手，pin 不匹配 fail-closed 拒绝。
func TestFileClient_XferTunnel_PinMismatch(t *testing.T) {
	idServer, _ := tunnel.GenerateIdentity()
	idClient, _ := tunnel.GenerateIdentity()
	wrong, _ := tunnel.GenerateIdentity()
	const hexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	name := registerPipeXfer(t, idServer, hexKey)
	c := NewFileClient("https://127.0.0.1:18083",
		WithXfer(name, "hub://test", hexKey),
		WithIdentity(idClient),
		WithPeerFingerprints([]string{wrong.Fingerprint()}))

	req, _ := http.NewRequest("POST", "/echo", strings.NewReader("x"))
	_, err := c.TunnelDo(req)
	if err == nil {
		t.Fatal("expected pin mismatch to fail TunnelDo")
	}
	if !errors.Is(err, tunnel.ErrPeerFingerprintMismatch) {
		t.Fatalf("expected ErrPeerFingerprintMismatch, got %v", err)
	}
}

// TestFileClient_XferTunnel_PinMatch 端到端验证 H-1 接线：pin 匹配时隧道 HTTP 往返成功。
func TestFileClient_XferTunnel_PinMatch(t *testing.T) {
	idServer, _ := tunnel.GenerateIdentity()
	idClient, _ := tunnel.GenerateIdentity()
	const hexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	name := registerPipeXfer(t, idServer, hexKey)
	c := NewFileClient("https://127.0.0.1:18083",
		WithXfer(name, "hub://test", hexKey),
		WithIdentity(idClient),
		WithPeerFingerprints([]string{idServer.Fingerprint()}))

	req, _ := http.NewRequest("POST", "/echo", strings.NewReader("ping-pinning"))
	resp, err := c.TunnelDo(req)
	if err != nil {
		t.Fatalf("TunnelDo: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ping-pinning" {
		t.Fatalf("body mismatch: %q", body)
	}
}

// registerCountingPipeXfer 注册一个测试 xfer 传输，统计 Dial 次数（N-1 复用/重建判定用）。
func registerCountingPipeXfer(t *testing.T, idServer *tunnel.Identity, hexKey string) (string, *atomic.Int32) {
	t.Helper()
	name := fmt.Sprintf("pipepin-count-%d", time.Now().UnixNano())
	key, err := tunnel.ParseKey(hexKey)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	var dialCount atomic.Int32
	xfer.Register(&xfer.Transport{
		Name: name,
		Dial: func(ctx context.Context, _ string) (xfer.Conn, error) {
			dialCount.Add(1)
			a, b := xfertest.Pipe()
			go func() {
				m := mux.New(b, mux.RoleListener)
				defer m.Close()
				tun := tunnel.NewTunnel(m, key, tunnel.WithIdentity(idServer))
				_ = tun.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(w, r.Body)
				}))
			}()
			return a, nil
		},
	})
	return name, &dialCount
}

// TestFileClient_XferTunnel_HandshakeFailure_RetryRebuildsMux 验证 N-1：
// 握手失败（fail-closed pin 校验）后，同一 FileClient 重试会重新建立 mux（再次 Dial），
// 而非复用残留 mux——残留 mux 已处于协议错位状态，复用会对已完成握手的服务端
// 发起第二次握手导致协议混淆。
func TestFileClient_XferTunnel_HandshakeFailure_RetryRebuildsMux(t *testing.T) {
	idServer, _ := tunnel.GenerateIdentity()
	idClient, _ := tunnel.GenerateIdentity()
	wrong, _ := tunnel.GenerateIdentity()
	const hexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	name, dialCount := registerCountingPipeXfer(t, idServer, hexKey)
	c := NewFileClient("https://127.0.0.1:18083",
		WithXfer(name, "hub://test", hexKey),
		WithIdentity(idClient),
		WithPeerFingerprints([]string{wrong.Fingerprint()}))

	// 第一次：pin 不匹配 → 握手失败，建立 mux（Dial=1）。
	req1, _ := http.NewRequest("POST", "/echo", strings.NewReader("x"))
	_, err1 := c.TunnelDo(req1)
	if err1 == nil {
		t.Fatal("第一次 TunnelDo 应因 pin 不匹配失败")
	}
	if dialCount.Load() != 1 {
		t.Fatalf("第一次后 Dial 次数应为 1, 实际 %d", dialCount.Load())
	}

	// 第二次：必须重新建立 mux（Dial=2），而非复用残留 mux。
	req2, _ := http.NewRequest("POST", "/echo", strings.NewReader("x"))
	_, err2 := c.TunnelDo(req2)
	if err2 == nil {
		t.Fatal("第二次 TunnelDo 应因 pin 不匹配失败")
	}
	if dialCount.Load() != 2 {
		t.Fatalf("握手失败重试应重新建立 mux（Dial=2）, 实际 %d", dialCount.Load())
	}
	if !errors.Is(err2, tunnel.ErrPeerFingerprintMismatch) {
		t.Fatalf("第二次应仍为 pin 不匹配, 实际 %v", err2)
	}
}

// TestFileClient_XferTunnel_Success_ReuseMux_NoRehandshake 验证 N-1 的另一半：
// 握手成功后同一 FileClient 多次请求复用同一 mux（Dial 保持 1），不发起第二次握手。
func TestFileClient_XferTunnel_Success_ReuseMux_NoRehandshake(t *testing.T) {
	idServer, _ := tunnel.GenerateIdentity()
	idClient, _ := tunnel.GenerateIdentity()
	const hexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	name, dialCount := registerCountingPipeXfer(t, idServer, hexKey)
	c := NewFileClient("https://127.0.0.1:18083",
		WithXfer(name, "hub://test", hexKey),
		WithIdentity(idClient),
		WithPeerFingerprints([]string{idServer.Fingerprint()}))

	for i := range 3 {
		req, _ := http.NewRequest("POST", "/echo", strings.NewReader(fmt.Sprintf("req-%d", i)))
		resp, err := c.TunnelDo(req)
		if err != nil {
			t.Fatalf("第 %d 次 TunnelDo: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := fmt.Sprintf("req-%d", i)
		if string(body) != want {
			t.Fatalf("第 %d 次 body mismatch: 期望 %q, 实际 %q", i, want, string(body))
		}
	}
	if dialCount.Load() != 1 {
		t.Fatalf("握手成功后应复用同一 mux（Dial=1）, 实际 %d", dialCount.Load())
	}
}

// TestFileClient_XferTunnel_NoPin 端到端验证：未配置 pin 时 xfer 隧道正常（向后兼容）。
func TestFileClient_XferTunnel_NoPin(t *testing.T) {
	idServer, _ := tunnel.GenerateIdentity()
	const hexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	name := registerPipeXfer(t, idServer, hexKey)
	c := NewFileClient("https://127.0.0.1:18083",
		WithXfer(name, "hub://test", hexKey),
		WithIdentity(nil))

	req, _ := http.NewRequest("POST", "/echo", strings.NewReader("no-pin"))
	resp, err := c.TunnelDo(req)
	if err != nil {
		t.Fatalf("TunnelDo: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "no-pin" {
		t.Fatalf("body mismatch: %q", body)
	}
}
