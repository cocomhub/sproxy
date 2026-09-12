// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// shortWriteRelayStream 是「窗口受限短写」的假中继流：单次 Write 最多投递 limit 字节并
// 返回 (n, nil)。真 mux 流只在剩余窗口恰好小于 len(p) 时才短写（依赖对端消费时序），
// 本假流把该行为变成确定性输入。Read 阻塞到 CloseWrite 后返回 EOF，使反方向泵送正常收尾。
type shortWriteRelayStream struct {
	limit     int
	mu        sync.Mutex
	buf       bytes.Buffer
	closeOnce sync.Once
	closed    chan struct{}
}

func newShortWriteRelayStream(limit int) *shortWriteRelayStream {
	return &shortWriteRelayStream{limit: limit, closed: make(chan struct{})}
}

func (s *shortWriteRelayStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > s.limit {
		p = p[:s.limit]
	}
	return s.buf.Write(p)
}

func (s *shortWriteRelayStream) Read([]byte) (int, error) {
	<-s.closed
	return 0, io.EOF
}

func (s *shortWriteRelayStream) CloseWrite() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func (s *shortWriteRelayStream) Close() error { return s.CloseWrite() }
func (s *shortWriteRelayStream) Abort() error { return s.CloseWrite() }

func (s *shortWriteRelayStream) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

// TestPumpRelayConn_ShortWriteStreamNotTruncated 确定性地钉住 pumpRelayConn 的
// 「客户端 → 中继流」方向不因短写而截断（I2）。回归背景：该方向曾是
// `_, _ = io.Copy(stream, ...)`——io.Copy 遇到 mux 流首次短写即返回 io.ErrShortWrite
// 并停止（返回值被丢弃），随后 CloseWrite 把截断当正常半关闭传播给对端。
func TestPumpRelayConn_ShortWriteStreamNotTruncated(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serverCh := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			serverCh <- c
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-serverCh
	defer server.Close()

	payload := bytes.Repeat([]byte{0x2a}, 200000) // 远超窗口；1 KB 短写下约 200 轮
	go func() {
		_, _ = client.Write(payload)
		// 半关闭：让泵送方向的读侧正常 EOF 收尾（写失败在变异下可忽略，断言看流侧内容）。
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	rt := hub.NewMeshRouteTable()
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	stream := newShortWriteRelayStream(1024)
	rw := bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server))
	h.pumpRelayConn(rw, server, stream, 0) // idleTimeout=0 ⇒ 不启用 watchdog

	if got := stream.Bytes(); !bytes.Equal(got, payload) {
		t.Fatalf("中继泵送被短写截断: got %d bytes want %d", len(got), len(payload))
	}
}

// TestRelayStream_LargePayloadNotTruncated 钉住中继泵送在**大于 mux 流控窗口（64 KB）**
// 的载荷上不被静默截断（I2）。回归背景：pumpRelayConn 两方向曾是
// `_, _ = io.Copy(stream/conn, ...)`——io.Copy 遇到 mux 流的窗口受限短写会返回
// io.ErrShortWrite 并提前结束（调用点丢弃返回值），随后把**截断**当正常半关闭传播
// 出去。本用例经「叶子回环 echo」实测双向全量：客户端写入 N 字节必须原样读回 N 字节。
func TestRelayStream_LargePayloadNotTruncated(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = io.Copy(cn, cn) // echo
			}(c)
		}
	}()
	echoAddr := echoLn.Addr().String()

	pipeA, pipeB := xfertest.Pipe()
	callerMux := mux.New(pipeA, mux.RoleDialer)
	leafMux := mux.New(pipeB, mux.RoleListener)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	leafErr := make(chan error, 1)
	go func() {
		leafErr <- relay.Serve(ctx, leafMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testutil.DiscardLogger(),
			relay.ServeOptions{DialPolicy: func(addr string) (string, bool) { return addr, true }, DialResultFrames: true})
	}()

	rt := hub.NewMeshRouteTable()
	rt.AddNode("", "leaf-node", callerMux)
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	ts := httptest.NewServer(h)
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	body, _ := json.Marshal(RelayStreamRequest{Target: "leaf-node", Type: "tcp", Addr: echoAddr})
	reqLine := fmt.Sprintf("POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", addr, len(body))
	if _, werr := io.WriteString(conn, reqLine); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := conn.Write(body); werr != nil {
		t.Fatal(werr)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, " 200 ") {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		t.Fatalf("hub 返回 %s%s", strings.TrimSpace(statusLine), rest)
	}
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			t.Fatal(rerr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// 200000 B 远超 65536 窗口：旧 io.Copy 实现在这里截断（泵送方向提前结束）。
	payload := bytes.Repeat([]byte{0x7e}, 200000)
	writeErr := make(chan error, 1)
	go func() {
		_, werr := conn.Write(payload)
		writeErr <- werr
	}()

	// 客户端可能尚未消费 bufio 预读字节，这里从 br 读（br 包着 conn）。
	if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(br, got); rerr != nil {
		t.Fatalf("大载荷 echo 读取失败（疑被静默截断）: %v", rerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo 内容不符（截断或错位）: got %d bytes want %d", len(got), len(payload))
	}
	if werr := <-writeErr; werr != nil {
		t.Fatalf("写入失败: %v", werr)
	}
	cancel()
	_ = leafErr // 与既有 Echo 用例一致：不阻塞等待叶子 Serve 收敛
}
