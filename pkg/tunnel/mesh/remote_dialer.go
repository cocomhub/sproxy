// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// RemoteDialer 是 `pkg/remote.Dialer` 的 **mesh 实现**（Y 二期 A 侧）：按 (node, service)
// 做服务发现，选路「WebRTC 打洞优先、按策略回落 hub 中继」，返回一条 net.Conn。
//
// 为什么放在本模块：选路细节（打洞、探测超时、mux 流、回落）**必须只有一份实现**——
// CLI（`sclient mesh connect`）与 server（sproxy 同步任务的 A 侧）都要用；把拨号器留在 CLI
// 内部会让 server 侧只能复制一份（两份必然分叉）。本模块的 go.mod 已 replace 根 module，
// 故可直接实现 `remote.Dialer`。
type RemoteDialer struct {
	cfg    RemoteDialerConfig
	svc    string
	punch  PunchFunc
	logger *slog.Logger

	// service 暴露给同包测试断言缺省值（不导出）。
	service string
}

// PunchFunc 执行一次 WebRTC 打洞并返回已就绪的连接（生产：DialWebRTC；测试：桩）。
// 返回错误表示打洞失败——是否回落中继由 `AllowRelayFallback` 决定。
type PunchFunc func(ctx context.Context, target *client.MeshService) (net.Conn, error)

// RemoteDialerConfig 配置 RemoteDialer。
type RemoteDialerConfig struct {
	// Client 是 hub 能力（服务发现 + 中继回落）。必填。
	//
	// 复用 `remote.RelayClient`（同一方法集：`MeshServices` + `RelayStream`）而**不另立
	// 同形接口**：`cmd/sproxy` 的同一条中继客户端要同时喂给 `remote.NewRelayDialer` 与本拨号器，
	// 两个同名方法集的接口会强迫调用方写适配器（且将来必然分叉）。
	Client remote.RelayClient
	// Signaler 是 WebRTC 信令客户端；nil = 不打洞（纯中继，等价现状 relay 载体）。
	// 与 Punch 同时给出时 Punch 优先（测试注入用）。
	Signaler *hub.HubSignaler
	// Punch 覆盖默认打洞实现（nil = 用 Signaler + DialWebRTC）。测试注入用。
	Punch PunchFunc
	// Service 是目标服务名（`remote.ServiceName` 只读 / `remote.ServiceNameWrite` 写面）。
	// 空 = remote.ServiceName（缺省只读；**写面必须显式声明**，避免漏配把读当写）。
	Service string
	// AllowRelayFallback 见 DialOptions（auto → true；webrtc → false）。
	AllowRelayFallback bool
	// ICE 是实例级 ICE 配置（nil = 包级全局）。
	ICE *webrtc.ICEOptions
	// Logger 可选（nil = slog.Default）。
	Logger *slog.Logger
}

// NewRemoteDialer 构造 mesh 远端拨号器。
func NewRemoteDialer(cfg RemoteDialerConfig) *RemoteDialer {
	svc := cfg.Service
	if svc == "" {
		svc = remote.ServiceName
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	d := &RemoteDialer{cfg: cfg, svc: svc, service: svc, logger: logger}
	switch {
	case cfg.Punch != nil:
		d.punch = cfg.Punch
	case cfg.Signaler != nil:
		// 默认打洞：单一实现（DialWebRTC），实例 ICE 配置透传。
		ice := cfg.ICE
		d.punch = func(ctx context.Context, target *client.MeshService) (net.Conn, error) {
			return DialWebRTC(ctx, cfg.Signaler, target, ice)
		}
	}
	return d
}

// 编译期断言：本类型就是 pkg/remote 的 Dialer（接口形状由 pkg/remote 定义，本模块只实现）。
var _ remote.Dialer = (*RemoteDialer)(nil)

// Dial 实现 `remote.Dialer`：建立到 node 上本 dialer 所声明服务的连接。
//
// 步骤（顺序即不变量）：
//  1. 服务发现：在 hub 服务表里找 **(node, service) 精确命中**；未命中即报错，
//     **绝不回落其它节点**（授权按节点绑定，借别的节点会破坏语义——与 RelayDialer 同结论）；
//  2. 有打洞能力则先打洞；失败时：`AllowRelayFallback=false` ⇒ 返回错误（不中继）；
//     `=true` ⇒ 记日志并回落；
//  3. 中继：`RelayStream(node, addr)`。
func (d *RemoteDialer) Dial(ctx context.Context, node string) (net.Conn, error) {
	if d.cfg.Client == nil {
		return nil, fmt.Errorf("mesh dialer: 未配置 hub 客户端")
	}
	if node == "" {
		return nil, fmt.Errorf("mesh dialer: 节点名为空")
	}
	svcs, err := d.cfg.Client.MeshServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("mesh dialer: 服务发现失败: %w", err)
	}
	target, ok := findService(svcs, node, d.svc)
	if !ok {
		return nil, fmt.Errorf("mesh dialer: 节点 %q 未宣告服务 %q（fail-closed，不回落其它节点）", node, d.svc)
	}

	if d.punch != nil {
		conn, perr := d.punch(ctx, target)
		if perr == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !d.cfg.AllowRelayFallback {
			return nil, fmt.Errorf("mesh dialer: WebRTC 直连失败（未允许回落中继）: %w", perr)
		}
		d.logger.Debug("mesh dialer: 打洞失败，回落 hub 中继", "error", perr, "node", node, "service", d.svc)
	}

	conn, err := d.cfg.Client.RelayStream(ctx, target.Node, target.Addr)
	if err != nil {
		return nil, fmt.Errorf("mesh dialer: 中继 %s@%s 失败: %w", target.Node, target.Addr, err)
	}
	return conn, nil
}

// findService 在服务表中精确查 (node, name)（同 (node,name) 多条目时取首个，与既有
// RelayDialer 行为一致）。
func findService(svcs []client.MeshService, node, name string) (*client.MeshService, bool) {
	for i := range svcs {
		if svcs[i].Node == node && svcs[i].Name == name {
			return &svcs[i], true
		}
	}
	return nil, false
}
