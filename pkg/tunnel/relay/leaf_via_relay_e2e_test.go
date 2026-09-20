// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestLeaf_ViaRelay_E2EFrameTransparentRelay 验证 X 侧透传分支：e2e dial 帧带
// Path="via-relay"（L 经 X 中继到 T 的多跳形态）时，X **不做解密**（E2EServe 不调用），
// 而是拨目标 T 后把 **Path 置空**的 e2e 帧写回 T（T 侧据此识别自己是对端并解密），
// 再双向泵送密文字节——X 全程不见明文（T1 红线：X 持 SK 读不到明文）。
//
// T 侧用「帧感知 echo」模拟：读 [4B len][改写后帧]（断言 Path 已清空）→ 读剩余
// 密文字节 → 原样回显。断言：① 密文往返成功；② T 收到的帧 Path 为空（透传改写正确）；
// ③ E2EServe 未被调用（X 不解密）。
//
// 红灯前提：透传分支未实现（当前 e2e 帧 + Path='via-relay' 会走 E2EServe 解密分支）——
// 本测试断言 E2EServe 不调用将失败（证明透传分支缺失）。
func TestLeaf_ViaRelay_E2EFrameTransparentRelay(t *testing.T) {
	t.Parallel()

	// T 侧「帧感知 echo」：读首帧（校验 Path 已清空）+ 读剩余字节 → 原样回显。
	var framePath atomic.Value // 存 T 收到的帧的 Path
	framePath.Store("unset")
	echoAddr := startFrameAwareEchoServer(t, &framePath)

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// X 侧：不注入 E2EServe（X 是中间节点，不解密）——透传分支必须不依赖 E2EServe。
	var e2eServeCalled atomic.Bool
	policy := func(addr string) (string, bool) { return addr, true }
	go func() {
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testLogger(),
			ServeOptions{
				DialPolicy: policy,
				E2EServe: func(_ context.Context, _ io.ReadWriteCloser, _ *tunnel.Identity, _ []string) (net.Conn, error) {
					e2eServeCalled.Store(true)
					return nil, nil
				},
			})
	}()

	// L 侧（客户端）：写 e2e dial 帧 + Path="via-relay" + 密文字节（任意字节——X 透传）。
	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	dialMeta, _ := json.Marshal(hub.DialRequest{Dial: echoAddr, E2E: true, Path: "via-relay"})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(dialMeta)))
	if _, werr := clientStream.Write(lenBuf); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := clientStream.Write(dialMeta); werr != nil {
		t.Fatal(werr)
	}

	// 密文负载（任意字节——X 应原样泵送，不解密）。
	cipher := []byte("\x01\x02\x03cipher-bytes-arbitrary\xff\xfe")
	if _, werr := clientStream.Write(cipher); werr != nil {
		t.Fatal(werr)
	}
	_ = clientStream.CloseWrite()

	// 断言 E2EServe 未被调用（X 走透传分支，不解密）。
	select {
	case <-time.After(300 * time.Millisecond):
	case <-time.After(1):
	}
	if e2eServeCalled.Load() {
		t.Fatal("X 侧透传分支不应调用 E2EServe（X 不解密，纯字节泵）")
	}

	// 读回显（密文原样往返）。
	got := make([]byte, len(cipher))
	if err := readFullWithTimeout(ctx, clientStream, got, "回显密文"); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(cipher) {
		t.Fatalf("密文回显不一致: got %q, want %q", got, cipher)
	}

	// T 收到的帧 Path 应已清空（X 透传改写：Path 置空，T 据此识别自己是对端）。
	if p := framePath.Load().(string); p != "" {
		t.Fatalf("T 收到的帧 Path = %q, want 空（X 透传须把 Path 置空，T 才能识别自己是最终目标）", p)
	}
}

// startFrameAwareEchoServer 启动「帧感知 echo」TCP 服务：读 [4B len][首帧]（把
// 帧的 Path 存入 framePath）→ 读剩余字节 → 原样回显。模拟 T 侧对端（X 透传后
// 收到改写帧 + 密文的节点）。
func startFrameAwareEchoServer(t *testing.T, framePath *atomic.Value) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// 读首帧。
				lenBuf := make([]byte, 4)
				if _, rerr := io.ReadFull(c, lenBuf); rerr != nil {
					return
				}
				metaLen := binary.BigEndian.Uint32(lenBuf)
				if metaLen == 0 || metaLen > 4096 {
					return
				}
				meta := make([]byte, metaLen)
				if _, rerr := io.ReadFull(c, meta); rerr != nil {
					return
				}
				var d hub.DialRequest
				if uerr := json.Unmarshal(meta, &d); uerr != nil {
					return
				}
				framePath.Store(d.Path)
				// 读剩余字节 → 原样回显。
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}
