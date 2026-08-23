// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ws

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// TestWSSend_AfterMarkClosedNoCloseCh_ReturnsError（P1-14 回归）：
// failLoop 置 closed 标志但 close(closeCh) 尚未执行（中间态）时，Send 必须返回
// xfer.ErrConnClosed 而非假成功入队——否则消息留在 sendCh 永不写出（sendLoop 已
// 退出），"Send 返回 nil = 已接受"契约被静默破坏，丢帧逃逸。
func TestWSSend_AfterMarkClosedNoCloseCh_ReturnsError(t *testing.T) {
	c := &wsConn{
		sendCh:  make(chan []byte, 16),
		closeCh: make(chan struct{}),
	}
	// 模拟 failLoop 中间态：置 closed 标志，不 close closeCh（Send 的前置 closeCh
	// 检查不会命中，必须靠入队后的 closed 复检拦截）。
	c.markClosed()

	if err := c.Send(context.Background(), []byte("payload")); err != xfer.ErrConnClosed {
		t.Fatalf("markClosed 后（closeCh 未关）Send 应返回 xfer.ErrConnClosed，got %v", err)
	}
}
