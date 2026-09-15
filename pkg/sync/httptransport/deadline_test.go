// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httptransport

import (
	"errors"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// 本文件 4 个「等 deadline 生效」用例（显式 SetReadDeadline / SetDeadline / 活跃写超时 /
// 活跃读超时）全部跑在 testing/synctest 气泡内：net.Pipe 的阻塞读写被判定为 durably
// blocked（实测 synctest.Wait 正常返回），因此 time.AfterFunc 到点由**虚拟时钟**确定性
// 推进——用例零真实耗时，也不再需要「15s 真实墙钟窗口」（旧内联 3s 窗口实测在 CI ubuntu
// runner 上超时，放宽到 15s 只是止痛）。虚拟时钟同时让「不得早于 deadline 返回」变成无容差
// 断言：气泡内时钟只在全部 goroutine 持久阻塞时才前进，若 Read/Write 提前返回，
// time.Since(start) 必然落在 deadline 之前 ⇒ 必红。

// virtualDeadlineWindow 是气泡内「等 deadline 生效」的兜底窗口。虚拟时钟下它不消耗真实
// 时间（气泡内 goroutine 全部持久阻塞时，时钟直接推进到最近的 timer），正常路径只会命中
// 「deadline 到点」分支；窗口仅在 deadline timer 根本没生效（回归）时命中，把「无限挂起」
// 变成一条确定的失败信息。
const virtualDeadlineWindow = 30 * time.Second

// TestDeadlineConn_SetReadDeadline_ClosesOnExpiry 验证：对空 pipe 设读截止，
// 到点后阻塞的 Read 返回错误（底层连接被强制 Close），而非无限挂起。
// 对应 DoD 8：webrtc 直连路径（MuxStreamConn.SetDeadline no-op）下 http.Transport
// 依赖的 deadline 超时由本包装兜底。
func TestDeadlineConn_SetReadDeadline_ClosesOnExpiry(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	synctest.Test(t, setReadDeadlineClosesOnExpiryBody)
}

// setReadDeadlineClosesOnExpiryBody 在 synctest 气泡内运行：deadline 到期走虚拟时钟。
func setReadDeadlineClosesOnExpiryBody(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := wrapDeadline(clientSide, 0, 0)
	defer dc.Close()

	// deadline 必须在 Read 启动前设置：arm 只在「下一次 Read/Write」时生效（见 deadline.go），
	// 否则读 goroutine 可能先阻塞在没有 timer 的路径上（旧写法依赖调度顺序，是窗口耗尽的根因之一）。
	const deadlineAfter = 80 * time.Millisecond
	start := time.Now()
	if err := dc.SetReadDeadline(start.Add(deadlineAfter)); err != nil {
		t.Fatalf("SetReadDeadline error: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := dc.Read(buf)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("deadline 到点后 Read 应返回错误，got nil")
		}
		if elapsed := time.Since(start); elapsed < deadlineAfter {
			t.Fatalf("Read 早于 deadline 返回（虚拟 elapsed %v < %v），deadline 未生效", elapsed, deadlineAfter)
		}
	case <-time.After(virtualDeadlineWindow):
		t.Fatalf("Read 未在 deadline 后返回（虚拟 %v 窗口耗尽，deadline timer 未生效）", virtualDeadlineWindow)
	}
}

// TestDeadlineConn_ClearDeadline 验证清除 deadline 后读不因过期连接被关闭而失败。
func TestDeadlineConn_ClearDeadline(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := wrapDeadline(clientSide, 0, 0)
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
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	synctest.Test(t, setDeadlineBothDirectionsBody)
}

// setDeadlineBothDirectionsBody 在 synctest 气泡内运行：SetDeadline 到期走虚拟时钟。
// 读、写两个方向各在**独立**的 net.Pipe 上验证（对端既不写也不读，net.Pipe 同步阻塞）：
// deadline 到点后阻塞的调用必须返回非 nil 错误，且不得早于 deadline 返回。
//
// 两个方向不用同一条 conn 并发跑：任一方向到点都会 forceClose **整个**连接（见 deadline.go
// 的 expire），另一方向即使完全没 arm 也会被顺带唤醒 ⇒ 会掩盖「SetDeadline 未作用于该方向」
// 的回归。先跑探针确认：写阻塞在气泡内同样被判定为 durably blocked，虚拟时钟能推进到 deadline。
func setDeadlineBothDirectionsBody(t *testing.T) {
	const deadlineAfter = 60 * time.Millisecond

	// assertDeadlineReleased 在一条新 pipe 上调 SetDeadline(now+deadlineAfter) 后执行 call
	// （call 必阻塞），断言它被 deadline 释放：返回非 nil 错误且虚拟 elapsed >= deadline。
	assertDeadlineReleased := func(dir string, call func(net.Conn) error) {
		clientSide, serverSide := net.Pipe()
		defer serverSide.Close()
		dc := wrapDeadline(clientSide, 0, 0)
		defer dc.Close()

		start := time.Now()
		if err := dc.SetDeadline(start.Add(deadlineAfter)); err != nil {
			t.Fatal(err)
		}
		type result struct {
			err     error
			elapsed time.Duration // 由 goroutine 自己记录：外部等待另一方向会掩盖早返回
		}
		resCh := make(chan result, 1)
		go func() {
			err := call(dc)
			resCh <- result{err: err, elapsed: time.Since(start)}
		}()

		select {
		case r := <-resCh:
			if r.err == nil {
				t.Fatalf("SetDeadline 到点后 %s 应返回错误", dir)
			}
			if r.elapsed < deadlineAfter {
				t.Fatalf("%s 早于 deadline 返回（虚拟 elapsed %v < %v），deadline 未作用于该方向", dir, r.elapsed, deadlineAfter)
			}
		case <-time.After(virtualDeadlineWindow):
			t.Fatalf("%s 未在 deadline 后返回（虚拟 %v 窗口耗尽，SetDeadline 未作用于该方向）", dir, virtualDeadlineWindow)
		}
	}

	// 读方向：对端不写，Read 阻塞。
	assertDeadlineReleased("Read", func(c net.Conn) error {
		_, err := c.Read(make([]byte, 16))
		return err
	})
	// 写方向：对端不读，Write 阻塞；此阶段没有读方向 timer 可以替它唤醒。
	assertDeadlineReleased("Write", func(c net.Conn) error {
		_, err := c.Write([]byte("blocked"))
		return err
	})
}

// TestDeadlineConn_Passthrough_NoDeadline 验证未设 deadline 时读写原样透传。
func TestDeadlineConn_Passthrough_NoDeadline(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := wrapDeadline(clientSide, 0, 0)
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
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := wrapDeadline(clientSide, 0, 0)

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

// TestDeadlineConn_WriteTimeout_ClosesOnExpiry 验证活跃写超时：对端停读时 Write
// 阻塞超过 writeTimeout 即强制关闭连接返回错误（审查 I-1/I-2：HTTP/1.1 不调
// SetWriteDeadline，写路径需活跃超时兜底；DoD 8 对端停读）。
func TestDeadlineConn_WriteTimeout_ClosesOnExpiry(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	synctest.Test(t, writeTimeoutClosesOnExpiryBody)
}

// writeTimeoutClosesOnExpiryBody 在 synctest 气泡内运行：活跃写超时到点走虚拟时钟。
func writeTimeoutClosesOnExpiryBody(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	const writeTimeout = 80 * time.Millisecond
	dc := wrapDeadline(clientSide, 0, writeTimeout)
	defer dc.Close()

	start := time.Now()
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 1<<20) // 1MB，net.Pipe 同步阻塞，服务端不读则 Write 挂起
		_, err := dc.Write(buf)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("写超时到点后 Write 应返回错误")
		}
		if elapsed := time.Since(start); elapsed < writeTimeout {
			t.Fatalf("Write 早于活跃写超时返回（虚拟 elapsed %v < %v）", elapsed, writeTimeout)
		}
	case <-time.After(virtualDeadlineWindow):
		t.Fatalf("Write 未在超时后返回（虚拟 %v 窗口耗尽，活跃写超时未生效）", virtualDeadlineWindow)
	}
}

