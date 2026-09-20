// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// recordingPipe 是 net.Pipe 一端的包装：透传全部字节的同时记录副本，
// 供"中间人 X 只看到密文、看不到明文"断言。
type recordingPipe struct {
	net.Conn
	mu   sync.Mutex
	seen strings.Builder
}

func (r *recordingPipe) Read(b []byte) (int, error) {
	n, err := r.Conn.Read(b)
	if n > 0 {
		r.mu.Lock()
		_, _ = r.seen.Write(b[:n])
		r.mu.Unlock()
	}
	return n, err
}

func (r *recordingPipe) Write(b []byte) (int, error) {
	n, err := r.Conn.Write(b)
	if n > 0 {
		r.mu.Lock()
		_, _ = r.seen.Write(b[:n])
		r.mu.Unlock()
	}
	return n, err
}

func (r *recordingPipe) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen.String()
}

// echoHandler 回显请求体（T 侧端到端解密后的 HTTP handler）。
func echoHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	})
}

// startEchoListener 起 127.0.0.1 TCP 监听（T 的出口服务端），accept 后把 conn
// 交给 ServeE2EListener（解密后 echo）。返回监听器与 serveErr 通道。
func startEchoListener(t *testing.T, ctx context.Context, idT *tunnel.Identity, pinL string) (net.Listener, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo 监听失败: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			serveErr <- fmt.Errorf("T accept 失败: %w", aerr)
			return
		}
		defer c.Close()
		serveErr <- ServeE2EListener(ctx, c, EndToEndOptions{
			Enabled:          true,
			Identity:         idT,
			PeerFingerprints: []string{pinL},
			Handler:          echoHandler(t),
		})
	}()
	return ln, serveErr
}

// startXRelay 起 X 侧中继：读 L 的 dial 帧 → 出口拨号 → 字节 pump。
// 返回 xErr 通道（ServeE2ERelay 的返回值）。
func startXRelay(ctx context.Context, outer net.Conn, dialPolicy func(string) (string, bool)) <-chan error {
	xErr := make(chan error, 1)
	go func() {
		xErr <- ServeE2ERelay(ctx, outer, dialPolicy)
	}()
	return xErr
}

// TestDialE2E_MiddlemanWithoutKeysCantRead 是 T1 核心安全断言：
// 中间人 X 只透传密文，无 L/T 私钥读不到明文；L/T 端到端加密后明文往返一致。
// X 侧跑真实 ServeE2ERelay（密文透传 + 出口拨号），断言 X 通道（两个方向）无明文。
func TestDialE2E_MiddlemanWithoutKeysCantRead(t *testing.T) {
	t.Parallel()
	// 1. 生成 L 与 T 的 Ed25519 身份（端到端密钥与 SK 解耦：身份私钥互不知晓）。
	idL, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成 L 身份失败: %v", err)
	}
	idT, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成 T 身份失败: %v", err)
	}

	// 2. 链路：L --lX/xL(rec 记录)-- X(中继) --TCP出口-- T(echo accept → ServeE2EListener)。
	//    L 侧 DialE2E 建隧道（dialer），X 侧 ServeE2ERelay 密文透传 + 出口拨号到 T，
	//    T 侧 ServeE2EListener 解密后 echo 回显。
	//    X 侧装配（关键）：L 与 X 之间只有一条 net.Pipe（lX/xL）承载「dial 帧 + mux 流」；
	//    X 先裸读 dial 帧（4B 长度前缀 + JSON），再 pump 剩余密文字节到出口。
	lX, xL := net.Pipe()            // L → X：dial 帧 + L 侧 mux（同一条连接）
	rec := &recordingPipe{Conn: xL} // X 视角：记录 X 读到的（来自 L 的）全部字节

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// T 侧：echo accept → ServeE2EListener。
	echoLn, serveErr := startEchoListener(t, ctx, idT, idL.Fingerprint())
	defer echoLn.Close()

	// X 侧：真实 ServeE2ERelay（读 dial 帧 + 出口拨号 + 字节 pump）。
	xErr := startXRelay(ctx, rec, func(addr string) (string, bool) {
		return echoLn.Addr().String(), true
	})

	// L 侧 DialE2E：外层数据面连接 = 到 X 的 lX（经 X 中继到 T）。
	conn, err := DialE2E(ctx, lX, EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: []string{idT.Fingerprint()},
		DialAddr:         echoLn.Addr().String(), // 多跳：写 dial 指令，X 出口拨号到 echo
	})
	if err != nil {
		cancel()
		t.Fatalf("DialE2E 失败: %v", err)
	}
	defer conn.Close()

	// 3. L 发明文 HTTP 请求 → X 密文透传 → T 解密 → echo 回显 → L 读回一致。
	plain := "TOP-SECRET-PAYLOAD"
	resp, err := conn.Do(ctx, "POST", "/echo", plain)
	if err != nil {
		t.Fatalf("端到端请求失败: %v", err)
	}
	if resp != plain {
		t.Fatalf("明文回读不一致: got %q, want %q", resp, plain)
	}

	// 4. 关键断言：中间人 X 只见密文，不见明文。
	//    X 读到的（来自 L 的方向）全部字节记录在 rec——必须不含明文子串。
	if got := rec.snapshot(); strings.Contains(got, plain) {
		t.Fatalf("中间人 X 记录了明文（应只见密文），snapshot=%q", got)
	}

	cancel()
	_ = conn.Close()
	_ = lX.Close()
	_ = xL.Close()
	_ = echoLn.Close()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeE2EListener 未退出")
	}
	select {
	case <-xErr:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeE2ERelay 未退出")
	}
}

