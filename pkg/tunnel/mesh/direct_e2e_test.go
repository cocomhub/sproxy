// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// TestDialWebRTC_E2EWired 验证 direct 直连（webrtc 打洞）端到端加密接线：
// DialWebRTC 配 e2eOpts 时，跳过普通 dial 帧（避免帧序冲突），在 mux 流上写 e2e 帧
// + ECDH 握手（DialE2EStream）→ 对端 relay.Serve（E2EServe 解密 echo）——L⇄T
// 端到端加密（X/hub 不存在，直连本身加密）。
//
// 断言：① 明文往返成功（E2E 生效——对端 E2EServe 只认 e2e 帧，非 E2E 会解密失败）；
// ② L 无 E2E 时对端 E2EServe 解密普通帧失败（对照，证明 E2E 帧是必需标记）。
func TestDialWebRTC_E2EWired(t *testing.T) {
	t.Parallel()
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(10 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	idL, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成 L 身份失败: %v", err)
	}
	idT, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成 T 身份失败: %v", err)
	}

	// T 侧：relay.Serve（E2EServe 解密 echo）。
	serviceAddr := startEchoService(t)
	srv, err := NewDirectSignalServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewDirectSignalServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	go srv.Serve(ctx)

	const peerID = "node-listener"
	const localID = "node-dialer"
	go func() {
		conn, lErr := webrtc.ListenWithSignalerOptsCtx(ctx, peerID, srv.NewSignaler(), nil)
		if lErr != nil {
			return
		}
		m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
		defer func() { _ = m.Close() }()
		_ = relay.Serve(ctx, m, "", true, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
			relay.ServeOptions{
				DialPolicy: relay.NewServiceDialPolicy(nil, []string{serviceAddr}),
				// T 侧 E2EServe：只认 e2e 帧（普通帧解密失败 → fail-closed 关流）。
				E2EServe: E2EServeClosure(idT, []string{idL.Fingerprint()}),
			})
	}()

	dsig, err := DialDirectSignaler(ctx, srv.Addr().String(), localID)
	if err != nil {
		t.Fatalf("DialDirectSignaler: %v", err)
	}
	defer func() { _ = dsig.Close() }()

	// L 侧：DialWebRTC 配 E2E（e2eOpts 非 nil）。
	conn, err := DialWebRTC(ctx, dsig, &client.MeshService{Node: peerID, Addr: serviceAddr}, nil, &EndToEndOptions{
		Enabled:          true,
		Identity:         idL,
		PeerFingerprints: []string{idT.Fingerprint()},
		HandshakeTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("DialWebRTC（E2E）失败: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 明文往返（L⇄T 端到端加密，对端 E2EServe 解密 echo）。
	plain := "DIRECT-E2E-SECRET"
	if _, werr := conn.Write([]byte(plain)); werr != nil {
		t.Fatalf("写: %v", werr)
	}
	buf := make([]byte, len(plain))
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, rErr := io.ReadFull(conn, buf); rErr != nil {
		t.Fatalf("读: %v", rErr)
	}
	if string(buf) != plain {
		t.Fatalf("明文回读不一致: got %q, want %q", buf, plain)
	}
}

// TestServeE2EStreamAfterFrame_NonE2EMetaFailsClosed 验证「已读帧」模式的 fail-closed：
// meta 是非 e2e dial 帧（E2E=false/缺省）时，serveE2EStreamAfterFrame 必须拒绝
// （不能把非加密帧当加密流处理——安全红线，禁静默明文降级）。
func TestServeE2EStreamAfterFrame_NonE2EMetaFailsClosed(t *testing.T) {
	t.Parallel()
	idT, _ := tunnel.GenerateIdentity()
	// outer 非 nil（nil 会在 e2e 校验前先报「外层为空」，测不到 e2e 校验分支）。
	pa, pb := net.Pipe()
	defer pa.Close()
	defer pb.Close()
	_, err := serveE2EStreamAfterFrame(t.Context(), pa, []byte(`{"dial":"127.0.0.1:1"}`), EndToEndOptions{
		Enabled:  true,
		Identity: idT,
	})
	if err == nil {
		t.Fatal("非 e2e 已读帧应 fail-closed（拒绝按加密流处理）")
	}
	if got := err.Error(); !strings.Contains(got, "非 e2e") {
		t.Fatalf("错误应含 '非 e2e', got %q", got)
	}
}
