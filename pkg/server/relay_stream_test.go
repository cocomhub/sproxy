// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestRelayStreamHandler_BadJSON 验证非法请求体被拒绝。
func TestRelayStreamHandler_BadJSON(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	req := httptest.NewRequest(http.MethodPost, "/api/relay/stream", strings.NewReader("{bad json"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestRelayStreamHandler_MissingFields 验证缺 target/addr 被拒绝。
// TestRelayStreamHandler_OversizedBody_413（P1-10 回归）：
// 请求体超过上限必须 413 拒绝（MaxBytesReader + *http.MaxBytesError），
// 而非 io.LimitReader 把 JSON 值截断成不完整的 400/静默截断。
func TestRelayStreamHandler_OversizedBody_413(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())

	// JSON 值本身超过 1MiB（大 pad 字段），json.Decoder 解析时必读穿上限。
	body := `{"target":"leaf","type":"tcp","addr":"127.0.0.1:22","pad":"` + strings.Repeat("x", 1<<20) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/relay/stream", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体应返回 413，got %d", w.Code)
	}
}

func TestRelayStreamHandler_MissingFields(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	for _, body := range []string{
		`{"target":"n","type":"tcp","addr":""}`,
		`{"target":"","type":"tcp","addr":"1.2.3.4:80"}`,
		`{"target":"n","type":"udp","addr":"1.2.3.4:80"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/relay/stream", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body=%s expected 400, got %d", body, w.Code)
		}
	}
}

// TestRelayStream_EndToEnd_Echo 端到端验证任意 TCP 流中继：
// hub → 叶子(mux 流) 出站 echo server，hub → 调用方返回双向字节流。
//
// 拓扑（全部 in-process，127.0.0.1）：
//
//	caller(原始 TCP CONNECT 风格 ⇄ RelayStreamHandler) ⇄ pipe mux ⇄ leaf(relay.Serve) ⇄ TCP echo
func TestRelayStream_EndToEnd_Echo(t *testing.T) {
	// 起一个 TCP echo server（127.0.0.1 回环）
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

	// 建立 leaf mux（RoleListener）与 caller mux（RoleDialer）
	pipeA, pipeB := xfertest.Pipe()
	callerMux := mux.New(pipeA, mux.RoleDialer)
	leafMux := mux.New(pipeB, mux.RoleListener)

	// leaf 侧：relay.Serve 出口模式（dialAllow=true），拨号到 echo
	// 测试用回环 echo server，因此用宽松拨号策略（允许回环）；生产默认严格（DialAllowed）。
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	leafErr := make(chan error, 1)
	go func() {
		// DialResultFrames=true：hub 写 200 前需读到 ok 结果帧（I27）。
		leafErr <- relay.Serve(ctx, leafMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testutil.DiscardLogger(),
			relay.ServeOptions{DialPolicy: func(addr string) (string, bool) { return addr, true }, DialResultFrames: true})
	}()

	// caller 侧：注册到 RouteTable 并用 RelayStreamHandler 服务
	rt := hub.NewMeshRouteTable()
	rt.AddNode("", "leaf-node", callerMux)
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 用原始 TCP 拨号 + CONNECT 风格请求，模拟 FileClient.RelayStream 的客户端
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

	// 读响应状态行 + 头直到空行（CONNECT 建立）
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

	// 现在 conn 是纯双向字节流：写 payload 读回 echo
	payload := []byte("hello-relay-stream")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo 不匹配: got %q want %q", got, payload)
	}
	cancel()
	_ = leafErr
}

// TestRelayStream_ClientHalfClose_KeepsInFlightResponse（P0-4 回归）：
// 客户端发送请求后半关闭写侧（TCP FIN），叶子在收到完整输入后延迟返回响应——
// hub 泵送必须传播半关闭并等待宽限期内的在途响应，而不是在任一方完成后立即
// conn.Close()+stream.Abort()（旧实现确定性截断 write-then-read 流）。
func TestRelayStream_ClientHalfClose_KeepsInFlightResponse(t *testing.T) {
	// 延迟响应 server：读完整输入（含 EOF）后等 300ms 再回写响应（模拟慢后端；
	// 若 hub 零宽限截断，客户端将读不到该响应）。
	backLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backLn.Close()
	go func() {
		for {
			c, aerr := backLn.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = io.ReadAll(cn) // 读请求直到 EOF（叶子半关闭传播）
				time.Sleep(300 * time.Millisecond)
				_, _ = cn.Write([]byte("delayed-response"))
			}(c)
		}
	}()
	backAddr := backLn.Addr().String()

	// 建立 leaf mux（RoleListener）与 caller mux（RoleDialer）
	pipeA, pipeB := xfertest.Pipe()
	callerMux := mux.New(pipeA, mux.RoleDialer)
	leafMux := mux.New(pipeB, mux.RoleListener)

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
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

	body, _ := json.Marshal(RelayStreamRequest{Target: "leaf-node", Type: "tcp", Addr: backAddr})
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

	// 发送请求后立即半关闭写侧（模拟 stdin EOF → FIN）。
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatalf("写请求失败: %v", err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatalf("半关闭写侧失败: %v", err)
		}
	} else {
		t.Fatalf("conn 类型 %T 不支持 CloseWrite", conn)
	}

	// 关键断言：在途响应必须被完整读回（旧实现会因 hub 立即 conn.Close()+Abort() 而截断）。
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("delayed-response"))
	if _, rerr := io.ReadFull(conn, got); rerr != nil {
		t.Fatalf("读在途响应失败（可能被 hub 零宽限截断）: %v", rerr)
	}
	if string(got) != "delayed-response" {
		t.Fatalf("响应不匹配: got %q want %q", got, "delayed-response")
	}
	cancel()
	_ = leafErr
}

// TestRelayStream_IdleTimeout_ClosesIdleConnection（P1-9 回归）：
// 中继流 200 后无任何数据流量超过 idleTimeout 即被强制关闭——旧实现无空闲上限，
// "拿到 200 后不发"的客户端可无限期占用叶子出站 FD 与 mux 流。
func TestRelayStream_IdleTimeout_ClosesIdleConnection(t *testing.T) {
	// 静默目标：接受连接但保持沉默（双向空闲）。
	backLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backLn.Close()
	go func() {
		for {
			c, aerr := backLn.Accept()
			if aerr != nil {
				return
			}
			// 保持打开但沉默（不读不写），使中继流双向空闲；测试进程结束随进程清理。
			_ = c
		}
	}()
	backAddr := backLn.Addr().String()

	pipeA, pipeB := xfertest.Pipe()
	callerMux := mux.New(pipeA, mux.RoleDialer)
	leafMux := mux.New(pipeB, mux.RoleListener)

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	go func() {
		_ = relay.Serve(ctx, leafMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testutil.DiscardLogger(),
			relay.ServeOptions{DialPolicy: func(addr string) (string, bool) { return addr, true }, DialResultFrames: true})
	}()

	rt := hub.NewMeshRouteTable()
	rt.AddNode("", "leaf-node", callerMux)
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	h.idleTimeout = 300 * time.Millisecond // 短超时供测试
	ts := httptest.NewServer(h)
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	body, _ := json.Marshal(RelayStreamRequest{Target: "leaf-node", Type: "tcp", Addr: backAddr})
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

	// 200 后不做任何事：等待空闲超时强制关闭。
	// 区分"被 watchdog 关闭"（EOF/连接重置 → PASS）与"仍存活"（读超时 → FAIL）。
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, rerr := conn.Read(make([]byte, 1)); rerr == nil {
		t.Fatal("空闲超时后应被强制关闭，但读到了数据")
	} else if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
		t.Fatal("空闲超时后连接应被关闭，但读取超时（watchdog 未生效）")
	}
}

// TestRelayStreamDialRequest_Framing 验证 dial 帧格式与 hub.DialRequest 一致。
func TestRelayStreamDialRequest_Framing(t *testing.T) {
	d := hub.DialRequest{Dial: "127.0.0.1:22"}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	// 长度前缀 + JSON
	frame := make([]byte, 4+len(b))
	frame[0] = byte(len(b) >> 24)
	frame[1] = byte(len(b) >> 16)
	frame[2] = byte(len(b) >> 8)
	frame[3] = byte(len(b))
	copy(frame[4:], b)

	var parsed hub.DialRequest
	if err := json.Unmarshal(frame[4:], &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Dial != "127.0.0.1:22" {
		t.Fatalf("unexpected dial: %q", parsed.Dial)
	}
}

// TestRelayStreamHandler_UnknownTarget 验证未知目标节点返回 404（I65）。
func TestRelayStreamHandler_UnknownTarget(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	req := httptest.NewRequest(http.MethodPost, "/api/relay/stream",
		strings.NewReader(`{"target":"missing","type":"tcp","addr":"1.2.3.4:80"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing") {
		t.Fatalf("响应体应包含节点 ID，got %q", w.Body.String())
	}
}

// TestRelayStreamHandler_CrossMeshTargetNotFound（M-9 路由面隔离）：调用方（mesh-a）
// 请求中继到 mesh-b 的节点 → 404（跨 mesh 目标对外不可见）。
// 同 mesh 对照：mesh 校验通过并进入转发（叶子回拨号失败 → 502，而非 404）。
func TestRelayStreamHandler_CrossMeshTargetNotFound(t *testing.T) {
	// 叶子 accept 循环：读 dial 帧后回 error 结果帧（让同 mesh 对照快速返回 502，
	// 避免 12s 结果帧超时拖慢测试；跨 mesh 请求在 Open 前即被 404 拦截，不占流）。
	pipeA, pipeB := xfertest.Pipe()
	leafMux := mux.New(pipeB, mux.RoleListener)
	defer leafMux.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		s, aerr := leafMux.Accept(ctx)
		if aerr != nil {
			return
		}
		defer s.Close()
		lenBuf := make([]byte, 4)
		if _, rerr := io.ReadFull(s, lenBuf); rerr != nil {
			return
		}
		meta := make([]byte, binary.BigEndian.Uint32(lenBuf))
		if _, rerr := io.ReadFull(s, meta); rerr != nil {
			return
		}
		frame, _ := json.Marshal(hub.DialResultFrame{DialResult: hub.DialResultError, Message: "deny"})
		ob := make([]byte, 4)
		binary.BigEndian.PutUint32(ob, uint32(len(frame)))
		_, _ = s.Write(ob)
		_, _ = s.Write(frame)
	}()

	rt := hub.NewMeshRouteTable()
	callerMux := mux.New(pipeA, mux.RoleDialer)
	defer callerMux.Close()
	rt.AddNode("mesh-b", "node-b", callerMux)
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())

	// 跨 mesh：mesh-a 调用方 → mesh-b 节点 → 404（不可见）。
	req := httptest.NewRequest(http.MethodPost, "/api/relay/stream",
		strings.NewReader(`{"target":"node-b","type":"tcp","addr":"1.2.3.4:80"}`))
	req = req.WithContext(withMesh(req.Context(), "mesh-a"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("跨 mesh 中继目标应 404, got %d", w.Code)
	}

	// 对照：同 mesh（mesh-b）→ 通过 mesh 校验并进入转发（叶子回拨号失败 → 502）。
	req2 := httptest.NewRequest(http.MethodPost, "/api/relay/stream",
		strings.NewReader(`{"target":"node-b","type":"tcp","addr":"1.2.3.4:80"}`))
	req2 = req2.WithContext(withMesh(req2.Context(), "mesh-b"))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadGateway {
		t.Fatalf("同 mesh 目标应进入转发（502 拨号失败）, got %d", w2.Code)
	}
}

