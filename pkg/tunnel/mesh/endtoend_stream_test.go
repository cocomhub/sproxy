// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestE2EStream_MiddlemanWithoutKeysCantRead 是字节流形态 T1 核心安全断言：
// 中间人 X 只透传密文，无 L/T 私钥读不到明文；L/T 端到端加密字节流往返一致。
// 链路：L --rec(记录)--> X(ServeE2ERelayStream 密文泵) --TCP出口--> T(ServeE2EStream 解密 echo)。
func TestE2EStream_MiddlemanWithoutKeysCantRead(t *testing.T) {
	t.Parallel()
	idL, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成 L 身份失败: %v", err)
	}
	idT, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成 T 身份失败: %v", err)
	}

	lX, xL := net.Pipe()
	rec := &recordingPipe{Conn: xL} // X 视角：记录 X 读到的全部字节

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// T 侧：echo accept → ServeE2EStream（解密后回显明文）。
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo 监听失败: %v", err)
	}
	defer echoLn.Close()
	serveErr := make(chan error, 1)
	go func() {
		c, aerr := echoLn.Accept()
		if aerr != nil {
			serveErr <- aerr
			return
		}
		defer c.Close()
		dec, derr := ServeE2EStream(ctx, c, EndToEndOptions{
			Enabled:          true,
			Identity:         idT,
			PeerFingerprints: []string{idL.Fingerprint()},
		})
		if derr != nil {
			serveErr <- derr
			return
		}
		defer dec.Close()
		// 回显：读全部明文 → 写回。
		buf := make([]byte, 4096)
		n, rerr := dec.Read(buf)
		if rerr != nil && rerr != io.EOF {
			serveErr <- rerr
			return
		}
		if _, werr := dec.Write(buf[:n]); werr != nil {
			serveErr <- werr
			return
		}
		serveErr <- nil
	}()

	// X 侧：真实 ServeE2ERelay（读 dial 帧 + 出口拨号到 echo + 泵密文）。
	xErr := startXRelayStream(ctx, rec, func(addr string) (string, bool) {
		return echoLn.Addr().String(), true
	})

	// L 侧：DialE2EStream（写 e2e dial 帧 + ECDH 握手 + 加密字节流）。
	conn, derr := DialE2EStream(ctx, lX, echoLn.Addr().String(), "", EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: []string{idT.Fingerprint()},
	})
	if derr != nil {
		cancel()
		t.Fatalf("DialE2EStream 失败: %v", derr)
	}
	defer conn.Close()

	// 明文往返。
	plain := "TOP-SECRET-STREAM"
	if _, werr := conn.Write([]byte(plain)); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	buf := make([]byte, len(plain))
	if _, rerr := io.ReadFull(conn, buf); rerr != nil {
		t.Fatalf("读失败: %v", rerr)
	}
	if string(buf) != plain {
		t.Fatalf("明文回读不一致: got %q, want %q", buf, plain)
	}

	// 关键断言：中间人 X 只见密文，不见明文。
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
		t.Fatal("ServeE2EStream 未退出")
	}
	select {
	case <-xErr:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeE2ERelay 未退出")
	}
}

// TestE2EStream_PinMismatchFailsClosed 验证字节流形态 pinning fail-closed：
// L pin 了错误指纹（非 T 身份）→ 握手失败（L 侧 Write/Read 触发，报指纹语义错误）。
func TestE2EStream_PinMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	idL, _ := tunnel.GenerateIdentity()
	idT, _ := tunnel.GenerateIdentity()
	evil, _ := tunnel.GenerateIdentity() // 错误指纹

	lX, xL := net.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo 监听失败: %v", err)
	}
	defer echoLn.Close()
	serveErr := make(chan error, 1)
	go func() {
		c, aerr := echoLn.Accept()
		if aerr != nil {
			serveErr <- aerr
			return
		}
		defer c.Close()
		// T pin L 真实指纹：握手正常；错误发生在 L 侧（L pin evil）。
		_, derr := ServeE2EStream(ctx, c, EndToEndOptions{
			Enabled:          true,
			Identity:         idT,
			PeerFingerprints: []string{idL.Fingerprint()},
		})
		serveErr <- derr
	}()

	xErr := startXRelayStream(ctx, xL, func(addr string) (string, bool) {
		return echoLn.Addr().String(), true
	})

	// 握手在 DialE2EStream 内**同步完成**（PerformHandshakeConn 在拨号时执行）——
	// L pin 错误指纹（evil）→ 握手阶段 L 校验对端（T=idT）指纹不匹配 evil，
	// 拨号直接失败（fail-closed，无延迟到 Write 的分支）。
	conn, derr := DialE2EStream(ctx, lX, echoLn.Addr().String(), "", EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: []string{evil.Fingerprint()}, // 错误 pin
	})
	if derr != nil {
		if !strings.Contains(derr.Error(), "指纹") {
			t.Fatalf("应报指纹校验失败，got: %v", derr)
		}
		cancel()
		// DialE2EStream 失败时 conn 可能为 nil（握手失败不返回连接）——先判空再关。
		if conn != nil {
			_ = conn.Close()
		}
		_ = lX.Close()
		_ = xL.Close()
		_ = echoLn.Close()
		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeE2EStream 未退出")
		}
		select {
		case <-xErr:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeE2ERelayStream 未退出")
		}
		return
	}
	t.Fatal("pin 不匹配应 fail-closed（L 侧拨号必须失败）")
	_ = lX.Close()
	_ = xL.Close()
	_ = echoLn.Close()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeE2EStream 未退出")
	}
	select {
	case <-xErr:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeE2ERelay 未退出")
	}
}

