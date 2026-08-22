// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"context"
	"testing"
	"time"

	"github.com/pion/logging"
)

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

// TestConfigureLoggerFactory_Default 验证默认（非 verbose）时保持 Error（无噪音）。
func TestConfigureLoggerFactory_Default(t *testing.T) {
	f := configureLoggerFactory(false)
	dlf, ok := f.(*logging.DefaultLoggerFactory)
	if !ok {
		t.Fatalf("期望 DefaultLoggerFactory, got %T", f)
	}
	for _, scope := range []string{"ice", "dtls", "sctp", "webrtc"} {
		if lv, found := dlf.ScopeLevels[scope]; found {
			t.Fatalf("scope %s: 默认不应被覆盖, level=%v", scope, lv)
		}
	}
}

// TestSetVerbose_GloballyEnabled 验证 SetVerbose 开关写入全局变量（供 newPC 使用）。
func TestSetVerbose_GloballyEnabled(t *testing.T) {
	SetVerbose(true)
	if !verbose {
		t.Fatal("SetVerbose(true) 后 verbose 应为 true")
	}
	SetVerbose(false)
	if verbose {
		t.Fatal("SetVerbose(false) 后 verbose 应为 false")
	}
}

// TestRoundTrip_WithStateCallbacks 验证接入状态回调后真实打洞往返仍正常
// （host-only 内网模式，不依赖外部 STUN），防止回调注册破坏既有连接流程。
func TestRoundTrip_WithStateCallbacks(t *testing.T) {
	SetHostOnly(true)
	defer SetHostOnly(false)

	signal := NewSignal()
	payload := []byte("hello webrtc diagnostics")

	type result struct {
		err  error
		data []byte
	}
	dialRes := make(chan result, 1)
	listenRes := make(chan result, 1)
	dialDone := make(chan struct{})

	// Listen goroutine：读到后写回，等 dial 完成再关闭（避免提前 Close 打断 SCTP）。
	go func() {
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

	// 两个 goroutine 都成功且数据一致才算过。
	okDial, okListen := false, false
	for !okDial || !okListen {
		select {
		case r := <-dialRes:
			if r.err != nil {
				t.Fatalf("dial side: %v", r.err)
			}
			if string(r.data) != string(payload) {
				t.Fatalf("dial 收到数据不匹配: %q", string(r.data))
			}
			okDial = true
		case r := <-listenRes:
			if r.err != nil {
				t.Fatalf("listen side: %v", r.err)
			}
			if string(r.data) != string(payload) {
				t.Fatalf("listen 收到数据不匹配: %q", string(r.data))
			}
			okListen = true
		case <-ctx.Done():
			t.Fatalf("roundtrip 超时: %v", ctx.Err())
		}
	}
}
