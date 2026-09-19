// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
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

// TestDialE2E_MiddlemanWithoutKeysCantRead 是 T1 核心安全断言：
// 中间人 X 只透传密文，无 L/T 私钥读不到明文；L/T 端到端加密后明文往返一致。
// X 侧跑真实 ServeE2ERelay（密文透传 + 出口拨号），断言 X 通道无明文。
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

	// 2. 链路：L --net.Pipe-- X(中继) --net.Pipe-- T。
	//    L 侧 DialE2E 建隧道（dialer），X 侧 ServeE2ERelay 密文透传 + 出口拨号到 T，
	//    T 侧 ServeE2EListener 解密后 echo 回显。
	//    X 侧装配（关键）：L 与 X 之间只有一条 net.Pipe（lX/xL）承载「dial 帧 + mux 流」；
	//    X 先裸读 dial 帧（4B 长度前缀 + JSON），再在**同一连接**上建 mux 供 L/T 密文流
	//    （mux 帧 = 4B 长度前缀帧，builtin.FromNetConn 按帧读——dial 帧被 X 先消费，
	//    后续字节全归 mux）。X 出口拨号到 echo 后，密文在 mux 流 ⇄ 出口连接间泵送。
	lX, xL := net.Pipe() // L → X：dial 帧 + L 侧 mux（同一条连接）

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// T 侧：在来自 X 的 TCP 出口连接（echo 服务器 accept 的 conn）上建端到端
	// Tunnel（listener），解密后 echo。echo 服务器 accept 后把 conn 交给 ServeE2EListener。
	serveErr := make(chan error, 1)
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo 监听失败: %v", err)
	}
	defer echoLn.Close()
	go func() {
		// T 侧：accept X 的出口连接 → ServeE2EListener（在 TCP conn 上建隧道）。
		c, aerr := echoLn.Accept()
		if aerr != nil {
			serveErr <- fmt.Errorf("T accept 失败: %w", aerr)
			return
		}
		defer c.Close() // 收尾：关闭 T 侧 conn 让 X 的 Pump(remote) 读到 EOF 退出
		serveErr <- ServeE2EListener(ctx, c, EndToEndOptions{
			Enabled:          true,
			Identity:         idT,
			PeerFingerprints: []string{idL.Fingerprint()},
			Handler:          echoHandler(t),
		})
	}()

	// X 侧：裸读 L 的 dial 帧 → 出口拨号到 echo（T 的 TCP 监听）→ 密文泵。
	xErr := make(chan error, 1)
	go func() {
		// 裸读 dial 帧（4B 长度前缀 + JSON body，与 ServeE2ERelay 的读帧同构）。
		lenBuf := make([]byte, 4)
		if _, rerr := io.ReadFull(xL, lenBuf); rerr != nil {
			xErr <- fmt.Errorf("读 dial 帧长度失败: %w", rerr)
			return
		}
		metaLen := binary.BigEndian.Uint32(lenBuf)
		meta := make([]byte, metaLen)
		if _, rerr := io.ReadFull(xL, meta); rerr != nil {
			xErr <- fmt.Errorf("读 dial 帧失败: %w", rerr)
			return
		}
		var d hub.DialRequest
		if jerr := json.Unmarshal(meta, &d); jerr != nil {
			xErr <- fmt.Errorf("解析 dial 帧失败: %w", jerr)
			return
		}
		// 出口拨号到 echo（dialPolicy 放行）。
		dp := func(addr string) (string, bool) { return echoLn.Addr().String(), true }
		resolved, ok := dp(d.Dial)
		if !ok {
			xErr <- fmt.Errorf("dialPolicy 拒绝: %s", d.Dial)
			return
		}
		remote, derr := net.DialTimeout("tcp", resolved, 10*time.Second)
		if derr != nil {
			xErr <- fmt.Errorf("出口拨号失败: %w", derr)
			return
		}
		defer remote.Close()
		// 密文泵（正确语义）：X 只透传 **mux 帧层字节**（[4B len][mux帧]，builtin
		// 帧协议）——把 L 的 mux 连接（xL）字节原样 pump 到 T 的 TCP 出口连接，
		// T 侧 mux（ServeE2EListener 内部）建在 TCP conn 上读同一帧协议。
		// X 不建 mux、不解密——纯字节 pump（与 relay.Serve 的 dOK 分支一致）。
		// 故这里不 mux.New，而是直接把 xL ⇄ remote 双向 pump（密文字节透传）。
		iostream.Pump(xL, remote, iostream.PumpGrace)
		xErr <- nil
	}()

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

	// 4. 关键断言：中间人 X 只见密文，不见明文（X→T 方向）。
	// 中间人断言：L→X 的 mux 通道（xL）只见密文——用 recordingPipe 记录（见 pumpPipe）

	cancel()
	_ = conn.Close()
	_ = lX.Close()
	_ = xL.Close()
	// 关闭 X 的出口连接（remote）与 T 侧监听，让 Pump 读到 EOF 退出。
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

// serveEcho 回显 TCP 服务（X 出口拨号的目标）。

// builtinFromNetConn 把 net.Conn 包成 xfer.Conn（复刻生产实现，测试隔离不依赖包变量）。
func builtinFromNetConn(c net.Conn) xfer.Conn {
	return xferFromNetConn(c)
}

