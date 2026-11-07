// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestViaRelay_E2EWired 验证 via-relay:<X> 多跳 E2E 全链路接线：
// L（--e2e）→ RelayStream 到 mock hub → hub 在 X mux 上 Open 流（写 dial 帧）→
// L conn 桥接到该流 → X 侧 relay.Serve 收到 e2e 帧 + Path="via-relay" → 透传分支
// （拨 T + 改写帧 Path 置空 + 泵密文，不见明文）→ T 侧 ServeE2EStream 解密 echo
// （L⇄T 端到端加密）。
//
// 结构（真实 hub 语义）：
//   - X 侧：pipeA（xfer.Conn）→ mux.New(RoleListener) → relay.Serve（透传分支）
//   - hub 侧：pipeB（xfer.Conn）→ mux.New(RoleDialer) → Open 流 → L conn ↔ 流双向桥接
//   - T 侧：TCP listener + ServeE2EStream 解密 echo
//
// 断言：① 明文往返成功（全链路 E2E 生效）；② Result.EndToEnd=true；③ Kind=via-node；
// ④ X mux 载体通道（pipeA 侧记录的字节）不见明文（T1 红线：X 持 SK 读不到明文）。
func TestViaRelay_E2EWired(t *testing.T) {
	t.Parallel()
	idL, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成 L 身份失败: %v", err)
	}
	idT, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成 T 身份失败: %v", err)
	}

	// T 侧：ServeE2EStream 解密 echo。
	tLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tLn.Close()
	tAddr := tLn.Addr().String()
	go viaRelayServeEchoAcceptLoop(tLn, idT, idL)

	// X mux 载体：xfertest.Pipe 对。X 侧记录载体字节（断言不见明文）。
	pipeA, pipeB := xfertest.Pipe()
	var xMu sync.Mutex
	var xBytes []byte
	recA := &recordingXfer{Conn: pipeA, mu: &xMu, buf: &xBytes}

	// X 侧：mux + relay.Serve（透传分支，E2EServe nil = X 不解密）。
	xCtx := t.Context()
	go func() {
		m := mux.New(recA, mux.RoleListener)
		defer m.Close()
		_ = relay.Serve(xCtx, m, "http://127.0.0.1:1", true,
			&http.Client{Timeout: 5 * time.Second}, discardLogger(),
			relay.ServeOptions{
				DialPolicy: func(addr string) (string, bool) { return addr, true },
				// E2EServe nil：X 是中间节点，透传分支不依赖 E2EServe（不解密）。
			})
	}()

	// mock hub：裸 TCP，/api/hub/nodes + /api/relay/stream（升级后：pipeB 开 mux →
	// Open 流 → L conn ↔ 流双向桥接）。
	hubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hubLn.Close()
	go viaRelayHubAcceptLoop(hubLn, tAddr, pipeB)

	// L 侧：viaNodeProvider.Expand → via-relay:x1 候选 → Dial 配 E2E。
	svc := client.NewFileClient("http://" + hubLn.Addr().String())
	p := viaNodeProvider{}
	target := &client.MeshService{Name: "svc", Node: "T", Addr: tAddr}
	cands := p.Expand(context.Background(), svc, target)
	relayCand := viaRelayFindViaRelayCandidate(cands)
	if relayCand == nil {
		t.Fatalf("缺少 via-relay:node-x1 候选, got %v", candIDs(cands))
	}

	res, derr := relayCand.Dial(t.Context(), svc, nil, target, "local-node", DialOptions{
		E2E: &EndToEndOptions{
			Enabled:          true,
			Identity:         idL,
			PeerFingerprints: []string{idT.Fingerprint()},
			HandshakeTimeout: 10 * time.Second,
		},
	})
	if derr != nil {
		t.Fatalf("via-relay E2E Dial 失败: %v", derr)
	}
	defer res.Conn.Close()
	if !res.EndToEnd {
		t.Fatal("E2E 配置时 Result.EndToEnd 应为 true")
	}
	if res.Kind != KindViaNode {
		t.Fatalf("kind = %q, want via-node", res.Kind)
	}

	// 明文往返（L⇄T 端到端加密）。
	plain := "VIA-RELAY-E2E-SECRET"
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

	// X 载体通道不见明文（红线：X 持 SK 读不到明文）。
	xMu.Lock()
	defer xMu.Unlock()
	if strings.Contains(string(xBytes), plain) {
		t.Fatalf("X 中间节点记录了明文（应只见密文），X 持 SK 也能读明文——违反 T1 红线")
	}
}

// viaRelayServeEchoAcceptLoop 是 T 侧 echo 服务的 accept 主循环：新连接各自开
// goroutine 跑 viaRelayServeEchoConn（解密 echo），监听关闭时 Accept 报错返回。
func viaRelayServeEchoAcceptLoop(tLn net.Listener, idT, idL *tunnel.Identity) {
	for {
		c, aerr := tLn.Accept()
		if aerr != nil {
			return
		}
		go viaRelayServeEchoConn(c, idT, idL)
	}
}

// viaRelayServeEchoConn 在单条 T 侧连接上跑 ServeE2EStream 解密 echo：
// 解密后读到的明文原样写回（io.Copy(dec, dec)）。握手失败即静默返回。
func viaRelayServeEchoConn(conn net.Conn, idT, idL *tunnel.Identity) {
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dec, derr := ServeE2EStream(ctx, conn, EndToEndOptions{
		Enabled:          true,
		Identity:         idT,
		PeerFingerprints: []string{idL.Fingerprint()},
	})
	if derr != nil {
		return
	}
	defer dec.Close()
	_, _ = io.Copy(dec, dec) // 解密 echo
}

