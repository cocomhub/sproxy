// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
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

// pumpPipe 把两侧字节互相原样透传（纯字节 pump，不解析不解密）。
// 任一侧 EOF/关闭后关闭另一侧并 close(done)。
func pumpPipe(a, b net.Conn, done chan<- struct{}) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); _ = b.Close() }()
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); _ = a.Close() }()
	wg.Wait()
	close(done)
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

	// 2. 链路：L --lA/lB-- X --tA/tB-- T。
	//    X 夹在中间：两侧各一条 net.Pipe，X 做纯字节 pump（recordingPipe 记录 X 看到的字节）。
	lA, lB := net.Pipe()
	tA, tB := net.Pipe()
	xToT := &recordingPipe{Conn: lB} // X 视角：来自 L 的方向（经 pump 到 T）
	tFromX := &recordingPipe{Conn: tA}
	xDone := make(chan struct{})
	go pumpPipe(xToT, tFromX, xDone) // X 纯字节透传（不接触明文）

	// 3. T 侧 ServeE2E（listener 角色）：在来自 X 的 net.Pipe 上建端到端 Tunnel，
	//    解密后交给 echo handler（HTTP 请求体原样回显）。
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ServeE2E(ctx, tB, EndToEndOptions{
			Enabled:          true,
			Identity:         idT,
			PeerFingerprints: []string{idL.Fingerprint()},
			StaticKey:        tunnel.DeriveRemoteStaticKey(idT.Fingerprint()),
			Handler:          echoHandler(t),
		})
	}()

	// 4. L 侧 DialE2E（dialer 角色）：在对外层数据面（net.Pipe 另一端）建端到端 Tunnel。
	conn, err := DialE2E(ctx, lA, EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: []string{idT.Fingerprint()},
		StaticKey:        tunnel.DeriveRemoteStaticKey(idL.Fingerprint()),
	})
	if err != nil {
		cancel()
		t.Fatalf("DialE2E 失败: %v", err)
	}
	defer conn.Close()

	// 5. L 发明文 HTTP 请求 → T 端解密 → echo 回显 → L 读回一致。
	plain := "TOP-SECRET-PAYLOAD"
	resp, err := conn.Do(ctx, "POST", "/echo", plain)
	if err != nil {
		t.Fatalf("端到端请求失败: %v", err)
	}
	if resp != plain {
		t.Fatalf("明文回读不一致: got %q, want %q", resp, plain)
	}

	// 6. 关键断言：中间人 X 只见密文，不见明文。
	if got := xToT.snapshot(); strings.Contains(got, plain) {
		t.Fatalf("中间人 X 记录了明文（应只见密文）")
	}
	if got := tFromX.snapshot(); strings.Contains(got, plain) {
		t.Fatalf("中间人 X 记录了明文（应只见密文）")
	}

	cancel()
	_ = conn.Close()
	// 关闭底层 net.Pipe：pump 两侧 io.Copy 读到 EOF/关闭后各自退出。
	_ = lA.Close()
	_ = lB.Close()
	_ = tA.Close()
	_ = tB.Close()
	select {
	case <-xDone:
	case <-time.After(2 * time.Second):
		t.Fatal("X pump 未退出")
	}
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeE2E 未退出")
	}
}

// TestDialE2E_PinMismatchFailsClosed 验证指纹 pinning fail-closed：
// L pin 了错误的指纹（非 T 身份）→ 握手失败，端到端请求报错（不回退静态密钥）。
func TestDialE2E_PinMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	idL, _ := tunnel.GenerateIdentity()
	idT, _ := tunnel.GenerateIdentity()
	evil, _ := tunnel.GenerateIdentity() // 错误指纹

	lA, lB := net.Pipe()
	tA, tB := net.Pipe()
	xDone := make(chan struct{})
	go pumpPipe(&recordingPipe{Conn: lB}, &recordingPipe{Conn: tA}, xDone)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ServeE2E(ctx, tB, EndToEndOptions{
			Enabled:          true,
			Identity:         idT,
			PeerFingerprints: []string{idL.Fingerprint()},
			StaticKey:        tunnel.DeriveRemoteStaticKey(idT.Fingerprint()),
			Handler:          echoHandler(t),
		})
	}()

	conn, err := DialE2E(ctx, lA, EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: []string{evil.Fingerprint()}, // 错误 pin
		StaticKey:        tunnel.DeriveRemoteStaticKey(idL.Fingerprint()),
	})
	if err == nil {
		defer conn.Close()
		if _, derr := conn.Do(ctx, "POST", "/echo", "hi"); derr == nil {
			t.Fatal("pin 不匹配应 fail-closed（不应成功返回）")
		}
	}
	cancel()
}
