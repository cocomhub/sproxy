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
	hubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hubLn.Close()
	go func() {
		for {
			c, aerr := hubLn.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				if _, lerr := br.ReadString('\n'); lerr != nil {
					return
				}
				var contentLength int64
				for {
					line, rerr := br.ReadString('\n')
					if rerr != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
					k, v, ok := strings.Cut(line, ":")
					if ok && strings.ToLower(strings.TrimSpace(k)) == "content-length" {
						contentLength, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
					}
				}
				// 读请求体（含 E2E 标记——hub-relay-e2e 设计）
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
				_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
				// 数据面对端：net.Pipe 模拟「hub → 叶子」方向（真实 hub 中叶子是另一条连接）。
				hubA, hubB := net.Pipe()
				// T 侧（叶子）：先启动 ServeE2EStream 读 hubB（等待帧/握手字节），
				// 之后 hub 同步写帧不阻塞（T 在读）、泵桥接 L 握手字节也不竞争帧序。
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				tDone := make(chan struct{})
				go func() {
					defer close(tDone)
					dec, derr := ServeE2EStream(ctx, hubB, EndToEndOptions{
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
				// hub-relay-e2e：T 已在读 hubB，同步写帧不阻塞（帧严格先于握手字节）。
				if reqE2E {
					head, _ := json.Marshal(hub.DialRequest{Dial: "127.0.0.1:7777", E2E: true})
					lenBuf := make([]byte, 4)
					binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
					if _, werr := hubA.Write(lenBuf); werr != nil {
						fmt.Printf("帧长度写失败: %v\n", werr)
						return
					}
					if _, werr := hubA.Write(head); werr != nil {
						fmt.Printf("帧 JSON 写失败: %v\n", werr)
						return
					}
				}
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
			}(c)
		}
	}()

	// L 侧：DialWithOptions 配 E2E。
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

// TestDial_NoE2EConfigPlaintext 验证未配置 E2E（nil）时保持现有裸数据面（零回归）。
func TestDial_NoE2EConfigPlaintext(t *testing.T) {
	t.Parallel()
	// mock hub：relay/stream → 200 升级 → 数据面裸 echo（无 E2E）。
	hubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hubLn.Close()
	go func() {
		for {
			c, aerr := hubLn.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				if _, lerr := br.ReadString('\n'); lerr != nil {
					return
				}
				var contentLength int64
				for {
					line, rerr := br.ReadString('\n')
					if rerr != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
					k, v, ok := strings.Cut(line, ":")
					if ok && strings.ToLower(strings.TrimSpace(k)) == "content-length" {
						contentLength, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
					}
				}
				if contentLength > 0 {
					_, _ = io.CopyN(io.Discard, br, contentLength)
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
				_, _ = io.Copy(conn, br) // 裸 echo
			}(c)
		}
	}()

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

// TestDial_E2EHandshakeTimeout 验证 E2E 握手超时（对端不回握手）不无限阻塞：
// HandshakeTimeout 到期返回错误（DoS 面防护，任务 2 审查 Minor-1）。
func TestDial_E2EHandshakeTimeout(t *testing.T) {
	t.Parallel()
	idL, _ := tunnel.GenerateIdentity()
	idT, _ := tunnel.GenerateIdentity()

	// mock hub：200 升级后数据面静默（不回握手）→ E2E 握手挂起，HandshakeTimeout 兜底。
	hubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hubLn.Close()
	go func() {
		for {
			c, aerr := hubLn.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				if _, lerr := br.ReadString('\n'); lerr != nil {
					return
				}
				var contentLength int64
				for {
					line, rerr := br.ReadString('\n')
					if rerr != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
					k, v, ok := strings.Cut(line, ":")
					if ok && strings.ToLower(strings.TrimSpace(k)) == "content-length" {
						contentLength, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
					}
				}
				if contentLength > 0 {
					_, _ = io.CopyN(io.Discard, br, contentLength)
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
				// 静默：不读不写，等 L 超时。
				<-time.After(2 * time.Second)
			}(c)
		}
	}()

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
