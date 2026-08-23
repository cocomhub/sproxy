// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// TestWebRTCStream_WritesDialFrameOnMuxStream（P0-1 回归）：
// 直连数据面必须在 mux 流上写拨号帧，而非裸字节写 DataChannel。对端 p2p listen
// 用 mux 按帧消费，本测试复现对端消费方式断言读到正确拨号帧。
func TestWebRTCStream_WritesDialFrameOnMuxStream(t *testing.T) {
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })

	signal := webrtc.NewSignal()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	frameCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := webrtc.Listen(signal)
		if err != nil {
			errCh <- err
			return
		}
		m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
		defer m.Close()
		stream, err := m.Accept(ctx)
		if err != nil {
			errCh <- err
			return
		}
		defer stream.Close()
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(stream, lenBuf); err != nil {
			errCh <- err
			return
		}
		meta := make([]byte, binary.BigEndian.Uint32(lenBuf))
		if _, err := io.ReadFull(stream, meta); err != nil {
			errCh <- err
			return
		}
		frameCh <- string(meta)
	}()

	conn, err := webrtc.Dial(signal)
	if err != nil {
		t.Fatalf("dial webrtc: %v", err)
	}
	defer conn.Close()
	res, err := WebRTCStream(ctx, conn, "127.0.0.1:22")
	if err != nil {
		t.Fatalf("WebRTCStream: %v", err)
	}
	if res.Kind != KindWebRTC {
		t.Fatalf("kind = %q, want webrtc", res.Kind)
	}

	select {
	case f := <-frameCh:
		var d hub.DialRequest
		if err := json.Unmarshal([]byte(f), &d); err != nil || d.Dial != "127.0.0.1:22" {
			t.Fatalf("对端收到的拨号帧 = %q, want {\"dial\":\"127.0.0.1:22\"}", f)
		}
	case err := <-errCh:
		t.Fatalf("对端读取拨号帧失败: %v", err)
	case <-ctx.Done():
		t.Fatal("等待对端读取拨号帧超时")
	}
}

// TestDial_FallsBackToRelay：webrtc 打洞失败（不可达信令）时回落 hub 中继。
func TestDial_FallsBackToRelay(t *testing.T) {
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/relay/stream" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	target := &client.MeshService{Name: "svc", Node: "node-a", Addr: "127.0.0.1:22"}
	// 不可达 hub 信令器 → webrtc 必然失败 → 回落中继。
	signaler := hub.NewHubSignaler("http://127.0.0.1:1", "", "local-node")

	_, err := Dial(context.Background(), svc, signaler, target, "local-node")
	if err == nil {
		t.Fatal("expected error: webrtc 失败且中继失败应报错")
	}
	// 错误应来自中继（webrtc 已回落），而非 webrtc 本身：用 "RelayStream" 前缀判定
	// （svc.RelayStream 的错误均以该前缀包装），覆盖 502/连接重置/超时等 relay 错误。
	if !strings.Contains(err.Error(), "RelayStream") {
		t.Fatalf("expected relay fallback error, got: %v", err)
	}
}
