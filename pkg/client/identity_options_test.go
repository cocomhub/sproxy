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

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestFileClient_IdentityAndPeerFingerprints 验证 WithIdentity / WithPeerFingerprints
// 选项应用到 FileClient，且默认（未配置）时保持零值（现状兼容）。
func TestFileClient_IdentityAndPeerFingerprints(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
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
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
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
	name := fmt.Sprintf("pipepin-%s", t.Name())
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
	t.Cleanup(func() { xfer.TransportRegistry.Delete(name) })
	return name
}

// TestFileClient_XferTunnel_RetrySurvivesPeerTestCleanup 回归：CI run 34949138918（SonarQube job）
// 实证的 pkg/client flake——TestGetTunnelMux 的 t.Cleanup **当时**调用
// xfer.TransportRegistry.Clear() 清空**进程级全局**注册表（现已在本次修复中改为只删自己注册的键）；
// 与它并发运行的 TestFileClient_XferTunnel_HandshakeFailure_RetryRebuildsMux 若正处在两次
// TunnelDo 之间，第二次的 xfer.Get 得到 nil ⇒ getTunnelMux 直接返回「xfer 传输层 ... 未注册」
// 而不 Dial ⇒ 断言「Dial=2 实际 1」（该次 run 的 pkg/client 全量日志里只有 2 条拨号侧握手失败
// ERROR，恰好对应 PinMismatch 与本次重试的第一次调用——即第二次调用根本没发起握手，排除
// 「重名/复用」两类猜测）。
//
// 本用例把该交错**确定性**展开：第一次 TunnelDo 失败后，模拟「另一个并行测试执行自己的
// cleanup」——注册一个独立的 peer 键后只删掉它自己那个键，再断言重试仍会重新 Dial、且本测试
// 的注册项未被波及。之所以能确定性复现：xfer.Get 返回 nil 时 getTunnelMux 在拨号前就返回
// 「未注册」错误，与调度无关。
func TestFileClient_XferTunnel_RetrySurvivesPeerTestCleanup(t *testing.T) {
	t.Parallel()
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
	if _, err := c.TunnelDo(req1); err == nil {
		t.Fatal("第一次 TunnelDo 应因 pin 不匹配失败")
	}
	if dialCount.Load() != 1 {
		t.Fatalf("第一次后 Dial 次数应为 1, 实际 %d", dialCount.Load())
	}

	// 并发运行的 TestGetTunnelMux 此时结束并执行自己的 t.Cleanup：它只应移除自己注册的键，
	// 不得动本测试的注册项。这里用「另一个测试自己注册、自己清理」的 peer 键把该动作展开，
	// 避免反过来真去删别的测试正在用的键（那与被修的 bug 同类，只是窗口更窄）。
	peerName, unregisterPeer := registerPeerPipeTestTransport(t)
	if xfer.Get(peerName) == nil {
		t.Fatalf("peer 传输层应已注册: %q", peerName)
	}
	unregisterPeer()
	if xfer.Get(peerName) != nil {
		t.Fatalf("peer 键应已被它自己的 cleanup 移除: %q", peerName)
	}
	if xfer.Get(name) == nil {
		t.Fatal("其它测试的 cleanup 不得移除本测试注册的传输层")
	}

	// 第二次：仍应重新建立 mux（Dial=2）。
	req2, _ := http.NewRequest("POST", "/echo", strings.NewReader("x"))
	if _, err := c.TunnelDo(req2); err == nil {
		t.Fatal("第二次 TunnelDo 应因 pin 不匹配失败")
	}
	if dialCount.Load() != 2 {
		t.Fatalf("其它测试的 cleanup 不得影响本测试重试重建 mux（Dial=2）, 实际 %d", dialCount.Load())
	}
}

// registerPeerPipeTestTransport 注册一个「另一个并行测试」风格的传输层，返回名字与
// 只删自己那个键的 cleanup——用来模拟「别的测试执行自己的 cleanup」这一动作，
// 而无需真去删别的测试正在用的键。
func registerPeerPipeTestTransport(t *testing.T) (string, func()) {
	t.Helper()
	name := "pipe-test-peer-" + t.Name()
	xfer.Register(&xfer.Transport{
		Name: name,
		Dial: func(context.Context, string) (xfer.Conn, error) {
			return nil, errors.New("peer 传输层不应被拨号")
		},
	})
	return name, func() { xfer.TransportRegistry.Delete(name) }
}

// TestPipeXferTransportNames_DerivedFromTestIdentity 回归：测试传输层名字此前由
// time.Now().UnixNano() 派生。实测（本机 Windows）时钟粒度约 0.5ms——200000 次连续调用中
// 199998 次取到同一个值——同一毫秒内并行启动的测试因此会拿到**同名**传输层；plugin.Registry
// 的「同名后注册覆盖前注册」语义使先注册者拨号到另一个测试的服务端，表现为 pin 校验失败
// （实测 TestFileClient_XferTunnel_Success_ReuseMux_NoRehandshake 报「期望 [自己的指纹],
// 实际 [别人的指纹]」）。临时探针在 -race -count=20 的一轮里直接抓到三次同名覆盖。
// 名字必须由测试身份派生：含 t.Name() 才能保证不同测试天然不同名。
func TestPipeXferTransportNames_DerivedFromTestIdentity(t *testing.T) {
	t.Parallel()
	idServer, _ := tunnel.GenerateIdentity()
	const hexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	pipeName := registerPipeXfer(t, idServer, hexKey)
	countName, _ := registerCountingPipeXfer(t, idServer, hexKey)
	for _, got := range []string{pipeName, countName} {
		if !strings.Contains(got, t.Name()) {
			t.Fatalf("传输层名字必须含测试身份（t.Name()=%q），否则并行测试会同名并相互覆盖: %q", t.Name(), got)
		}
	}
	if pipeName == countName {
		t.Fatalf("两种测试传输层 helper 的名字不得相同: %q", pipeName)
	}
}

// TestFileClient_XferTunnel_PinMismatch 端到端验证 H-1 接线：
// FileClient WithXfer + WithIdentity + WithPeerFingerprints(错误指纹) 时，
// TunnelDo 走 xfer/mux 握手，pin 不匹配 fail-closed 拒绝。
func TestFileClient_XferTunnel_PinMismatch(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
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
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
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
	name := fmt.Sprintf("pipepin-count-%s", t.Name())
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
	t.Cleanup(func() { xfer.TransportRegistry.Delete(name) })
	return name, &dialCount
}

// TestFileClient_XferTunnel_HandshakeFailure_RetryRebuildsMux 验证 N-1：
// 握手失败（fail-closed pin 校验）后，同一 FileClient 重试会重新建立 mux（再次 Dial），
// 而非复用残留 mux——残留 mux 已处于协议错位状态，复用会对已完成握手的服务端
// 发起第二次握手导致协议混淆。
func TestFileClient_XferTunnel_HandshakeFailure_RetryRebuildsMux(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
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
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
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
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
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