// TestDialE2E_PinMismatchFailsClosed 验证指纹 pinning fail-closed：
// L pin 了错误的指纹（非 T 身份）→ 握手失败，T 侧 serveErr 报指纹校验错误。
// 拓扑：L → X(真实 ServeE2ERelay) → T(ServeE2EListener)——L/T 真实连通，
// 握手真实发生，断言放能拿到指纹语义错误的一侧（T 侧 serveErr）。
func TestDialE2E_PinMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	idL, _ := tunnel.GenerateIdentity()
	idT, _ := tunnel.GenerateIdentity()
	evil, _ := tunnel.GenerateIdentity() // 错误指纹

	lX, xL := net.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// T 侧：echo accept → ServeE2EListener（pin L 的真实指纹）。
	echoLn, serveErr := startEchoListener(t, ctx, idT, idL.Fingerprint())
	defer echoLn.Close()

	// X 侧：真实 ServeE2ERelay（dial 帧 → 出口拨号到 echo → 字节 pump）。
	xErr := startXRelay(ctx, xL, func(addr string) (string, bool) {
		return echoLn.Addr().String(), true
	})

	// L 侧 DialE2E：pin 错误指纹（evil）→ 握手 fail-closed。
	conn, err := DialE2E(ctx, lX, EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: []string{evil.Fingerprint()}, // 错误 pin
		DialAddr:         echoLn.Addr().String(),
	})
	if err != nil {
		// DialE2E 写 dial 帧即可返回；握手在 Do 阶段触发——err 可能已含指纹错误。
		if strings.Contains(err.Error(), "指纹") {
			// 错误语义正确（fail-closed）→ 通过；收尾。
			cancel()
			_ = lX.Close()
			_ = xL.Close()
			_ = echoLn.Close()
			select {
			case <-serveErr:
			case <-time.After(2 * time.Second):
				t.Fatal("ServeE2EListener 未退出")
			}
			select {
			case <-xErr:
			case <-time.After(2 * time.Second):
				t.Fatal("ServeE2ERelay 未退出")
			}
			return
		}
		t.Logf("DialE2E 返回（握手尚未触发）: %v", err)
	}
	// 错误语义：pin 不匹配在 **L 侧**（L pin evil，对端 T 是 idT 不匹配）——
	// L 侧 Do 必须失败且含「指纹」语义；T 侧 pin L 真实指纹（idL），L 身份正确，
	// 故 T 侧握手成功（serveErr nil）是**合理**的（T 无法感知 L 的 pin 配置）。
	var doErr error
	if conn != nil {
		defer conn.Close()
		_, doErr = conn.Do(ctx, "POST", "/echo", "hi")
	}
	if doErr == nil {
		t.Fatal("pin 不匹配应 fail-closed（L 侧 Do 必须失败）")
	}
	if !strings.Contains(doErr.Error(), "指纹") {
		t.Fatalf("L 侧应报指纹校验失败，got: %v", doErr)
	}
	cancel()
	_ = lX.Close()
	_ = xL.Close()
	_ = echoLn.Close()
	// T 侧 serveErr 可为 nil（T pin 正确，L 身份正确）；仅等待退出即可。
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeE2EListener 未退出")
	}
	// X 侧 ServeE2ERelay 应自然退出（握手失败后连接关闭）。
	select {
	case <-xErr:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeE2ERelay 未退出")
	}
}

