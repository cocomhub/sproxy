// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestDial_E2EWired 验证 L 侧 E2E 接线：DialWithOptions 配 E2E 时，
// RelayStream 返回的裸数据面连接包 DialE2EStream → 与 T 侧（mock hub 数据面
// 跑 ServeE2EStream）端到端加密字节流往返；Result.EndToEnd = true。
//
// mock hub：POST /api/relay/stream → 200 升级 → 数据面对端跑 ServeE2EStream
// （读 e2e dial 帧 → 握手 → 解密 → echo 回显）。X（hub）只看密文。
func TestDial_E2EWired(t *testing.T) {
	t.Parallel()
	idL, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成 L 身份失败: %v", err)
	}
	idT, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成 T 身份失败: %v", err)
	}

	// mock hub：relay/stream → 200 升级 → 数据面对端 = T 侧（ServeE2EStream echo）。
	hubLn := mockHubListen(t)
	go mockHubAcceptLoop(hubLn, func(c net.Conn) {
		mockHubE2EWiredConn(c, idT, idL)
	})

	// L 侧：DialWithOptions 配 E2E。
	e2eWiredAssertRoundtrip(t, hubLn, idL, idT)
}

// TestDial_NoE2EConfigPlaintext 验证未配置 E2E（nil）时保持现有裸数据面（零回归）。
func TestDial_NoE2EConfigPlaintext(t *testing.T) {
	t.Parallel()
	// mock hub：relay/stream → 200 升级 → 数据面裸 echo（无 E2E）。
	hubLn := mockHubListen(t)
	go mockHubAcceptLoop(hubLn, mockHubPlainEchoConn)

	e2eWiredAssertPlaintext(t, hubLn)
}

// TestDial_E2EHandshakeTimeout 验证 E2E 握手超时（对端不回握手）不无限阻塞：
// HandshakeTimeout 到期返回错误（DoS 面防护，任务 2 审查 Minor-1）。
func TestDial_E2EHandshakeTimeout(t *testing.T) {
	t.Parallel()
	idL, _ := tunnel.GenerateIdentity()
	idT, _ := tunnel.GenerateIdentity()

	// mock hub：200 升级后数据面静默（不回握手）→ E2E 握手挂起，HandshakeTimeout 兜底。
	hubLn := mockHubListen(t)
	go mockHubAcceptLoop(hubLn, mockHubSilentConn)

	e2eWiredAssertHandshakeTimeout(t, hubLn, idL, idT)
}

// mockHubListen 创建监听 127.0.0.1 临时端口的 mock hub 监听器，并注册 t.Cleanup 关闭。
func mockHubListen(t *testing.T) net.Listener {
	t.Helper()
	hubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hubLn.Close() })
	return hubLn
}

// mockHubAcceptLoop 循环接受连接并把每条连接交给 handle 异步处理，直到监听关闭。
func mockHubAcceptLoop(hubLn net.Listener, handle func(net.Conn)) {
	for {
		c, aerr := hubLn.Accept()
		if aerr != nil {
			return
		}
		go handle(c)
	}
}

// mockHubE2EWiredConn 处理一条 E2E 接线连接：读请求、200 升级、启动 T 侧
// ServeE2EStream echo、写 E2E 帧、双向泵送，最后挂起等待 L 侧控制连接生命周期。
func mockHubE2EWiredConn(conn net.Conn, idT, idL *tunnel.Identity) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	if _, lerr := br.ReadString('\n'); lerr != nil {
		return
	}
	contentLength, herr := mockHubReadHeaders(br)
	if herr != nil {
		return
	}
	reqE2E := mockHubReadE2EBody(br, contentLength)
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	// 数据面对端：net.Pipe 模拟「hub → 叶子」方向（真实 hub 中叶子是另一条连接）。
	hubA, hubB := net.Pipe()
	// T 侧（叶子）：先启动 ServeE2EStream 读 hubB（等待帧/握手字节），
	// 之后 hub 同步写帧不阻塞（T 在读）、泵桥接 L 握手字节也不竞争帧序。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tDone := mockHubRunTE2EStream(ctx, hubB, idT, idL)
	// hub-relay-e2e：T 已在读 hubB，同步写帧不阻塞（帧严格先于握手字节）。
	mockHubWriteE2EFrame(hubA, reqE2E)
	// hub 桥接：L 连接 ⇄ hubA（数据面双向泵送；帧已严格先写，握手字节随后经泵到 T）
	go func() { _, _ = io.Copy(hubA, br) }() // 从 br 读（含 bufio 缓冲，不丢握手首字节）
	go func() { _, _ = io.Copy(conn, hubA) }()
	// 等 T 完成或超时；不关闭 conn（泵/连接由 L 侧关闭时自然清理，
	// 提前关闭会让 L 读 echo 时 EOF）。
	select {
	case <-tDone:
	case <-time.After(15 * time.Second):
	}
	// 保持 mock goroutine 存活（不 return），让 L 侧控制连接生命周期。
	select {} //nolint:staticcheck // 测试 mock：挂起直到进程退出（L 侧关闭 conn 结束）
}

