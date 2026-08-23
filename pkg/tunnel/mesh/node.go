// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

const (
	// nodeReconnectBaseDelay 是 mesh node 断线重连的初始退避。
	nodeReconnectBaseDelay = 1 * time.Second
	// nodeReconnectMaxDelay 是重连退避上限。
	nodeReconnectMaxDelay = 30 * time.Second
)

// NodeConfig 是 mesh node 常驻节点的配置。
type NodeConfig struct {
	// HubURL 是 hub 地址（http(s)/ws(s)，空时回落 ServerURL）。
	HubURL string
	// ServerURL 是 HubURL 为空的回退基址。
	ServerURL string
	// NodeID 是本节点稳定 ID（为空回落主机名；mesh connect 用它寻址，需唯一）。
	NodeID string
	// RelayToken 是 hub 中继注册 token。
	RelayToken string
	// SignalToken 是信令 Bearer（hub auth_token）。
	SignalToken string
	// Services 是宣告到 hub 的服务（mesh connect 服务发现）。
	Services []hub.Service
	// ServiceAddrs 是出口拨号精确放行地址（含 loopback/私网，供 NewServiceDialPolicy）。
	ServiceAddrs []string
	// Tags 是节点标签（如 ["exit"] 表示出口节点，mesh node --dial-allow 时打）。
	Tags []string
	// DialAllow 允许出口拨号（mesh connect 恒发 dial 帧，依赖此开关）。
	DialAllow bool
	// DialAllowCIDRs 是出口拨号额外放行的网段。
	DialAllowCIDRs []string
	// LocalAddr 是 HTTP 中继转发目标（空回落 http://127.0.0.1:8080）。
	LocalAddr string
	// Insecure 注册 WS + 信令 HTTP 跳过证书校验（自签 wss hub）。
	Insecure bool
	// EnableWebRTC 是否接受 WebRTC 直连（信令 poll + listen）。
	EnableWebRTC bool
	// Logger 是会话日志（nil 用 slog.Default()）。
	Logger *slog.Logger
}