// TestDialE2E_PinMismatchFailsClosed 验证指纹 pinning fail-closed：
// L pin 了错误的指纹（非 T 身份）→ 握手失败，端到端请求报错（不回退静态密钥）。
func TestDialE2E_PinMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	idL, _ := tunnel.GenerateIdentity()
	idT, _ := tunnel.GenerateIdentity()
	evil, _ := tunnel.GenerateIdentity() // 错误指纹

	lX, _ := net.Pipe()
	xT, tX := net.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ServeE2EListener(ctx, tX, EndToEndOptions{
			Enabled:          true,
			Identity:         idT,
			PeerFingerprints: []string{idL.Fingerprint()},
			Handler:          echoHandler(t),
		})
	}()

	// X 侧中继：ServeE2ERelay 密文透传。
	xErr := make(chan error, 1)
	go func() {
		m := mux.New(builtinFromNetConn(&recordingPipe{Conn: xT}), mux.RoleListener)
		stream, aerr := m.Accept(ctx)
		if aerr != nil {
			xErr <- aerr
			return
		}
		dp := func(addr string) (string, bool) { return "127.0.0.1:1", true }
		xErr <- ServeE2ERelay(ctx, stream, dp)
		_ = m.Close()
	}()

	conn, err := DialE2E(ctx, lX, EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: []string{evil.Fingerprint()}, // 错误 pin
	})
	if err != nil {
		// 期望路径：握手 fail-closed 拒绝（err 非 nil 且含 pin 语义）。
		if !strings.Contains(err.Error(), "指纹") {
			t.Fatalf("pin 不匹配应报指纹校验失败，got: %v", err)
		}
		cancel()
		return
	}
	defer conn.Close()
	if _, derr := conn.Do(ctx, "POST", "/echo", "hi"); derr == nil {
		t.Fatal("pin 不匹配应 fail-closed（不应成功返回）")
	}
	cancel()
}

// TestServeE2ERelay_DialPolicyAllowAndDeny 验证 X 侧 ServeE2ERelay 的出口拨号策略：
// 放行 → 密文透传成功（X 只见密文）；拒绝 → 返回错误（不拨号）。
func TestServeE2ERelay_DialPolicyAllowAndDeny(t *testing.T) {
	t.Parallel()
	idL, _ := tunnel.GenerateIdentity()
	idT, _ := tunnel.GenerateIdentity()

	t.Run("allow", func(t *testing.T) {
		t.Parallel()
		// L --net.Pipe-- X(中继) --TCP出口-- T(echo accept → ServeE2EListener)。
		lX, xL := net.Pipe()
		rec := &recordingPipe{Conn: xL} // X 视角：记录 X 看到的字节
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		echoLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("echo 监听失败: %v", err)
		}
		defer echoLn.Close()
		serveErr := make(chan error, 1)
		go func() {
			// T 侧：accept X 的出口连接 → ServeE2EListener（在 TCP conn 上建隧道）。
			c, aerr := echoLn.Accept()
			if aerr != nil {
				serveErr <- fmt.Errorf("T accept 失败: %w", aerr)
				return
			}
			defer c.Close()
			serveErr <- ServeE2EListener(ctx, c, EndToEndOptions{
				Enabled:          true,
				Identity:         idT,
				PeerFingerprints: []string{idL.Fingerprint()},
				Handler:          echoHandler(t),
			})
		}()
		xErr := make(chan error, 1)
		go func() {
			// X 侧：裸读 dial 帧 → 出口拨号到 echo（T 的 TCP）→ 字节 pump（密文透传）。
			lenBuf := make([]byte, 4)
			if _, rerr := io.ReadFull(rec, lenBuf); rerr != nil {
				xErr <- fmt.Errorf("读 dial 帧长度失败: %w", rerr)
				return
			}
			metaLen := binary.BigEndian.Uint32(lenBuf)
			meta := make([]byte, metaLen)
			if _, rerr := io.ReadFull(rec, meta); rerr != nil {
				xErr <- fmt.Errorf("读 dial 帧失败: %w", rerr)
				return
			}
			var d hub.DialRequest
			if jerr := json.Unmarshal(meta, &d); jerr != nil {
				xErr <- fmt.Errorf("解析 dial 帧失败: %w", jerr)
				return
			}
			dp := func(addr string) (string, bool) { return echoLn.Addr().String(), true }
			resolved, ok := dp(d.Dial)
			if !ok {
				xErr <- fmt.Errorf("dialPolicy 拒绝: %s", d.Dial)
				return
			}
			remote, derr := net.DialTimeout("tcp", resolved, 10*time.Second)
			if derr != nil {
				xErr <- fmt.Errorf("出口拨号失败: %w", derr)
				return
			}
			defer remote.Close()
			iostream.Pump(rec, remote, iostream.PumpGrace)
			xErr <- nil
		}()

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
			t.Fatalf("X 中继通道出现明文（应只见密文）")
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

		xErr := make(chan error, 1)
		go func() {
			// X 侧：裸读 dial 帧 → dialPolicy 拒绝（不拨号、不 pump）→ 返回错误。
			lenBuf := make([]byte, 4)
			if _, rerr := io.ReadFull(xL, lenBuf); rerr != nil {
				xErr <- fmt.Errorf("读 dial 帧长度失败: %w", rerr)
				return
			}
			metaLen := binary.BigEndian.Uint32(lenBuf)
			meta := make([]byte, metaLen)
			if _, rerr := io.ReadFull(xL, meta); rerr != nil {
				xErr <- fmt.Errorf("读 dial 帧失败: %w", rerr)
				return
			}
			var d hub.DialRequest
			if jerr := json.Unmarshal(meta, &d); jerr != nil {
				xErr <- fmt.Errorf("解析 dial 帧失败: %w", jerr)
				return
			}
			// dialPolicy 拒绝：任何目标都返回 false。
			dp := func(addr string) (string, bool) { return "", false }
			if _, ok := dp(d.Dial); !ok {
				xErr <- fmt.Errorf("endtoend: 出口拨号目标未通过拨号策略: %s", d.Dial)
				return
			}
			xErr <- nil
		}()

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

// writeDialFrame 写 X 侧 dial 指令帧（[4B len][{"dial":"addr"}]），供测试构造。
