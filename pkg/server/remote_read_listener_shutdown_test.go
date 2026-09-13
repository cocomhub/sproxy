// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
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
// 断言方式：先证明 cancel 前端口可连（listener 确在 accept），cancel 后在有限时间内
// 必须变为不可连（连接被拒绝）。这是"先停 accept、再关卷根"这一顺序中 accept 侧的
// 可观测判据。
func TestRemoteReadListener_CtxCancelStopsAccept(t *testing.T) {
	ln, rcancel := startRemoteReadForShutdownTest(t)

	// 前置：取消前 listener 必须在接受连接（否则本测试无意义）。
	c, err := net.Dial("tcp", ln.Addr())
	if err != nil {
		t.Fatalf("cancel 前 listener 应可连接: %v", err)
	}
	_ = c.Close()

	rcancel()

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, derr := net.Dial("tcp", ln.Addr())
		if derr != nil {
			return // 期望路径：listener 已关闭，accept 已停止
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("ctx 取消后 listener 仍在接受新连接（I3 回归：停机顺序无保证）")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
