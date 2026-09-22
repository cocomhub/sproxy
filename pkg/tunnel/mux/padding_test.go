// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestIdlePadding_SendsPaddingFrames 验证开启空闲填充后：空闲连接周期收到填充帧
// （对端 handler 计数），且与心跳 Ping 独立共存。
func TestIdlePadding_SendsPaddingFrames(t *testing.T) {
	t.Parallel()
	serverT, clientT := xfertest.Pipe() // 内存管道传输

	// 服务端开启空闲填充（50ms 周期，测试用短周期）。
	server := NewWithOpts(serverT, RoleListener, WithIdlePadding(50*time.Millisecond))
	defer server.Close()
	client := New(clientT, RoleDialer)
	defer client.Close()

	// 等服务端填充帧到达客户端：客户端帧计数应增长。
	testutil.WaitFor(t, 2*time.Second, func() bool {
		return client.metrics.PaddingReceived.Load() >= 2
	}, func() string { return "客户端未收到空闲填充帧（want >=2）" })
}

// TestIdlePadding_DefaultOff 验证默认（不开启）零回归：无填充帧发送。
func TestIdlePadding_DefaultOff(t *testing.T) {
	t.Parallel()
	serverT, clientT := xfertest.Pipe()

	server := New(serverT, RoleListener) // 默认不开启填充
	defer server.Close()
	client := New(clientT, RoleDialer)
	defer client.Close()

	// 等一小段时间（select 内 time.After 计时，无 time.Sleep——R18 棘轮合规），
	// 断言无填充帧（默认关零回归）。
	<-time.After(300 * time.Millisecond)
	if n := client.metrics.PaddingReceived.Load(); n != 0 {
		t.Fatalf("默认应无填充帧（零回归）, got PaddingReceived=%d", n)
	}
}