// TestServeE2ERelay_DialPolicyAllowAndDeny 验证 X 侧 ServeE2ERelay 的出口拨号策略：
// 放行 → 密文透传成功（X 只见密文）；拒绝 → 返回错误（不拨号）。
func TestServeE2ERelay_DialPolicyAllowAndDeny(t *testing.T) {
	t.Parallel()
	idL, _ := tunnel.GenerateIdentity()
	idT, _ := tunnel.GenerateIdentity()

	t.Run("allow", func(t *testing.T) {
		t.Parallel()
		// L --net.Pipe-- X(真实 ServeE2ERelay) --TCP出口-- T(echo accept → ServeE2EListener)。
		lX, xL := net.Pipe()
		rec := &recordingPipe{Conn: xL} // X 视角：记录 X 读到的全部字节
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		echoLn, serveErr := startEchoListener(t, ctx, idT, idL.Fingerprint())
		defer echoLn.Close()

		xErr := startXRelay(ctx, rec, func(addr string) (string, bool) {
			return echoLn.Addr().String(), true
		})

		conn, derr := DialE2E(ctx, lX, EndToEndOptions{
			Enabled:          true,
			Identity:         idL,
			PeerFingerprints: []string{idT.Fingerprint()},
			DialAddr:         echoLn.Addr().String(), // 多跳：写 dial 指令，X 出口拨号到 echo
		})
		if derr != nil {
			t.Fatalf("DialE2E 失败: %v", derr)
		}
		defer conn.Close()
		plain := "RELAY-SECRET"
		resp, rerr := conn.Do(ctx, "POST", "/echo", plain)
		if rerr != nil {
			t.Fatalf("经 X 中继的端到端请求失败: %v", rerr)
		}
		if resp != plain {
			t.Fatalf("回读不一致: got %q, want %q", resp, plain)
		}
		if got := rec.snapshot(); strings.Contains(got, plain) {
			t.Fatalf("X 中继通道出现明文（应只见密文），snapshot=%q", got)
		}
		cancel()
		_ = conn.Close()
		_ = lX.Close()
		_ = xL.Close()
		_ = echoLn.Close()
		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeE2EListener 未退出")
		}
		select {
		case <-xErr:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeE2ERelay 未退出")
		}
	})

	t.Run("deny", func(t *testing.T) {
		t.Parallel()
		lX, xL := net.Pipe()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		xErr := startXRelay(ctx, xL, func(addr string) (string, bool) {
			return "", false // dialPolicy 拒绝：任何目标都返回 false
		})

		conn, derr := DialE2E(ctx, lX, EndToEndOptions{
			Enabled:          true,
			Identity:         idL,
			PeerFingerprints: []string{idT.Fingerprint()},
			DialAddr:         "deny-target.invalid:1", // 多跳：写 dial 指令（X 将拒绝）
		})
		if derr != nil {
			t.Fatalf("DialE2E 不应失败（dial 帧写在外层首部）: %v", derr)
		}
		defer conn.Close()
		cancel()
		_ = lX.Close()
		_ = xL.Close()
		select {
		case rxErr := <-xErr:
			if rxErr == nil {
				t.Fatal("dialPolicy 拒绝应返回错误")
			}
			if !strings.Contains(rxErr.Error(), "拨号策略") {
				t.Fatalf("应报拨号策略拒绝，got: %v", rxErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("ServeE2ERelay 未退出")
		}
	})
}

// TestServeE2ERelay_NilDialPolicyFailsClosed 验证 dialPolicy nil fail-closed：
// ServeE2ERelay 返回错误而非 panic。
func TestServeE2ERelay_NilDialPolicyFailsClosed(t *testing.T) {
	t.Parallel()
	lA, lB := net.Pipe()
	defer lA.Close()
	defer lB.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := ServeE2ERelay(ctx, lB, nil); err == nil {
		t.Fatal("nil dialPolicy 应 fail-closed 返回错误")
	}
}

// TestDialE2E_EmptyPinFailsClosed 验证空指纹元素 fail-closed：
// PeerFingerprints 含空元素 → DialE2E 返回错误（不 panic）。
func TestDialE2E_EmptyPinFailsClosed(t *testing.T) {
	t.Parallel()
	idL, _ := tunnel.GenerateIdentity()
	conn, err := DialE2E(context.Background(), nil, EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: []string{""},
	})
	if err == nil {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("空指纹 pin 应 fail-closed 返回错误")
	}
	if !strings.Contains(err.Error(), "空元素") {
		t.Fatalf("应报空元素 fail-closed，got: %v", err)
	}
	// 空 pin 列表同样 fail-closed。
	if _, err := DialE2E(context.Background(), nil, EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: nil,
	}); err == nil || !strings.Contains(err.Error(), "必填") {
		t.Fatalf("空 pin 列表应 fail-closed，got: %v", err)
	}
}
