// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

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
				if contentLength > 0 {
					_, _ = io.CopyN(io.Discard, br, contentLength)
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
				// T 侧：读 e2e dial 帧 → ServeE2EStream 解密 → echo。
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
				buf := make([]byte, 4096)
				n, rerr := dec.Read(buf)
				if rerr != nil && rerr != io.EOF {
					return
				}
				if _, werr := dec.Write(buf[:n]); werr != nil {
					return
				}
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
