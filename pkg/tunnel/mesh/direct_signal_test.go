// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// directSigTestServer 构造并启动监听 127.0.0.1 的直连信令服务器：非空 secret 会
// SetSecret、非空 fps 会 SetAllowedFingerprints（均在 Serve 启动前配置）。返回服务器
// 与带超时的测试上下文；二者经 t.Cleanup 释放（关闭顺序与 Serve 启动顺序一致）。
func directSigTestServer(t *testing.T, timeout time.Duration, secret string, fps []string) (*DirectSignalServer, context.Context) {
	t.Helper()
	srv, err := NewDirectSignalServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewDirectSignalServer: %v", err)
	}
	if secret != "" {
		srv.SetSecret(secret)
	}
	if len(fps) > 0 {
		srv.SetAllowedFingerprints(fps)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	go srv.Serve(ctx)
	t.Cleanup(func() { _ = srv.Close() })
	return srv, ctx
}

// directSigTestDial 以 node-dialer 身份拨入直连信令服务器，返回拨号侧 Signaler。
func directSigTestDial(t *testing.T, ctx context.Context, srv *DirectSignalServer) DirectSignaler {
	t.Helper()
	client, err := DialDirectSignaler(ctx, srv.Addr().String(), "node-dialer")
	if err != nil {
		t.Fatalf("DialDirectSignaler: %v", err)
	}
	return client
}

// directSigTestListenAnswer 监听侧角色：等待 offer 并回 answer；from/offer 不匹配时以
// 组合错误消息上报。结果经 errCh 收集（nil=成功）。
func directSigTestListenAnswer(sig webrtc.Signaler, ctx context.Context, errCh chan error) {
	from, offer, werr := sig.WaitOffer(ctx)
	if werr != nil {
		errCh <- werr
		return
	}
	if from != "node-dialer" || offer != "offer-sdp" {
		errCh <- fmt.Errorf("from=%q offer=%q", from, offer)
		return
	}
	errCh <- sig.SendAnswer("node-dialer", "answer-sdp")
}

// directSigTestListenAnswerExact 监听侧角色（严格版）：等待 offer 后逐字段校验
// from/offer（报错更精确），再回 answer。经 errCh 上报错误（nil=成功）。
func directSigTestListenAnswerExact(sig webrtc.Signaler, ctx context.Context, errCh chan error) {
	from, offer, werr := sig.WaitOffer(ctx)
	if werr != nil {
		errCh <- werr
		return
	}
	if from != "node-dialer" {
		errCh <- fmt.Errorf("from = %q, want node-dialer", from)
		return
	}
	if offer != "offer-sdp" {
		errCh <- fmt.Errorf("offer = %q, want offer-sdp", offer)
		return
	}
	errCh <- sig.SendAnswer("node-dialer", "answer-sdp")
}

// directSigTestOfferAnswer 执行一次 offer/answer 交换：listener 以 goroutine 扮演监听侧
// （等待 offer 并回 answer），随后拨号侧发送 offer、断言收到 answer-sdp；withFrom 为真时
// 额外断言 answer 的 from 为空。监听侧错误经 channel 收集后断言。
func directSigTestOfferAnswer(t *testing.T, ctx context.Context, sig webrtc.Signaler, client DirectSignaler, listener func(webrtc.Signaler, context.Context, chan error), withFrom bool) {
	t.Helper()
	errCh := make(chan error, 1)
	go listener(sig, ctx, errCh)
	if serr := client.SendOffer("node-listener", "offer-sdp"); serr != nil {
		t.Fatalf("SendOffer: %v", serr)
	}
	from, answer, aerr := client.WaitAnswer(ctx)
	if aerr != nil {
		t.Fatalf("WaitAnswer: %v", aerr)
	}
	if withFrom {
		if from != "" {
			t.Errorf("answer from = %q, want 空（拨号侧不区分）", from)
		}
	}
	if answer != "answer-sdp" {
		t.Errorf("answer = %q, want answer-sdp", answer)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("监听侧失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("等待监听侧完成超时")
	}
}

// directSigRejectCase 是直连信令拒绝帧测试的用例描述：待写入的信令帧与各失败文案
// （dialMsg/writeMsg/wantMsg），打包传入 directSigTestSendRejectedFrame 避免过参透传。
type directSigRejectCase struct {
	msg      directSignalMsg
	dialMsg  string
	writeMsg string
	wantMsg  string
}

// directSigTestSendRejectedFrame 拨入一条原始连接、写入一个应被拒的信令帧后关闭，断言
// WaitOffer 返回 errDirectSignalConn（非致命拒绝）。dialMsg/writeMsg/wantMsg 为失败文案。
func directSigTestSendRejectedFrame(t *testing.T, ctx context.Context, srv *DirectSignalServer, sig webrtc.Signaler, tc directSigRejectCase) {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("%s: %v", tc.dialMsg, err)
	}
	if werr := writeDirectSignalFrame(conn, tc.msg); werr != nil {
		t.Fatalf("%s: %v", tc.writeMsg, werr)
	}
	_ = conn.Close()
	if _, _, werr := sig.WaitOffer(ctx); !errors.Is(werr, errDirectSignalConn) {
		t.Fatalf("%s, got %v", tc.wantMsg, werr)
	}
}

