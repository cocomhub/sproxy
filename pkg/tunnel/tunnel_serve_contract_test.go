// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// 本文件锁定 (*Tunnel).Serve 的返回值契约（见 tunnel_mux.go 文档注释）：
//
//	ctx 取消（正常关闭，含握手中断）→ nil；返回非 nil ⟺ 终止性错误。
//
// 真失败一侧另由既有门禁覆盖：TestECDHHandshake_KeyedListenerNilDialerFails
// （ecdh_test.go）断言 keyed listener 握手失败（父 ctx 存活）必须返回非 nil——
// 它同时是「不得把真握手失败归零」的反向保险。
//
// 补强：TestTunnelServe_MuxCloseDuringHandshakeReturnsError 直接锁住「**握手进行中**
// 且父 ctx 存活时 mux 被关闭 → 非 nil」——此前该情形仅由上述 ecdh_test.go 门禁
// **间接**覆盖（同为「握手 err≠nil + 父 ctx 存活」），缺一条 keyed listener 的
// 直接用例。

// TestTunnelServe_CtxCancelReturnsNil 断言两条退出路径在 ctx 取消时都返回 nil：
// 未配置 key 时的 accept 循环、keyed listener 的握手中断。
func TestTunnelServe_CtxCancelReturnsNil(t *testing.T) {
	key, err := ParseKey(testHexKey)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	tests := []struct {
		name string
		key  []byte
	}{
		{"accept 循环（无 key）", nil},
		{"握手中断（keyed listener）", key},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, b := xfertest.Pipe()
			m := mux.New(b, mux.RoleListener)
			defer func() { _ = m.Close() }()
			tun := NewTunnel(m, tc.key)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			errCh := make(chan error, 1)
			go func() {
				errCh <- tun.Serve(ctx, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			}()

			time.Sleep(50 * time.Millisecond) // 等 Serve 进入 Accept/握手等待
			cancel()

			select {
			case err := <-errCh:
				if err != nil {
					t.Fatalf("ctx 取消应返回 nil（正常关闭），实际返回 %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve 未在 ctx 取消后返回")
			}
		})
	}
}

// TestTunnelServe_MuxCloseReturnsError 断言：ctx 仍存活时 mux 被关闭属终止性错误，
// Serve 必须返回非 nil——cmd/sproxy 的 `sErr != nil && ctx.Err() == nil` 告警守卫
// 依赖这条语义。
func TestTunnelServe_MuxCloseReturnsError(t *testing.T) {
	t.Parallel()
	_, b := xfertest.Pipe()
	m := mux.New(b, mux.RoleListener)
	tun := NewTunnel(m, nil) // 无 key：跳过握手，直接进 accept 循环

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- tun.Serve(ctx, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	}()

	time.Sleep(50 * time.Millisecond)
	if err := m.Close(); err != nil {
		t.Fatalf("mux.Close: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("ctx 存活时 mux 关闭应返回非 nil（终止性错误），实际返回 nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve 未在 mux 关闭后返回")
	}
}

// TestTunnelServe_MuxCloseDuringHandshakeReturnsError 断言：父 ctx 存活时，**握手正在
// 进行中**的 keyed listener 的 mux 被关闭同样返回非 nil（且必须是 fail-closed 的
// 「握手失败」错误，而非 accept 循环错误）。TestTunnelServe_MuxCloseReturnsError 走的是
// NewTunnel(m, nil) 的无 key accept 路径，未覆盖握手分支；本用例补上直达覆盖。
//
// 时序确定性（无 sleep 竞猜、无 flaky 源）：
//   - dialer 侧只开握手流、**一个字节都不写**——listener 的握手阻塞在「读对端 32B 公钥」
//     上，只有收到我们的公钥才可能前进，故握手**不可能**成功，Serve 必然从握手分支
//     （而非 accept 循环）返回；
//   - 关闭前先等 listener 侧 mux 收到 FrameOpen（Metrics.Streams.Opened 由
//     handleOpenFrame 入队 acceptCh 时自增），确保关的不是尚未送达的空 mux。
func TestTunnelServe_MuxCloseDuringHandshakeReturnsError(t *testing.T) {
	t.Parallel()
	key, err := ParseKey(testHexKey)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer func() { _ = muxA.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- NewTunnel(muxB, key).Serve(ctx, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	}()

	// 兜底：握手流送达不应超过下方轮询 deadline（防用例挂死）；超时关闭两 mux 让
	// Serve 立即返回，用例以明确信息失败，而非卡到 go test 全局超时。
	//
	// 顺序不变式：watchdog(10s) **必须显著晚于**下方轮询 deadline(2s)。旧时序下二者同刻：
	// watchdog 的关闭可能早于轮询的最后一次检查，而一个 in-flight 的 handleOpenFrame 可在
	// mux 关闭后才 Opened++，把轮询带出等待循环——此时「帧未及时送达」（底层已损坏/极端
	// 迟滞）这一故障不会被报出，用例反而以 errCh 上如期出现的「握手失败」判绿。
	// 拉开间距后 watchdog 不可能在轮询等待期间触发，该窗口被关闭；断言不变（未削弱用例）。
	//
	// 反面澄清（勿再写成这样）：误通过**不是**「轮询侧 t.Fatal 输掉调度竞争」。轮询循环
	// 唯一的另一出口就是无条件判失败的 t.Fatal，且 Opened 只由帧投递或本端 Open 递增、
	// mux.Close 不触碰它——那条路径不存在（已由「轮询条件改恒真」的副本实测证伪：
	// 旧 5s/5s 与新 10s/2s 两版均以轮询超时 FAIL）。
	watchdog := time.AfterFunc(10*time.Second, func() {
		_ = muxA.Close()
		_ = muxB.Close()
	})
	defer watchdog.Stop()

	// dialer 侧只开握手流，不写任何字节（listener 握手将停在其 pubkey 读取上）。
	s, err := muxA.Open(ctx)
	if err != nil {
		t.Fatalf("muxA.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	// 等 listener 侧 mux 收到握手流（此时其握手必然已在进行中：流已入队 acceptCh，
	// 而我们未写任何字节，握手不可能推进完成）。
	//
	// 2s（而非旧值 5s）：必须严格早于 watchdog 的 10s——见其「顺序不变式」注释。
	deadline := time.Now().Add(2 * time.Second)
	for muxB.Metrics().Streams.Opened.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("listener 侧未收到握手流（FrameOpen 未送达）")
		}
		time.Sleep(time.Millisecond)
	}

	// 握手确实在进行中：关闭 mux（父 ctx 仍存活）。
	if err := muxB.Close(); err != nil {
		t.Fatalf("muxB.Close: %v", err)
	}

	select {
	case serr := <-errCh:
		if ctx.Err() != nil {
			t.Fatalf("父 ctx 不应被取消（否则本用例退化为 ctx 取消路径）: %v", ctx.Err())
		}
		if serr == nil {
			t.Fatal("父 ctx 存活时握手中 mux 关闭应返回非 nil（终止性错误），实际返回 nil")
		}
		if !strings.Contains(serr.Error(), "握手失败") {
			t.Fatalf("应命中握手中断分支（keyed listener fail-closed），实际返回: %v", serr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve 未在 mux 关闭后返回")
	}
}
