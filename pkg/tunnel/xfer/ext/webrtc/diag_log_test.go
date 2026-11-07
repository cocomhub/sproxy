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

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
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
	if !verboseEnabled() {
		t.Fatal("SetVerbose(true) 后 verbose 应为 true")
	}
	SetVerbose(false)
	if verboseEnabled() {
		t.Fatal("SetVerbose(false) 后 verbose 应为 false")
	}
}

// webrtcDiagResult 是 TestRoundTrip_HostOnly_StateCallbacksRegistered 双 goroutine 的结果载体。
type webrtcDiagResult struct {
	err  error
	data []byte
}

// diagRoundTripListenThread 监听侧 goroutine：读到后写回，等拨号侧读完再收尾。
func diagRoundTripListenThread(signal *Signal, dialDone <-chan struct{}, listenRes chan<- webrtcDiagResult, wg *sync.WaitGroup) {
	defer wg.Done()
	conn, err := Listen(signal)
	if err != nil {
		listenRes <- webrtcDiagResult{err: err}
		return
	}
	defer conn.Close()
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		listenRes <- webrtcDiagResult{err: err}
		return
	}
	if _, err := conn.Write(buf[:n]); err != nil {
		listenRes <- webrtcDiagResult{err: err}
		return
	}
	listenRes <- webrtcDiagResult{data: buf[:n]}
	<-dialDone
}

// diagRoundTripDialThread 拨号侧 goroutine：写 payload → 读回显 → 通知监听侧收工。
func diagRoundTripDialThread(signal *Signal, payload []byte, dialDone chan<- struct{}, dialRes chan<- webrtcDiagResult, wg *sync.WaitGroup) {
	defer wg.Done()
	conn, err := Dial(signal)
	if err != nil {
		dialRes <- webrtcDiagResult{err: err}
		return
	}
	defer conn.Close()
	if _, werr := conn.Write(payload); werr != nil {
		dialRes <- webrtcDiagResult{err: werr}
		return
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		dialRes <- webrtcDiagResult{err: err}
		return
	}
	dialRes <- webrtcDiagResult{data: buf[:n]}
	close(dialDone)
}

// diagRoundTripCollect 收集两个 goroutine 的结果，任一失败即返回包装后的错误。
func diagRoundTripCollect(dialRes, listenRes <-chan webrtcDiagResult, payload []byte, ctx context.Context) error {
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
	return roundtripErr
}

// diagRoundTripAssertLogs 断言三条诊断日志都出现在捕获输出中。
func diagRoundTripAssertLogs(t *testing.T, logs string) {
	t.Helper()
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

// TestRoundTrip_HostOnly_StateCallbacksRegistered 验证 host-only 内网模式下往返正常，
// 并断言打洞诊断回调（logICEEvent/logPCStateEvent/logCandidateEvents）确实被触发。
func TestRoundTrip_HostOnly_StateCallbacksRegistered(t *testing.T) {
	// loopback 收敛 + 禁用 mDNS，避免 Windows 测试弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
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

	dialRes := make(chan webrtcDiagResult, 1)
	listenRes := make(chan webrtcDiagResult, 1)
	dialDone := make(chan struct{})

	// 等待两个 goroutine 完全结束（含 conn.Close 触发的 state=closed 日志写入），
	// 避免读 logBuf 时后台连接 goroutine 仍在写造成数据竞争。
	var wg sync.WaitGroup
	wg.Add(2)

	// Listen goroutine：读到后写回，等 dial 完成再关闭（避免提前 Close 打断 SCTP）。
	go diagRoundTripListenThread(signal, dialDone, listenRes, &wg)

	// Dial goroutine。
	go diagRoundTripDialThread(signal, payload, dialDone, dialRes, &wg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 两个 goroutine 都成功且数据一致才算过。收集错误而不是直接 t.Fatalf，
	// 确保 wg.Wait() 让两个 goroutine 完全退出后再读 logBuf / 结束测试。
	roundtripErr := diagRoundTripCollect(dialRes, listenRes, payload, ctx)
	wg.Wait()
	if roundtripErr != nil {
		t.Fatalf("roundtrip 失败: %v", roundtripErr)
	}

	// 断言诊断日志确实被触发（host-only 下 ICE 状态流转 + 候选收集必然发生）。
	logs := logBuf.String()
	diagRoundTripAssertLogs(t, logs)
}
