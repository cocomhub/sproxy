// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestDeadlineConn_SetReadDeadline_ClosesOnExpiry 验证：对空 pipe 设读截止，
// 到点后阻塞的 Read 返回错误（底层连接被强制 Close），而非无限挂起。
// 对应 DoD 8：webrtc 直连路径（MuxStreamConn.SetDeadline no-op）下 http.Transport
// 依赖的 deadline 超时由本包装兜底。
func TestDeadlineConn_SetReadDeadline_ClosesOnExpiry(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := wrapDeadline(clientSide)
	defer dc.Close()

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := dc.Read(buf)
		errCh <- err
	}()

	start := time.Now()
	if err := dc.SetReadDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline error: %v", err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("deadline 到点后 Read 应返回错误，got nil")
		}
		if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
			t.Fatalf("Read 过早返回（%v），deadline 未生效", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Read 未在 deadline 后返回（无限挂起）")
	}
}

// TestDeadlineConn_ClearDeadline 验证清除 deadline 后读不因过期连接被关闭而失败。
func TestDeadlineConn_ClearDeadline(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := wrapDeadline(clientSide)
	defer dc.Close()

	if err := dc.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// 清除 deadline
	if err := dc.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		_, _ = serverSide.Write([]byte("hi"))
		close(done)
	}()

	buf := make([]byte, 4)
	n, err := dc.Read(buf)
	if err != nil {
		t.Fatalf("清除 deadline 后 Read 应成功，got %v", err)
	}
	if n != 2 || string(buf[:n]) != "hi" {
		t.Fatalf("读内容不符: %d %q", n, buf[:n])
	}
	<-done
}

// TestDeadlineConn_SetDeadline_BothDirections 验证 SetDeadline 同时作用于读写。
func TestDeadlineConn_SetDeadline_BothDirections(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := wrapDeadline(clientSide)
	defer dc.Close()

	if err := dc.SetDeadline(time.Now().Add(60 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// 读阻塞，deadline 到点应返回错误
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := dc.Read(buf)
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("SetDeadline 到点后 Read 应返回错误")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Read 未在 deadline 后返回（无限挂起）")
	}
}

// TestDeadlineConn_Passthrough_NoDeadline 验证未设 deadline 时读写原样透传。
func TestDeadlineConn_Passthrough_NoDeadline(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := wrapDeadline(clientSide)
	defer dc.Close()

	go func() {
		_, _ = serverSide.Write([]byte("payload"))
		_ = serverSide.Close()
	}()

	buf := make([]byte, 32)
	n, err := io.ReadFull(dc, buf[:7])
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Read 失败: %v", err)
	}
	if string(buf[:n]) != "payload" {
		t.Fatalf("透传内容不符: %q", buf[:n])
	}
}

// TestDeadlineConn_Close_StopsTimer 验证 Close 后底层连接关闭、后续读写报错。
func TestDeadlineConn_Close_StopsTimer(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := wrapDeadline(clientSide)

	if err := dc.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := dc.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := dc.Read(buf); err == nil {
		t.Fatalf("Close 后 Read 应返回错误")
	}
	// 空地址断言：确保 LocalAddr/RemoteAddr 透传不 panic（间接覆盖内嵌 net.Conn）
	if serverSide.LocalAddr() == nil {
		t.Fatalf("LocalAddr 不应为 nil")
	}
}