// mockHubReadHeaders 读取请求行之后的所有 header 并返回 Content-Length 值；
// 读失败返回 error（调用方需提前结束该连接处理）。
func mockHubReadHeaders(br *bufio.Reader) (int64, error) {
	var contentLength int64
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			return 0, rerr
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.ToLower(strings.TrimSpace(k)) == "content-length" {
			contentLength, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return contentLength, nil
}

// mockHubReadE2EBody 按 Content-Length 读取请求体，解析其中的 E2E 标记
// （hub-relay-e2e 设计）；无请求体返回 false。
func mockHubReadE2EBody(br *bufio.Reader, contentLength int64) bool {
	var reqE2E bool
	if contentLength > 0 {
		bodyBytes := make([]byte, contentLength)
		_, _ = io.ReadFull(br, bodyBytes)
		var req struct {
			E2E bool `json:"e2e,omitempty"`
		}
		_ = json.Unmarshal(bodyBytes, &req)
		reqE2E = req.E2E
		fmt.Printf("mock hub 请求体: %s (E2E=%v)\n", bodyBytes, reqE2E)
	}
	return reqE2E
}

// mockHubRunTE2EStream 在数据面对端启动 ServeE2EStream 并 echo 收到的明文，
// 返回完成信号 chan（T 侧 goroutine 结束后关闭）。
func mockHubRunTE2EStream(ctx context.Context, conn net.Conn, idT, idL *tunnel.Identity) <-chan struct{} {
	tDone := make(chan struct{})
	go func() {
		defer close(tDone)
		dec, derr := ServeE2EStream(ctx, conn, EndToEndOptions{
			Enabled:          true,
			Identity:         idT,
			PeerFingerprints: []string{idL.Fingerprint()},
		})
		if derr != nil {
			return
		}
		defer dec.Close()
		buf := make([]byte, 4096)
		n, rerr := dec.Read(buf)
		if rerr != nil && rerr != io.EOF {
			return
		}
		if _, werr := dec.Write(buf[:n]); werr != nil {
			return
		}
	}()
	return tDone
}

// mockHubWriteE2EFrame 在 E2E 模式下向 hubA 写 DialRequest 帧（严格先于握手字节）。
func mockHubWriteE2EFrame(hubA net.Conn, reqE2E bool) {
	if !reqE2E {
		return
	}
	head, _ := json.Marshal(hub.DialRequest{Dial: "127.0.0.1:7777", E2E: true})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	if _, werr := hubA.Write(lenBuf); werr != nil {
		fmt.Printf("帧长度写失败: %v\n", werr)
		return
	}
	if _, werr := hubA.Write(head); werr != nil {
		fmt.Printf("帧 JSON 写失败: %v\n", werr)
	}
}

// mockHubPlainEchoConn 处理一条裸数据面连接：读请求、200 升级后直接 echo（无 E2E）。
func mockHubPlainEchoConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	if _, lerr := br.ReadString('\n'); lerr != nil {
		return
	}
	contentLength, herr := mockHubReadHeaders(br)
	if herr != nil {
		return
	}
	if contentLength > 0 {
		_, _ = io.CopyN(io.Discard, br, contentLength)
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	_, _ = io.Copy(conn, br) // 裸 echo
}

// mockHubSilentConn 处理一条静默连接：200 升级后不回任何握手字节（对端超时兜底）。
func mockHubSilentConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	if _, lerr := br.ReadString('\n'); lerr != nil {
		return
	}
	contentLength, herr := mockHubReadHeaders(br)
	if herr != nil {
		return
	}
	if contentLength > 0 {
		_, _ = io.CopyN(io.Discard, br, contentLength)
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	// 静默：不读不写，等 L 超时。
	<-time.After(2 * time.Second)
}

// e2eWiredAssertRoundtrip 校验 L 侧 E2E 拨号结果：EndToEnd/Kind 断言 + 明文往返。
func e2eWiredAssertRoundtrip(t *testing.T, hubLn net.Listener, idL, idT *tunnel.Identity) {
	t.Helper()
	svc := client.NewFileClient("http://" + hubLn.Addr().String())
	target := &client.MeshService{Name: "svc", Node: "node-t", Addr: "127.0.0.1:7777"}
	res, derr := DialWithOptions(t.Context(), svc, nil, target, "local-node", DialOptions{
		AllowRelayFallback: true,
		E2E: &EndToEndOptions{
			Enabled:          true,
			Identity:         idL,
			PeerFingerprints: []string{idT.Fingerprint()},
			HandshakeTimeout: 10 * time.Second,
		},
	})
	if derr != nil {
		t.Fatalf("DialWithOptions(E2E) 失败: %v", derr)
	}
	defer res.Conn.Close()
	if !res.EndToEnd {
		t.Fatal("E2E 配置时 Result.EndToEnd 应为 true")
	}
	if res.Kind != KindRelay {
		t.Fatalf("kind = %q, want relay", res.Kind)
	}

	// 明文往返。
	plain := "WIRED-E2E-SECRET"
	if _, werr := res.Conn.Write([]byte(plain)); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	buf := make([]byte, len(plain))
	if _, rerr := io.ReadFull(res.Conn, buf); rerr != nil {
		t.Fatalf("读失败: %v", rerr)
	}
	if string(buf) != plain {
		t.Fatalf("明文回读不一致: got %q, want %q", buf, plain)
	}
}

// e2eWiredAssertPlaintext 校验未配置 E2E（nil）时的裸数据面往返（零回归）。
func e2eWiredAssertPlaintext(t *testing.T, hubLn net.Listener) {
	t.Helper()
	svc := client.NewFileClient("http://" + hubLn.Addr().String())
	target := &client.MeshService{Name: "svc", Node: "node-t", Addr: "127.0.0.1:7777"}
	res, derr := DialWithOptions(t.Context(), svc, nil, target, "local-node", DialOptions{AllowRelayFallback: true})
	if derr != nil {
		t.Fatalf("DialWithOptions(无 E2E) 失败: %v", derr)
	}
	defer res.Conn.Close()
	if res.EndToEnd {
		t.Fatal("未配置 E2E 时 Result.EndToEnd 应为 false（零回归）")
	}
	plain := []byte("plain-relay")
	if _, werr := res.Conn.Write(plain); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	buf := make([]byte, len(plain))
	if _, rerr := io.ReadFull(res.Conn, buf); rerr != nil {
		t.Fatalf("读失败: %v", rerr)
	}
	if string(buf) != string(plain) {
		t.Fatalf("裸回读不一致: got %q, want %q", buf, plain)
	}
}

// e2eWiredAssertHandshakeTimeout 校验 E2E 握手超时（对端不回握手）快速失败不阻塞。
func e2eWiredAssertHandshakeTimeout(t *testing.T, hubLn net.Listener, idL, idT *tunnel.Identity) {
	t.Helper()
	svc := client.NewFileClient("http://" + hubLn.Addr().String())
	target := &client.MeshService{Name: "svc", Node: "node-t", Addr: "127.0.0.1:7777"}
	start := time.Now()
	_, derr := DialWithOptions(t.Context(), svc, nil, target, "local-node", DialOptions{
		AllowRelayFallback: true,
		E2E: &EndToEndOptions{
			Enabled:          true,
			Identity:         idL,
			PeerFingerprints: []string{idT.Fingerprint()},
			HandshakeTimeout: 500 * time.Millisecond,
		},
	})
	if derr == nil {
		t.Fatal("握手超时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("握手超时应快速失败（<=1s），实际 %v", elapsed)
	}
}