// directSigTestWebRTCEcho 监听侧 WebRTC echo 角色：握手后读 hello→写 world→再读 bye
// 收尾（二次握手避免写完即关的竞态）；任何失败以 error 上报 errCh（仅本 goroutine 写）。
func directSigTestWebRTCEcho(ctx context.Context, sig webrtc.Signaler, errCh chan error) {
	conn, lerr := webrtc.ListenWithSignalerCtx(ctx, "node-listener", sig)
	if lerr != nil {
		errCh <- lerr
		return
	}
	defer conn.Close()
	buf := make([]byte, 64)
	n, rerr := conn.Read(buf)
	if rerr != nil {
		errCh <- fmt.Errorf("监听侧读失败: %w", rerr)
		return
	}
	if string(buf[:n]) != "hello" {
		errCh <- fmt.Errorf("监听侧收到 %q, want hello", buf[:n])
		return
	}
	if _, werr := conn.Write([]byte("world")); werr != nil {
		errCh <- fmt.Errorf("监听侧写失败: %w", werr)
		return
	}
	n, rerr = conn.Read(buf)
	if rerr != nil {
		errCh <- fmt.Errorf("监听侧读 bye 失败: %w", rerr)
		return
	}
	if string(buf[:n]) != "bye" {
		errCh <- fmt.Errorf("监听侧收到 %q, want bye", buf[:n])
		return
	}
	errCh <- nil
}

