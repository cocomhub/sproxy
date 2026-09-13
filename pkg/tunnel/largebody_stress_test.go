// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestTunnel_LargeBodyStress_SplitsAtMaxFramePayload 是 issue #213 的**回归哨兵**。
//
// #213 的根因（已定位并修复）：流发送窗口 `DefaultWindowSize=65536` 比帧头 Length 字段上限
// （2 字节 = 65535）大 1，而 `EncodeFrame` 曾经把超限负载**静默截断** ⇒ 当窗口恰好写满时
// 对端**少收 1 字节**，整条字节流从此错位；上层隧道流是分块加密的，于是表现为
// `cipher: message authentication failed`（且重传无法纠正——错位发生在解密层之下）。
//
// 为什么必须是**多轮**往返而不是一条用例：触发要求「某次写入恰好用满整个窗口」，属时序相关，
// 单次运行大概率不触发（实测：单次 1/30~1/200；连续 200 次几乎必现——修复前本用例在
// 迭代 ~100 内稳定失败，且失败点正是 65536→65535 的差 1）。故这里用 200 轮把它变成
// **确定性门禁**：一旦有人动窗口/帧长/写收敛，本用例会立刻变红。
func TestTunnel_LargeBodyStress_SplitsAtMaxFramePayload(t *testing.T) {
	const iters = 200
	const size = 300000 // 跨多个 64 KiB 窗口与多个密文块
	key := bytes.Repeat([]byte{0x11}, 32)
	payload := bytes.Repeat([]byte{0xab}, size)

	for i := range iters {
		t.Run(fmt.Sprintf("i%03d", i), func(t *testing.T) {
			a, b := xfertest.Pipe()
			defer func() { _ = a.Close() }()
			defer func() { _ = b.Close() }()

			mb := mux.New(b, mux.RoleListener)
			defer func() { _ = mb.Close() }()
			tb := NewTunnel(mb, key)
			go func() {
				_ = tb.Serve(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write(payload)
				}))
			}()

			ma := mux.New(a, mux.RoleDialer)
			defer func() { _ = ma.Close() }()
			ta := NewTunnel(ma, key)

			req, err := http.NewRequest(http.MethodGet, "/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := ta.Do(req)
			if err != nil {
				t.Fatalf("iter %d Do: %v", i, err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("iter %d 读响应体: %v（字节流错位/丢字节）", i, err)
			}
			if len(got) != size || !bytes.Equal(got, payload) {
				t.Fatalf("iter %d 内容不符: got %d 字节 want %d", i, len(got), size)
			}
		})
	}
}