// TestE2EStream_IdentityOptional 验证 Identity 可选：无身份时纯 ECDH
// （staticKey nil），往返成功（防窃听，X 不见明文）。
func TestE2EStream_IdentityOptional(t *testing.T) {
	t.Parallel()
	lX, xL := net.Pipe()
	rec := &recordingPipe{Conn: xL}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo 监听失败: %v", err)
	}
	defer echoLn.Close()
	serveErr := make(chan error, 1)
	go func() {
		c, aerr := echoLn.Accept()
		if aerr != nil {
			serveErr <- aerr
			return
		}
		defer c.Close()
		// T 侧也无身份：纯 ECDH。
		dec, derr := ServeE2EStream(ctx, c, EndToEndOptions{Enabled: true})
		if derr != nil {
			serveErr <- derr
			return
		}
		defer dec.Close()
		buf := make([]byte, 4096)
		n, rerr := dec.Read(buf)
		if rerr != nil && rerr != io.EOF {
			serveErr <- rerr
			return
		}
		if _, werr := dec.Write(buf[:n]); werr != nil {
			serveErr <- werr
			return
		}
		serveErr <- nil
	}()

	xErr := startXRelayStream(ctx, rec, func(addr string) (string, bool) {
		return echoLn.Addr().String(), true
	})
	defer func() {
		select {
		case <-xErr:
		case <-time.After(2 * time.Second):
		}
	}()

	conn, derr := DialE2EStream(ctx, lX, echoLn.Addr().String(), "", EndToEndOptions{Enabled: true})
	if derr != nil {
		cancel()
		t.Fatalf("DialE2EStream（无身份）失败: %v", derr)
	}
	defer conn.Close()

	plain := "ANON-ECDH-SECRET"
	if _, werr := conn.Write([]byte(plain)); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	buf := make([]byte, len(plain))
	if _, rerr := io.ReadFull(conn, buf); rerr != nil {
		t.Fatalf("读失败: %v", rerr)
	}
	if string(buf) != plain {
		t.Fatalf("明文回读不一致: got %q, want %q", buf, plain)
	}
	if got := rec.snapshot(); strings.Contains(got, plain) {
		t.Fatalf("X 记录明文（应只见密文），snapshot=%q", got)
	}
}

// TestE2EStream_ServeRejectsNonE2E 验证 ServeE2EStream 对非 e2e dial 帧
// （无 e2e:true 标记）fail-closed 报错（不当作普通流处理）。
func TestE2EStream_ServeRejectsNonE2E(t *testing.T) {
	t.Parallel()
	lX, xL := net.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		_, serr := ServeE2EStream(ctx, xL, EndToEndOptions{Enabled: true})
		serveErr <- serr
	}()

	// L 写一个**非 e2e** dial 帧（无 e2e:true）。
	head := []byte(`{"dial":"127.0.0.1:1"}`)
	lenBuf := make([]byte, 4)
	lenBuf[0] = byte(len(head) >> 24)
	lenBuf[1] = byte(len(head) >> 16)
	lenBuf[2] = byte(len(head) >> 8)
	lenBuf[3] = byte(len(head))
	if _, werr := lX.Write(append(lenBuf, head...)); werr != nil {
		t.Fatalf("写非 e2e dial 帧失败: %v", werr)
	}

	select {
	case serr := <-serveErr:
		if serr == nil {
			t.Fatal("非 e2e dial 帧应 fail-closed 报错")
		}
		if !strings.Contains(serr.Error(), "e2e") {
			t.Fatalf("应报 e2e 标记错误，got: %v", serr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeE2EStream 未在时限内返回错误")
	}
	cancel()
	_ = lX.Close()
	_ = xL.Close()
}

// TestE2EStream_EmptyPinFailsClosed 验证空指纹 pin fail-closed（不 panic）。
func TestE2EStream_EmptyPinFailsClosed(t *testing.T) {
	t.Parallel()
	idL, _ := tunnel.GenerateIdentity()
	_, err := DialE2EStream(context.Background(), nil, "127.0.0.1:1", "", EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: []string{""},
	})
	if err == nil {
		t.Fatal("空指纹 pin 应 fail-closed 返回错误")
	}
	if !strings.Contains(err.Error(), "空元素") {
		t.Fatalf("应报空元素 fail-closed，got: %v", err)
	}
}

// startXRelayStream 起 X 侧字节流中继（透传 e2e dial 帧 + 泵密文）：
// 读 dial 帧 → 出口拨号 → 透传 dial 帧到出口 → 泵剩余密文。T 侧据此 DetectE2E。
func startXRelayStream(ctx context.Context, outer net.Conn, dialPolicy func(string) (string, bool)) <-chan error {
	xErr := make(chan error, 1)
	go func() {
		xErr <- ServeE2ERelayStream(ctx, outer, dialPolicy)
	}()
	return xErr
}
