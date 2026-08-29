// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/logging"
)

// lockedBuffer 是并发安全的写入缓冲，用于测试捕获 slog 输出。
// slog handler 可能被 pion 后台 goroutine（如异步连接状态变化）并发写入，
// 直接读普通 bytes.Buffer 会产生数据竞争。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestConfigureLoggerFactory_Verbose 验证 verbose 时 ice/dtls/sctp/webrtc scope 提升到 TRACE。
func TestConfigureLoggerFactory_Verbose(t *testing.T) {
	f := configureLoggerFactory(true)
	dlf, ok := f.(*logging.DefaultLoggerFactory)
	if !ok {
		t.Fatalf("期望 DefaultLoggerFactory, got %T", f)
	}
	for _, scope := range []string{"ice", "dtls", "sctp", "webrtc"} {
		if lv, found := dlf.ScopeLevels[scope]; !found || lv != logging.LogLevelTrace {
			t.Fatalf("scope %s: verbose 时应为 Trace(level=%v, found=%v)", scope, lv, found)
		}
	}
}

// TestConfigureLoggerFactory_Default 验证默认（非 verbose）时 4 个关键 scope 显式设为 Error。
// 不再依赖 PION_LOG_* 环境变量的单例：无论 env 如何，默认始终无噪音。
func TestConfigureLoggerFactory_Default(t *testing.T) {
	f := configureLoggerFactory(false)
	dlf, ok := f.(*logging.DefaultLoggerFactory)
	if !ok {
		t.Fatalf("期望 DefaultLoggerFactory, got %T", f)
	}
	for _, scope := range []string{"ice", "dtls", "sctp", "webrtc"} {
		if lv, found := dlf.ScopeLevels[scope]; !found || lv != logging.LogLevelError {
			t.Fatalf("scope %s: 默认时应为 Error(level=%v, found=%v)", scope, lv, found)
		}
	}
}

// TestSetVerbose_GloballyEnabled 验证 SetVerbose 开关写入全局变量（供 newPC 使用）。
func TestSetVerbose_GloballyEnabled(t *testing.T) {
	t.Cleanup(func() { SetVerbose(false) })
	SetVerbose(true)
	if !verbose {
		t.Fatal("SetVerbose(true) 后 verbose 应为 true")
	}
	SetVerbose(false)
	if verbose {
		t.Fatal("SetVerbose(false) 后 verbose 应为 false")
	}
}

// TestRoundTrip_HostOnly_StateCallbacksRegistered 验证 host-only 内网模式下往返正常，
// 并断言打洞诊断回调（logICEEvent/logPCStateEvent/logCandidateEvents）确实被触发。
func TestRoundTrip_HostOnly_StateCallbacksRegistered(t *testing.T) {
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })

	// 捕获 slog 输出，验证状态回调被触发（Debug 级起全捕获）。
	// 用互斥保护的缓冲：pion 内部 goroutine 可能异步写日志（如 state=closed）。
	logBuf := &lockedBuffer{}
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	signal := NewSignal()
	payload := []byte("hello webrtc diagnostics")

	type result struct {
		err  error
		data []byte
	}
	dialRes := make(chan result, 1)
	listenRes := make(chan result, 1)
	dialDone := make(chan struct{})

	// 等待两个 goroutine 完全结束（含 conn.Close 触发的 state=closed 日志写入），
	// 避免读 logBuf 时后台连接 goroutine 仍在写造成数据竞争。
	var wg sync.WaitGroup
	wg.Add(2)

	// Listen goroutine：读到后写回，等 dial 完成再关闭（避免提前 Close 打断 SCTP）。
	go func() {
		defer wg.Done()
		conn, err := Listen(signal)
		if err != nil {
			listenRes <- result{err: err}
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			listenRes <- result{err: err}
			return
		}
		if _, err := conn.Write(buf[:n]); err != nil {
			listenRes <- result{err: err}
			return
		}
		listenRes <- result{data: buf[:n]}
		<-dialDone
	}()

	// Dial goroutine。
	go func() {
		defer wg.Done()
		conn, err := Dial(signal)
		if err != nil {
			dialRes <- result{err: err}
			return
		}
		defer conn.Close()
		if _, werr := conn.Write(payload); werr != nil {
			dialRes <- result{err: werr}
			return
		}
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			dialRes <- result{err: err}
			return
		}
		dialRes <- result{data: buf[:n]}
		close(dialDone)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 两个 goroutine 都成功且数据一致才算过。收集错误而不是直接 t.Fatalf，
	// 确保 wg.Wait() 让两个 goroutine 完全退出后再读 logBuf / 结束测试。
	okDial, okListen := false, false
	var roundtripErr error
	for !okDial || !okListen {
		select {
		case r := <-dialRes:
			switch {
			case r.err != nil:
				roundtripErr = fmt.Errorf("dial side: %w", r.err)
			case string(r.data) != string(payload):
				roundtripErr = fmt.Errorf("dial 收到数据不匹配: %q", string(r.data))
			default:
				okDial = true
			}
		case r := <-listenRes:
			switch {
			case r.err != nil:
				roundtripErr = fmt.Errorf("listen side: %w", r.err)
			case string(r.data) != string(payload):
				roundtripErr = fmt.Errorf("listen 收到数据不匹配: %q", string(r.data))
			default:
				okListen = true
			}
		case <-ctx.Done():
			roundtripErr = ctx.Err()
		}
		if roundtripErr != nil {
			break
		}
	}
	wg.Wait()
	if roundtripErr != nil {
		t.Fatalf("roundtrip 失败: %v", roundtripErr)
	}

	// 断言诊断日志确实被触发（host-only 下 ICE 状态流转 + 候选收集必然发生）。
	logs := logBuf.String()
	for _, want := range []string{
		"webrtc: ICE 状态变化",
		"webrtc: 连接状态变化",
		"webrtc: 收集到 ICE 候选",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("诊断日志缺失 %q；捕获输出:\n%s", want, logs)
		}
	}
}
