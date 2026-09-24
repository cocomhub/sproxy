// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// startRemoteReadForShutdownTest 装配一个**保留 cancel** 的只读面 listener
// （与 startRemoteReadDualEnd 同构，但把 rcancel 交给调用方，用于验证停机顺序）。
func startRemoteReadForShutdownTest(t *testing.T) (*RemoteReadListener, context.CancelFunc) {
	t.Helper()
	bID, bErr := tunnel.GenerateIdentity()
	if bErr != nil {
		t.Fatal(bErr)
	}
	aID, aErr := tunnel.GenerateIdentity()
	if aErr != nil {
		t.Fatal(aErr)
	}
	dir := t.TempDir()

	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Hub.XferIdentityFile = filepath.Join(dir, "b-identity.json")
	if err := tunnel.SaveIdentity(bID, cfg.Hub.XferIdentityFile); err != nil {
		t.Fatal(err)
	}
	cfg.Volumes = []VolumeConfig{{Name: testDualVol, Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testDualOwner},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: aID.Fingerprint(), Owner: testDualOwner,
		}},
	}}}
	cfg.RemoteRead.Enabled = true
	cfg.RemoteRead.Listen = "127.0.0.1:0" // 端口由 OS 分配
	cfg.RemoteRead.HandshakeTimeout = 10 * time.Second
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg: %v", err)
	}

	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	rctx, rcancel := context.WithCancel(t.Context())
	ln, err := StartRemoteReadListener(rctx, cfg, h, testutil.DiscardLogger())
	if err != nil {
		t.Fatalf("StartRemoteReadListener: %v", err)
	}
	t.Cleanup(func() {
		rcancel()
		_ = ln.Close()
	})
	return ln, rcancel
}

// TestRemoteReadListener_CtxCancelStopsAccept 钉住 I3：**ctx 取消必须停止 accept**。
//
// 回归背景：accept 循环曾是阻塞的 l.ln.Accept()（不感知 ctx），只由 RunE 的 defer 里
// rrLn.Close() 停止；而正常信号停机走 handleSignalShutdown（cmd/sproxy/root.go），它先
// cancel(ctx) 再**直接**调 h.Close()（关卷根、清 volSet/globalRoot），**不经过 defer 链**
// ——于是 cancel 之后 listener 仍在收新连接，新请求落在 volSet==nil 的卷根上。修法见
// acceptLoop：ctx 取消由 watcher goroutine 关闭 listener，并在 Serve 前判 ctx.Err()
// 丢弃竞态窗口内 accept 到的连接。
//
// 断言方式（确定性，不再靠 net.Dial 轮询）：等 acceptLoop 退出的信号 `ln.acceptDone`，
// 再断言底层 listener 已关闭（第二次 Close 返回 net.ErrClosed）。
//
// 为何不用轮询 dial：① 依赖 watcher 被及时调度，CI 高争用下会假红；② 端口被释放后可能被
// 其它并行测试重新绑定（`127.0.0.1:0`），dial 成功不等于「本 listener 还在接」；③ 只看到
// 「连不上」并不能区分「循环已退出且 listener 已关」与「循环退出但 listener 未关」——后者
// 是真正的隐患（内核 backlog 仍完成握手），本测试要把它钉死。
func TestRemoteReadListener_CtxCancelStopsAccept(t *testing.T) {
	ln, rcancel := startRemoteReadForShutdownTest(t)

	// 前置：取消前 listener 必须在接受连接（否则本测试无意义）。
	c, err := net.Dial("tcp", ln.Addr())
	if err != nil {
		t.Fatalf("cancel 前 listener 应可连接: %v", err)
	}
	_ = c.Close()

	rcancel()

	// 窗口 60s：等的是「accept 循环确实退出」这一确定性事件，不是估算调度延迟；
	// 若 cancel 真的不生效（I3 回归），60s 后必然红且错误信息直指根因。
	select {
	case <-ln.acceptDone:
	case <-time.After(60 * time.Second):
		t.Fatal("ctx 取消后 accept 循环未退出（I3 回归：停机顺序无保证）")
	}

	// 循环退出后 listener 必须已关闭——否则内核 backlog 仍会完成握手，表现为
	// 「端口仍可连但无人服务」。第二次 Close 返回 net.ErrClosed 即证明已关闭。
	// 注意：acceptLoop 退出（acceptDone 关闭）与 watcher 执行到 ln.Close() 是两个
	// goroutine，前者先于后者——直接断言会落在竞态窗口内（本次 CI 失败
	// remote_read_listener_shutdown_test.go:108 返回 <nil> 即此形态）。此处对
	// ln.ln.Close() 做有界重试：Close 幂等，watcher 完成前返回 nil，完成后返回
	// net.ErrClosed；若 watcher 始终未关（I3 回归），重试超时必红。
	var cerr error
	if cerr = ln.ln.Close(); cerr == nil || errors.Is(cerr, net.ErrClosed) {
		return
	}
	if !testutil.WaitForBool(5*time.Second, func() bool {
		cerr = ln.ln.Close()
		return cerr != nil && !errors.Is(cerr, net.ErrClosed)
	}) {
		t.Fatalf("accept 循环退出后 listener 未关闭（端口仍可连）: %v", cerr)
	}
}
