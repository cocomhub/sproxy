// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// rwcNetConn 把 io.ReadWriteCloser（mux.Stream 等）适配为 net.Conn，供
// E2EServe 回调（ServeE2EStream 要求 net.Conn：LocalAddr/RemoteAddr/Deadline）。
// 非真实 socket——地址/超时方法尽力而为（空地址 + nil deadline），满足接口即可。
type rwcNetConn struct {
	io.ReadWriteCloser
}

func (rwcNetConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (rwcNetConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (rwcNetConn) SetDeadline(_ time.Time) error      { return nil }
func (rwcNetConn) SetReadDeadline(_ time.Time) error  { return nil }
func (rwcNetConn) SetWriteDeadline(_ time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "e2e-stream" }
func (dummyAddr) String() string  { return "e2e-stream" }

// E2EServeClosure 构造 relay.ServeOptions.E2EServe 闭包：把 mesh.ServeE2EStream
// （端到端加密字节流解密）包成 relay 包可注入的回调，避免 relay→mesh 包级环。
//
// identity 是本端长时身份（NodeConfig.Identity，nil = 纯 ECDH 防窃听）；pins 是
// 对端指纹白名单（NodeConfig.AllowedPeerFingerprints，空 = 不 pinning）。
// 安全语义：启用 = 配置了身份或白名单（显式 pinning）；未配置时 E2EServe 仍注入
// （Enabled: true 纯 ECDH）——X/hub 仍读不到明文，但无 MITM 防护（可观测：
// 无身份/无 pin 时日志告警提示「端到端加密纯 ECDH 模式，未配置指纹 pinning」）。
func E2EServeClosure(identity *tunnel.Identity, pins []string) func(ctx context.Context, conn io.ReadWriteCloser, id *tunnel.Identity, peerPins []string, meta []byte) (net.Conn, error) {
	return func(ctx context.Context, conn io.ReadWriteCloser, id *tunnel.Identity, peerPins []string, meta []byte) (net.Conn, error) {
		// 装配层注入的身份/白名单优先；回调参数为 0 时回落（双保险）。
		if id == nil {
			id = identity
		}
		if len(peerPins) == 0 {
			peerPins = pins
		}
		// meta 非 nil = 首帧已由 relay.Serve dOK 分支消费（透传已读帧，跳过读帧防错位）；
		// nil = ServeE2EStream 自行读帧（mock/直连场景）。
		return serveE2EStreamAfterFrame(ctx, rwcNetConn{ReadWriteCloser: conn}, meta, EndToEndOptions{
			Enabled:          true,
			Identity:         id,
			PeerFingerprints: peerPins,
		})
	}
}
