// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRelayStreamWithHeaders_SendsCustomHeaders：自定义头透传到对端
// （跨 hub 转发防环元数据 X-Relay-Hop / X-Relay-Path）。
func TestRelayStreamWithHeaders_SendsCustomHeaders(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	gotHeaders := make(chan map[string]string, 1)
	addr := startCaptureRelayMock(t, gotHeaders)

	c := NewFileClient("http://" + addr)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := c.RelayStreamWithHeaders(ctx, "leaf", "127.0.0.1:7777", map[string]string{
		"X-Relay-Hop":  "1",
		"X-Relay-Path": "hubA",
	})
	if err != nil {
		t.Fatalf("RelayStreamWithHeaders: %v", err)
	}
	defer conn.Close()

	hdrs := <-gotHeaders
	if hdrs["X-Relay-Hop"] != "1" {
		t.Errorf("X-Relay-Hop = %q, want 1", hdrs["X-Relay-Hop"])
	}
	if hdrs["X-Relay-Path"] != "hubA" {
		t.Errorf("X-Relay-Path = %q, want hubA", hdrs["X-Relay-Path"])
	}

	// 数据面 echo 往返验证升级成功。
	if _, werr := conn.Write([]byte("ping!123")); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	got := make([]byte, 8)
	if _, rerr := io.ReadFull(conn, got); rerr != nil {
		t.Fatalf("read echo: %v", rerr)
	}
	if string(got) != "ping!123" {
		t.Fatalf("echo mismatch: got %q", got)
	}
}

// startCaptureRelayMock 启动一个单连接的 mock 中继服务：读请求行+头（原样大小写），
// 把 headers 发到 out，回 CONNECT 200，并 echo 一条 8 字节数据以验证数据面。
func startCaptureRelayMock(t *testing.T, out chan map[string]string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go serveCaptureRelayOnce(ln, out)
	return ln.Addr().String()
}

// serveCaptureRelayOnce 接受单连接，读取请求头并回 CONNECT 200 后 echo 8 字节。
func serveCaptureRelayOnce(ln net.Listener, out chan map[string]string) {
	conn, aerr := ln.Accept()
	if aerr != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	headers, contentLength := readRelayHeadersCase(br)
	out <- headers
	if contentLength > 0 {
		if _, rerr := io.CopyN(io.Discard, br, int64(contentLength)); rerr != nil {
			return
		}
	}
	// 回 CONNECT 200
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	// echo 一条数据（验证升级后数据面）
	echoRelayBytes(conn, br, 8)
}

// readRelayHeadersCase 读取请求头直到空行，返回原样大小写的 headers 与 Content-Length。
func readRelayHeadersCase(br *bufio.Reader) (map[string]string, int) {
	contentLength := 0
	hdrs := map[string]string{}
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			return hdrs, contentLength
		}
		if line == "\r\n" || line == "\n" {
			return hdrs, contentLength
		}
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			k = strings.TrimSpace(k)
			hdrs[k] = strings.TrimSpace(v)
			if strings.EqualFold(k, "Content-Length") {
				contentLength, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
	}
}

// echoRelayBytes 从 br 读 n 字节并原样写回 conn（验证 CONNECT 升级后数据面）。
func echoRelayBytes(conn net.Conn, br *bufio.Reader, n int) {
	buf := make([]byte, n)
	if _, rerr := io.ReadFull(br, buf); rerr != nil {
		return
	}
	_, _ = conn.Write(buf)
}

// TestRelayStreamWithHeaders_RejectsCRLFHeader：防 CRLF 注入——含 \r\n 的头值
// 被拒绝（不写入请求）。
func TestRelayStreamWithHeaders_RejectsCRLFHeader(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	c := NewFileClient("http://" + ln.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = c.RelayStreamWithHeaders(ctx, "leaf", "127.0.0.1:7777", map[string]string{
		"X-Evil": "x\r\nInjected: y",
	})
	if err == nil {
		t.Fatalf("含 CRLF 的头应被拒绝")
	}
	if !strings.Contains(err.Error(), "非法自定义头") {
		t.Fatalf("错误信息应说明非法头, got %v", err)
	}
}

// TestRelayStream_ErrorStatus_RelayStatusError：非 200 状态返回 *RelayStatusError
// 且携带状态码（跨 hub 转发据此映射错误）。
func TestRelayStream_ErrorStatus_RelayStatusError(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	addr := startRelayStatusMock(t, "HTTP/1.1 508 Loop Detected\r\nContent-Length: 5\r\n\r\nloopy")

	c := NewFileClient("http://" + addr)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := c.RelayStream(ctx, "leaf", "127.0.0.1:7777")
	if err == nil {
		t.Fatalf("非 200 应返回 error")
	}
	var rse *RelayStatusError
	if !errors.As(err, &rse) {
		t.Fatalf("错误类型应为 *RelayStatusError, got %T: %v", err, err)
	}
	if rse.Status != 508 {
		t.Fatalf("RelayStatusError.Status = %d, want 508", rse.Status)
	}
	if !strings.Contains(err.Error(), "508") {
		t.Fatalf("错误信息应含状态码, got %v", err)
	}
}

// startRelayStatusMock 启动一个单连接的 mock 中继服务：读请求头后直接写固定状态行。
func startRelayStatusMock(t *testing.T, statusLine string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go serveRelayStatusOnce(ln, statusLine)
	return ln.Addr().String()
}

// serveRelayStatusOnce 接受单连接，读完请求头与 body 后写固定状态行。
func serveRelayStatusOnce(ln net.Listener, statusLine string) {
	conn, aerr := ln.Accept()
	if aerr != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	contentLength := 0
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			return
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			contentLength, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if contentLength > 0 {
		_, _ = io.CopyN(io.Discard, br, int64(contentLength))
	}
	_, _ = fmt.Fprintf(conn, "%s", statusLine)
}