// viaRelayHubAcceptLoop 是 mock hub 的 accept 主循环：新连接各自开 goroutine 处理
// （viaRelayHubHandleConn），监听关闭时 Accept 报错返回。
func viaRelayHubAcceptLoop(hubLn net.Listener, tAddr string, pipeB xfer.Conn) {
	for {
		c, aerr := hubLn.Accept()
		if aerr != nil {
			return
		}
		go viaRelayHubHandleConn(c, tAddr, pipeB)
	}
}

// viaRelayHubHandleConn 处理一条 mock hub 连接：读请求 → 读 body（relay/stream 才解析
// e2e 字段）→ 按请求行分发（/api/hub/nodes、/api/relay/stream、其余 404）。
func viaRelayHubHandleConn(conn net.Conn, tAddr string, pipeB xfer.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	reqLine, contentLength, lerr := viaRelayReadRequest(br)
	if lerr != nil {
		return
	}
	reqE2E := viaRelayReadBodyE2E(br, reqLine, contentLength)
	switch {
	case strings.Contains(reqLine, "/api/hub/nodes"):
		viaRelayWriteNodes(conn)
	case strings.Contains(reqLine, "/api/relay/stream"):
		viaRelayHandleRelayStream(conn, br, pipeB, tAddr, reqE2E)
	default:
		_, _ = conn.Write([]byte("HTTP/1.1 404 Not Found\r\n\r\n"))
	}
}

// viaRelayReadRequest 读取请求行并跳过请求头（到空行止），返回请求行与 Content-Length。
// 客户端断开或读错误时返回 err。
func viaRelayReadRequest(br *bufio.Reader) (string, int64, error) {
	reqLine, lerr := br.ReadString('\n')
	if lerr != nil {
		return "", 0, lerr
	}
	var contentLength int64
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			return "", 0, rerr
		}
		if line == "\r\n" || line == "\n" {
			return reqLine, contentLength, nil
		}
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.ToLower(strings.TrimSpace(k)) == "content-length" {
			contentLength, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
}

// viaRelayReadBodyE2E 按请求分发读 body：relay/stream 请求解析 JSON 返回 e2e 字段，
// 其它带 body 请求直接丢弃 body；无 body 返回 false。
func viaRelayReadBodyE2E(br *bufio.Reader, reqLine string, contentLength int64) bool {
	var reqE2E bool
	if contentLength > 0 && strings.Contains(reqLine, "/api/relay/stream") {
		bodyBytes := make([]byte, contentLength)
		_, _ = io.ReadFull(br, bodyBytes)
		var req struct {
			E2E bool `json:"e2e,omitempty"`
		}
		_ = json.Unmarshal(bodyBytes, &req)
		reqE2E = req.E2E
	} else if contentLength > 0 {
		_, _ = io.CopyN(io.Discard, br, contentLength)
	}
	return reqE2E
}

// viaRelayWriteNodes 响应 /api/hub/nodes：返回带 outbound-dial 能力的 node-x1 列表。
func viaRelayWriteNodes(conn net.Conn) {
	nodesJSON := `[{"id":"node-x1","capabilities":["outbound-dial"]}]`
	_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(nodesJSON))+"\r\n\r\n")
	_, _ = conn.Write([]byte(nodesJSON))
}

// viaRelayHandleRelayStream 处理 /api/relay/stream 升级：写 200 → pipeB 开 mux →
// Open 流（E2E 时写 e2e+via-relay 帧）→ L conn ↔ 流双向桥接，任一侧 EOF 即返回。
func viaRelayHandleRelayStream(conn net.Conn, br *bufio.Reader, pipeB xfer.Conn, tAddr string, reqE2E bool) {
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	// hub 侧：pipeB 开 mux → Open 流 → L conn ↔ 流桥接。
	m := mux.New(pipeB, mux.RoleDialer)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, oerr := m.Open(ctx)
	if oerr != nil {
		return
	}
	defer stream.Close()
	// hub-relay-e2e：E2E 请求时 hub 写 e2e+via-relay 帧给 X（X 透传分支读）
	if reqE2E {
		head, _ := json.Marshal(hub.DialRequest{Dial: tAddr, E2E: true, Path: "via-relay"})
		lenBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
		_, _ = stream.Write(lenBuf)
		_, _ = stream.Write(head)
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stream, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, stream); done <- struct{}{} }()
	<-done
}

// viaRelayFindViaRelayCandidate 在 viaNodeProvider 展开的候选里找 via-relay:node-x1 入口，
// 返回该候选；缺失时返回 nil（由调用方断言是否必须存在）。
func viaRelayFindViaRelayCandidate(cands []Candidate) *Candidate {
	for i := range cands {
		if cands[i].ID == "via-relay:node-x1" {
			return &cands[i]
		}
	}
	return nil
}

// recordingXfer 包装 xfer.Conn，把 Send/Receive 字节记录到共享 buffer。
type recordingXfer struct {
	xfer.Conn
	mu  *sync.Mutex
	buf *[]byte
}

func (r *recordingXfer) Send(ctx context.Context, msg []byte) error {
	r.mu.Lock()
	*r.buf = append(*r.buf, msg...)
	r.mu.Unlock()
	return r.Conn.Send(ctx, msg)
}

func (r *recordingXfer) Receive(ctx context.Context) ([]byte, error) {
	msg, err := r.Conn.Receive(ctx)
	if err == nil {
		r.mu.Lock()
		*r.buf = append(*r.buf, msg...)
		r.mu.Unlock()
	}
	return msg, err
}

// discardLogger 返回丢弃日志的 slog.Logger。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