// directSigTestDialWebRTC 拨号侧经直连信令拨入 node-listener：完成 hello/world 双向传输
// 与 bye 收尾，最后等待监听侧 listenerErr 结果并断言无错误。
func directSigTestDialWebRTC(t *testing.T, ctx context.Context, srv *DirectSignalServer, listenerErr chan error) {
	t.Helper()
	client := directSigTestDial(t, ctx, srv)
	defer client.Close()
	conn, err := webrtc.DialWithSignalerCtx(ctx, "node-listener", client)
	if err != nil {
		t.Fatalf("DialWithSignalerCtx: %v", err)
	}
	defer conn.Close()
	if conn.RemotePeerID() != "node-listener" {
		t.Errorf("RemotePeerID = %q, want node-listener", conn.RemotePeerID())
	}
	if _, werr := conn.Write([]byte("hello")); werr != nil {
		t.Fatalf("拨号侧写失败: %v", werr)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("拨号侧读失败: %v", err)
	}
	if string(buf[:n]) != "world" {
		t.Fatalf("拨号侧收到 %q, want world", buf[:n])
	}
	if _, err := conn.Write([]byte("bye")); err != nil {
		t.Fatalf("拨号侧写 bye 失败: %v", err)
	}
	select {
	case err := <-listenerErr:
		if err != nil {
			t.Fatalf("监听侧失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("等待监听侧完成超时")
	}
}

// TestDirectSignaler_OfferAnswer 直连信令协议级测试：拨号侧发 offer、监听侧读 offer
// 并回 answer、拨号侧读 answer，且监听侧能恢复拨号方 node-id。
func TestDirectSignaler_OfferAnswer(t *testing.T) {
	srv, ctx := directSigTestServer(t, 15*time.Second, "", nil)
	serverSig := srv.NewSignaler()
	client := directSigTestDial(t, ctx, srv)
	defer client.Close()
	directSigTestOfferAnswer(t, ctx, serverSig, client, directSigTestListenAnswerExact, true)
}

// TestDirectSignaler_MalformedConnNonFatal（F1 回归）：直连信令端口收到空/畸形连接
// （端口扫描 / curl 误连）应返回 errDirectSignalConn 并关闭该连接，**不**使监听器失效；
// 随后正常的 offer/answer 交换仍能成功（否则远程无认证即可杀整节点）。
func TestDirectSignaler_MalformedConnNonFatal(t *testing.T) {
	srv, ctx := directSigTestServer(t, 15*time.Second, "", nil)
	sig := srv.NewSignaler()
	// 场景 1：连上即关（端口扫描）→ WaitOffer 返回 errDirectSignalConn。
	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	if _, _, werr := sig.WaitOffer(ctx); !errors.Is(werr, errDirectSignalConn) {
		t.Fatalf("空连接应返回 errDirectSignalConn, got %v", werr)
	}
	// 场景 2：畸形长度前缀（0xFFFFFFFF > 上限）→ 同样非致命。
	conn2, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	_, _ = conn2.Write([]byte{0xff, 0xff, 0xff, 0xff, 'x', 'x'})
	_ = conn2.Close()
	if _, _, werr := sig.WaitOffer(ctx); !errors.Is(werr, errDirectSignalConn) {
		t.Fatalf("畸形帧应返回 errDirectSignalConn, got %v", werr)
	}
	// 场景 3：监听器仍存活——正常 offer/answer 成功。
	client := directSigTestDial(t, ctx, srv)
	defer client.Close()
	directSigTestOfferAnswer(t, ctx, sig, client, directSigTestListenAnswer, false)
}

// TestDirectSignaler_SecretAuth（安全审查 A/C 回归）：配置共享密钥后，无签名/错误签名
// 的 offer 被拒（非致命，监听器存活），正确签名被接受——防未授权 peer 借本节点作
// 中继/出口。
func TestDirectSignaler_SecretAuth(t *testing.T) {
	srv, ctx := directSigTestServer(t, 15*time.Second, "mesh-secret", nil)
	sig := srv.NewSignaler()
	// 场景 1：无签名 offer → 拒。
	directSigTestSendRejectedFrame(t, ctx, srv, sig, directSigRejectCase{msg: directSignalMsg{Node: "evil", SDP: "offer-sdp"}, dialMsg: "dial", writeMsg: "write", wantMsg: "无签名 offer 应被拒"})
	// 场景 2：错误签名 offer → 拒。
	directSigTestSendRejectedFrame(t, ctx, srv, sig, directSigRejectCase{msg: directSignalMsg{Node: "evil", SDP: "offer-sdp", Sig: "wrong-sig"}, dialMsg: "dial2", writeMsg: "write2", wantMsg: "错误签名 offer 应被拒"})
	// 场景 3：正确签名 → 接受，answer 成功。
	client := directSigTestDial(t, ctx, srv)
	defer client.Close()
	client.SetSecret("mesh-secret")
	directSigTestOfferAnswer(t, ctx, sig, client, directSigTestListenAnswer, false)
}

// TestDirectSignaler_WaitAnswerCtxAware（N1 回归）：对端信令端点可达但不回 answer 时，
// WaitAnswer 应在 ctx 到期时及时返回 ctx.Err()（而非卡满 directSignalTimeout 30s），
// 保证 WebRTCProbeTimeout(10s) 与用户中断/节点关停的 ctx 语义真实生效。
func TestDirectSignaler_WaitAnswerCtxAware(t *testing.T) {
	// 伪信令端点：接受 offer 帧后挂起（永不回 answer）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		// 读完完整 offer 帧后挂起（不回复 answer），直到对端关闭连接。
		var lenBuf [4]byte
		if _, rerr := io.ReadFull(c, lenBuf[:]); rerr != nil {
			return
		}
		payload := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
		if _, rerr := io.ReadFull(c, payload); rerr != nil {
			return
		}
		var one [1]byte
		_, _ = c.Read(one[:]) // 阻塞：对端不会再发数据；连接关闭即返回
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := DialDirectSignaler(ctx, ln.Addr().String(), "node-dialer")
	if err != nil {
		t.Fatalf("DialDirectSignaler: %v", err)
	}
	defer client.Close()
	if serr := client.SendOffer("node-listener", "offer-sdp"); serr != nil {
		t.Fatalf("SendOffer: %v", serr)
	}

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer shortCancel()
	start := time.Now()
	_, _, werr := client.WaitAnswer(shortCtx)
	elapsed := time.Since(start)
	if !errors.Is(werr, context.DeadlineExceeded) {
		t.Fatalf("WaitAnswer = %v, want context.DeadlineExceeded", werr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("WaitAnswer 未在 ctx 到期时及时返回，耗时 %v", elapsed)
	}
}

// TestWebRTCOverDirectSignaling 全链路：经直连信令（非 hub）建立 WebRTC 连接，
// 数据面可双向传输——这是 mDNS 无 hub 场景 `mesh connect` 的核心链路。
func TestWebRTCOverDirectSignaling(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback，避免防火墙弹窗。
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(10 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	// 监听侧：WaitOffer 接一个连接 → ListenWithSignalerCtx 完成握手 → 读写数据。
	// 收尾用二次握手（监听侧读 "bye"）避免"写完即关"与拨号侧读之间的竞态
	// （pion 在 close 后可能返回 abort 而非缓冲数据）。
	srv, ctx := directSigTestServer(t, 30*time.Second, "", nil)
	listenerErr := make(chan error, 1)
	go directSigTestWebRTCEcho(ctx, srv.NewSignaler(), listenerErr)
	directSigTestDialWebRTC(t, ctx, srv, listenerErr)
}

// TestDirectSignaler_FingerprintAuth：直连信令指纹认证——接受侧配置
// AllowedFingerprints 白名单后，offer 必须携带匹配的 fp= 才被接受（fail-closed），
// 无 fp / 指纹不匹配的 offer 被拒（非致命，监听器存活）。防"任意节点可连入"。
func TestDirectSignaler_FingerprintAuth(t *testing.T) {
	t.Parallel()
	const dialerFP = "sha256:aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999"
	srv, ctx := directSigTestServer(t, 15*time.Second, "", []string{dialerFP})
	sig := srv.NewSignaler()
	// 场景 1：无 fp 的 offer → 拒（fail-closed：配置白名单时缺指纹即拒绝）。
	directSigTestSendRejectedFrame(t, ctx, srv, sig, directSigRejectCase{msg: directSignalMsg{Node: "evil", SDP: "offer-sdp"}, dialMsg: "dial", writeMsg: "write", wantMsg: "无 fp offer 应被拒"})
	// 场景 2：fp 不匹配 → 拒。
	directSigTestSendRejectedFrame(t, ctx, srv, sig, directSigRejectCase{msg: directSignalMsg{Node: "evil", SDP: "offer-sdp", FP: "sha256:zzzz"}, dialMsg: "dial2", writeMsg: "write2", wantMsg: "指纹不匹配 offer 应被拒"})
	// 场景 3：正确 fp → 接受。
	client := directSigTestDial(t, ctx, srv)
	defer client.Close()
	client.SetFingerprint(dialerFP)
	directSigTestOfferAnswer(t, ctx, sig, client, directSigTestListenAnswer, false)
}

// TestDirectSignaler_FingerprintInSignature：配置共享密钥 + 指纹时，fp= 加入
// 信令签名内容（computeSignalSig 带 fp 参数）——防篡改（攻击者改 fp 破坏签名）。
func TestDirectSignaler_FingerprintInSignature(t *testing.T) {
	t.Parallel()
	// 直接验证：computeSignalSig 签名内容含 fp（通过重算对比——fp 变则签名变）。
	sigA := computeSignalSig("secret", "node-a", "offer-sdp", "fp1")
	sigB := computeSignalSig("secret", "node-a", "offer-sdp", "fp2")
	if sigA == sigB {
		t.Error("computeSignalSig 应包含 fp（fp 变则签名变）")
	}
	if sigA == "" || sigB == "" {
		t.Error("computeSignalSig 不应为空")
	}
}