// TestRelayStreamHandler_NilLogger 验证构造器 logger==nil 兜底为 slog.Default（I65）。
func TestRelayStreamHandler_NilLogger(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	h := NewRelayStreamHandler(rt, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/relay/stream",
		strings.NewReader(`{"target":"missing","type":"tcp","addr":"1.2.3.4:80"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestRelayStreamHandler_NilRouteTable 验证 routeTable nil 守卫返回 404 而非 panic（I65/S34）。
func TestRelayStreamHandler_NilRouteTable(t *testing.T) {
	h := NewRelayStreamHandler(nil, testutil.DiscardLogger())
	req := httptest.NewRequest(http.MethodPost, "/api/relay/stream",
		strings.NewReader(`{"target":"n","type":"tcp","addr":"1.2.3.4:80"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestRelayStreamHandler_BadAddr 验证 addr 语法校验 fail-fast 返回 400（I26/I65）。
func TestRelayStreamHandler_BadAddr(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	for _, body := range []string{
		`{"target":"n","type":"tcp","addr":"garbage"}`,       // 无端口
		`{"target":"n","type":"tcp","addr":"127.0.0.1"}`,     // 无端口
		`{"target":"n","type":"tcp","addr":"http://x"}`,      // 协议分隔符
		`{"target":"n","type":"tcp","addr":"a@b:80"}`,        // @
		`{"target":"n","type":"tcp","addr":"1.2.3.4:0"}`,     // 端口 0
		`{"target":"n","type":"tcp","addr":"1.2.3.4:99999"}`, // 端口越界
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/relay/stream", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body=%s expected 400, got %d", body, w.Code)
		}
	}
}

// TestRelayStreamHandler_NoHijacker 验证 ResponseWriter 无 http.Hijacker 时返回 500（I65）。
// 需要叶子先回 ok 结果帧（I27：hub 写 200/升级前先读结果帧），再走到 hijack 步骤。
func TestRelayStreamHandler_NoHijacker(t *testing.T) {
	pipeA, pipeB := xfertest.Pipe()
	callerMux := mux.New(pipeA, mux.RoleDialer)
	defer callerMux.Close()
	leafMux := mux.New(pipeB, mux.RoleListener)
	defer leafMux.Close()

	// 叶子 accept 流，读 dial 帧后回写 ok 结果帧，使 hub 通过结果帧检查进入 hijack。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		s, aerr := leafMux.Accept(ctx)
		if aerr != nil {
			return
		}
		defer s.Close()
		lenBuf := make([]byte, 4)
		if _, rerr := io.ReadFull(s, lenBuf); rerr != nil {
			return
		}
		metaLen := binary.BigEndian.Uint32(lenBuf)
		meta := make([]byte, metaLen)
		if _, rerr := io.ReadFull(s, meta); rerr != nil {
			return
		}
		ok, _ := json.Marshal(hub.DialResultFrame{DialResult: hub.DialResultOK})
		ob := make([]byte, 4)
		binary.BigEndian.PutUint32(ob, uint32(len(ok)))
		_, _ = s.Write(ob)
		_, _ = s.Write(ok)
	}()

	rt := hub.NewMeshRouteTable()
	rt.AddNode("", "node-a", callerMux)
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())

	req := httptest.NewRequest(http.MethodPost, "/api/relay/stream",
		strings.NewReader(`{"target":"node-a","type":"tcp","addr":"1.2.3.4:80"}`))
	w := httptest.NewRecorder() // 无 Hijacker
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d (body=%q)", w.Code, w.Body.String())
	}
}

// TestRelayStream_DialFailure_Returns502 验证叶子拨号失败（DialPolicy 拒绝）时
// hub 读到 error 结果帧返回 502（I27 错误路径端到端）。
func TestRelayStream_DialFailure_Returns502(t *testing.T) {
	pipeA, pipeB := xfertest.Pipe()
	callerMux := mux.New(pipeA, mux.RoleDialer)
	defer callerMux.Close()
	leafMux := mux.New(pipeB, mux.RoleListener)
	defer leafMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	leafErr := make(chan error, 1)
	go func() {
		// DialPolicy 拒绝一切 → 叶子回 error 结果帧。
		leafErr <- relay.Serve(ctx, leafMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testutil.DiscardLogger(),
			relay.ServeOptions{
				DialPolicy:       func(addr string) (string, bool) { return "", false },
				DialResultFrames: true,
			})
	}()

	rt := hub.NewMeshRouteTable()
	rt.AddNode("", "leaf-node", callerMux)
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())

	body, _ := json.Marshal(RelayStreamRequest{Target: "leaf-node", Type: "tcp", Addr: "1.2.3.4:80"})
	req := httptest.NewRequest(http.MethodPost, "/api/relay/stream", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	cancel()
	_ = leafErr
}
