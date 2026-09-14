// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// scriptedListener 是 acceptLoop 的白盒假 listener：先按脚本吐若干错误，之后阻塞在
// done 上直到 Close（返回 net.ErrClosed）。
//
// 用于确定性地注入 Accept 错误——真实环境里 EMFILE/ENFILE/ENOBUFS/ENOMEM 无法按需触发。
type scriptedListener struct {
	mu      sync.Mutex
	errs    []error
	accepts int
	closed  bool
	done    chan struct{}
	addr    net.Addr
}

func newScriptedListener(errs ...error) *scriptedListener {
	return &scriptedListener{errs: errs, done: make(chan struct{}),
		addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}}
}

func (s *scriptedListener) Accept() (net.Conn, error) {
	s.mu.Lock()
	s.accepts++
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	<-s.done
	return nil, net.ErrClosed
}

func (s *scriptedListener) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}

func (s *scriptedListener) Addr() net.Addr { return s.addr }

func (s *scriptedListener) acceptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepts
}

func (s *scriptedListener) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func waitForCond(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("条件在超时内未满足")
}

func TestRetryableAcceptError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"EMFILE", &net.OpError{Op: "accept", Err: syscall.EMFILE}, true},
		{"ENFILE", &net.OpError{Op: "accept", Err: syscall.ENFILE}, true},
		{"ENOBUFS", &net.OpError{Op: "accept", Err: syscall.ENOBUFS}, true},
		{"ENOMEM", &net.OpError{Op: "accept", Err: syscall.ENOMEM}, true},
		{"ErrClosed 非瞬时", net.ErrClosed, false},
		{"普通错误非瞬时", errors.New("boom"), false},
		{"nil 非瞬时", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableAcceptError(tc.err); got != tc.want {
				t.Fatalf("retryableAcceptError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestNextAcceptBackoff(t *testing.T) {
	if got := nextAcceptBackoff(0); got != minAcceptBackoff {
		t.Fatalf("首次退避 = %v, want %v", got, minAcceptBackoff)
	}
	if got := nextAcceptBackoff(maxAcceptBackoff); got != maxAcceptBackoff {
		t.Fatalf("封顶退避 = %v, want %v", got, maxAcceptBackoff)
	}
	if got := nextAcceptBackoff(minAcceptBackoff); got != 2*minAcceptBackoff {
		t.Fatalf("翻倍退避 = %v, want %v", got, 2*minAcceptBackoff)
	}
}

// TestAcceptLoop_RetriesTransientAcceptError 钉住：瞬时 Accept 错误必须退避重试，
// 不得让 accept 循环永久退出（否则 listener 仍绑定、backlog 继续完成握手，服务静默死亡）。
func TestAcceptLoop_RetriesTransientAcceptError(t *testing.T) {
	fl := newScriptedListener(&net.OpError{Op: "accept", Err: syscall.EMFILE})
	l := &RemoteReadListener{
		ln: fl, logger: testutil.DiscardLogger(), cfg: Default(), acceptDone: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	exited := make(chan struct{})
	go func() {
		l.acceptLoop(ctx, nil, nil, nil)
		close(exited)
	}()

	// 瞬时错误后应再次调用 Accept（且阻塞在假 listener 上），而不是退出。
	waitForCond(t, 3*time.Second, func() bool { return fl.acceptCount() >= 2 })
	select {
	case <-exited:
		t.Fatal("acceptLoop 在瞬时 Accept 错误后退出（应退避重试）")
	default:
	}
	if fl.isClosed() {
		t.Fatal("瞬时错误不应关闭 listener")
	}

	cancel()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel 后 acceptLoop 未退出")
	}
}

// TestAcceptLoop_ClosesListenerOnFatalAcceptError 钉住：不可重试的致命 Accept 错误下，
// 循环退出前必须关闭 listener——否则端口仍可连但无人服务（本次 CI flake 的根因形态）。
func TestAcceptLoop_ClosesListenerOnFatalAcceptError(t *testing.T) {
	fl := newScriptedListener(errors.New("fatal accept failure"))
	l := &RemoteReadListener{
		ln: fl, logger: testutil.DiscardLogger(), cfg: Default(), acceptDone: make(chan struct{}),
	}

	l.acceptLoop(t.Context(), nil, nil, nil) // 同步调用：致命错误应立即返回

	if !fl.isClosed() {
		t.Fatal("致命 Accept 错误后 listener 未关闭（端口仍在收连接）")
	}
	select {
	case <-l.acceptDone:
	default:
		t.Fatal("acceptLoop 退出时未关闭 acceptDone 信号")
	}
}

// TestRemoteWriteAcceptLoop_ClosesListenerOnFatalAcceptError 与只读面同构的写面同等约束。
func TestRemoteWriteAcceptLoop_ClosesListenerOnFatalAcceptError(t *testing.T) {
	fl := newScriptedListener(fmt.Errorf("fatal accept failure"))
	l := &RemoteWriteListener{
		ln: fl, logger: testutil.DiscardLogger(), cfg: Default(), acceptDone: make(chan struct{}),
	}

	l.acceptLoop(t.Context(), nil, nil, nil)

	if !fl.isClosed() {
		t.Fatal("写面致命 Accept 错误后 listener 未关闭（端口仍在收连接）")
	}
}