// TestDeadlineConn_ReadTimeout_ClosesOnExpiry 验证活跃读超时：对端停发时 Read
// 阻塞超过 readTimeout 即强制关闭连接返回错误。
func TestDeadlineConn_ReadTimeout_ClosesOnExpiry(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	synctest.Test(t, readTimeoutClosesOnExpiryBody)
}

// readTimeoutClosesOnExpiryBody 在 synctest 气泡内运行：活跃读超时到点走虚拟时钟。
func readTimeoutClosesOnExpiryBody(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	const readTimeout = 80 * time.Millisecond
	dc := wrapDeadline(clientSide, readTimeout, 0)
	defer dc.Close()

	start := time.Now()
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := dc.Read(buf)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("读超时到点后 Read 应返回错误")
		}
		if elapsed := time.Since(start); elapsed < readTimeout {
			t.Fatalf("Read 早于活跃读超时返回（虚拟 elapsed %v < %v）", elapsed, readTimeout)
		}
	case <-time.After(virtualDeadlineWindow):
		t.Fatalf("Read 未在超时后返回（虚拟 %v 窗口耗尽，活跃读超时未生效）", virtualDeadlineWindow)
	}
}

// TestDeadlineConn_WriteTimeout_ShortWriteReturns 验证 Write 正常快速返回时不触发
// 超时关闭（活跃超时只在阻塞时生效，不误杀正常读写）。
func TestDeadlineConn_WriteTimeout_ShortWriteReturns(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dc := wrapDeadline(clientSide, 0, 200*time.Millisecond)
	defer dc.Close()

	// 服务端先读，客户端写小块数据：写应快速成功，不触发超时。
	go func() {
		buf := make([]byte, 8)
		_, _ = serverSide.Read(buf)
	}()
	if _, err := dc.Write([]byte("ping")); err != nil {
		t.Fatalf("快速 Write 不应触发超时: %v", err)
	}
}