// RunNode 运行 mesh 常驻节点：单进程单注册（稳定 node-id + 服务宣告 + per-node
// secret），并行提供中继服务（经 hub 的中继流）与 WebRTC 直连（信令 poll + listen），
// 断线指数退避重连（per-node secret 随重连轮换）。阻塞直到 ctx 取消或终态错误
// （hub 注册被拒，errors.Is(err, hub.ErrRegisterRejected)）。
func RunNode(ctx context.Context, cfg NodeConfig) error {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	delay := nodeReconnectBaseDelay
	for {
		err := runNodeOnce(ctx, cfg, logger)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if errors.Is(err, hub.ErrRegisterRejected) {
			return err
		}
		logger.Warn("mesh node 会话断开，退避重连", "delay", delay, "error", err)
		select {
		case <-time.After(delay):
			delay *= 2
			if delay > nodeReconnectMaxDelay {
				delay = nodeReconnectMaxDelay
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runNodeOnce 一次完整注册 + 中继/直连双循环。
// 返回 nil（ctx 取消/优雅退出）或真实错误（触发 RunNode 退避重连）。
//
// 生命周期：cycleCtx 内 AutoRegister（拿注册 mux + per-node secret + HubSignaler），
// 两个 goroutine 并行——中继 relay.Serve（注册 mux 上）与 webrtc 直连环；
// 首个真实失败 → cycleCancel 终止双 serve → 幂等 closeReg 关注册连接（hub
// RemoveIfOwned 摘节点）→ wg.Wait 有界回收。
func runNodeOnce(ctx context.Context, cfg NodeConfig, logger *slog.Logger) error {
	cycleCtx, cycleCancel := context.WithCancel(ctx)
	defer cycleCancel()

	reg, err := AutoRegister(cycleCtx, AutoRegisterParams{
		HubURL:      cfg.HubURL,
		ServerURL:   cfg.ServerURL,
		RelayToken:  cfg.RelayToken,
		SignalToken: cfg.SignalToken,
		NodeID:      cfg.NodeID,
		Prefix:      "mesh",
		ExactNode:   true, // mesh node 是稳定 node-id，供 mesh connect 寻址
		Insecure:    cfg.Insecure,
		Services:    cfg.Services,
		Tags:        cfg.Tags,
	})
	if err != nil {
		return err
	}
	var closeOnce sync.Once
	closeReg := func() { closeOnce.Do(func() { _ = reg.Closer() }) }
	defer closeReg()

	httpClient := &http.Client{Timeout: 30 * time.Second}
	localAddr := cfg.LocalAddr
	if localAddr == "" {
		localAddr = "http://127.0.0.1:8080"
	}
	// 中继路径 DialResultFrames=true：hub 写 200 前读拨号结果帧确认数据面就绪（I27）。
	relayOpts := []relay.ServeOptions{
		{DialPolicy: relay.NewServiceDialPolicy(cfg.DialAllowCIDRs, cfg.ServiceAddrs), DialResultFrames: true},
	}
	// 直连路径 DialResultFrames=false：结果帧会污染 webrtc 数据流（见 relay/leaf.go）。
	directOpts := []relay.ServeOptions{
		{DialPolicy: relay.NewServiceDialPolicy(cfg.DialAllowCIDRs, cfg.ServiceAddrs)},
	}

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := relay.Serve(cycleCtx, reg.Mux, localAddr, cfg.DialAllow, httpClient, logger, relayOpts...); err != nil {
			errCh <- err
		}
	}()
	if cfg.EnableWebRTC {
		go func() {
			defer wg.Done()
			if err := runWebRTCAcceptLoop(cycleCtx, reg.Signaler, reg.TempNode, localAddr, cfg.DialAllow, httpClient, logger, directOpts); err != nil {
				errCh <- err
			}
		}()
	} else {
		go func() {
			defer wg.Done()
			<-cycleCtx.Done()
		}()
	}

	var loopErr error
	select {
	case err := <-errCh:
		loopErr = err
	case <-ctx.Done():
		loopErr = nil // 优雅退出
	}
	cycleCancel() // 终止中继/直连 serve（Accept/轮询返回）
	closeReg()    // 关闭注册 mux → WS → hub RemoveIfOwned
	wg.Wait()     // 有界：cycleCtx 取消保证 goroutine 在 ≤一次 poll/Accept 周期内返回
	if ctx.Err() != nil {
		return nil
	}
	return loopErr
}

// runWebRTCAcceptLoop 循环接受 webrtc 直连：每条直连用 relay.Serve 分发
// （dial 帧→出口拨号 / HTTP 中继到 localAddr）。
//
// 空闲（ErrNoIncomingConnection，signalingTimeout 内无对端发起连接）不是失败，
// 不重注册继续监听（P1-11）；ctx 取消返回 nil；真实信令失败返回错误触发整 cycle
// 重连（节点被 hub 移除时 secret 已轮换，重连即拿新 secret 自愈）。
func runWebRTCAcceptLoop(ctx context.Context, signaler *hub.HubSignaler, nodeID, localAddr string, dialAllow bool, httpClient *http.Client, logger *slog.Logger, opts []relay.ServeOptions) error {
	for {
		conn, err := webrtc.ListenWithSignalerCtx(ctx, nodeID, signaler)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, webrtc.ErrNoIncomingConnection) {
				continue
			}
			return err
		}
		m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
		go func() {
			defer m.Close() // serve 结束即关 mux → 关底层 webrtc conn → 解除 pump
			if err := relay.Serve(ctx, m, localAddr, dialAllow, httpClient, logger, opts...); err != nil {
				logger.Debug("mesh node 直连会话结束", "error", err)
			}
		}()
	}
}
