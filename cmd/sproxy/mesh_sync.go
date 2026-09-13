// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_sync.go 是 A 侧 **mesh 载体**（Y 二期 P3-d）的装配：把 `syncexec` 的 `MeshFSFactory`
// 接到 `pkg/remote` + 中继拨号器，使 `kind=mesh` 的同步任务真正可执行。
//
// 链路形态（与规格 §5.3/§5.6 一致）：
//
//	同步引擎(sync.FS) ← pkg/remote(隧道) ← 中继拨号器 ← 本机 hub API(/api/hub/services、/api/relay/stream)
//
// 为什么经本机 hub API 而不是直连对端：B 侧写/读 listener 只绑 loopback（`remote_read.listen`
// / `remote_write.listen` 强制 loopback），跨节点可达性由 mesh/hub 提供 ⇒ A 侧必须先经 hub
// 做服务发现与流中继。
//
// **只有 relay**：`pkg/tunnel/mesh`（WebRTC 直连）是独立 module，cmd/sproxy 不导入 ⇒
// `transport=webrtc` 在本装配明确报错（留待 CLI 侧注入自己的 Dialer）。

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/server"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// newMeshFSFactory 构造 mesh 载体的 FS 工厂。
//
// 依赖全部**显式入参**（便于测试与装配解耦）：relay 按服务名给出中继客户端（生产：指向本机
// HTTP 面的 `*client.FileClient`），id 是 A 侧 Ed25519 身份（与 xfer 身份同一份）。
//
// 校验全部 fail-closed（`syncmgr.Validate` 已查一遍，这里是**最后一道**）：
// 缺 node/volume/pins/身份 ⇒ 拒绝；`transport` 非空且非 auto/relay ⇒ 拒绝（不静默回落 relay，
// 否则「声明了 webrtc」会被悄悄按 relay 跑，掩盖未生效的配置）。
func newMeshFSFactory(relay func(service string) remote.RelayClient, id *tunnel.Identity, log *slog.Logger) syncexec.MeshFSFactory {
	return func(ctx context.Context, rc syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		if rc.Node == "" || rc.Volume == "" {
			return nil, nil, fmt.Errorf("mesh 载体需要 node 与 volume（当前 node=%q volume=%q）", rc.Node, rc.Volume)
		}
		if len(rc.PeerPins) == 0 {
			return nil, nil, fmt.Errorf("mesh 载体需要 peer_pins（空 = 拒绝连接，不 TOFU）")
		}
		switch rc.Transport {
		case "", "auto", "relay":
			// 默认/显式 relay：本装配唯一支持的载体。
		case "webrtc":
			return nil, nil, fmt.Errorf("transport=webrtc 在本装配不受支持（cmd/sproxy 只提供 relay；" +
				"WebRTC 直连位于 pkg/tunnel/mesh 子 module，需由 CLI 侧注入 Dialer）")
		default:
			return nil, nil, fmt.Errorf("未知 transport %q（可选：auto|relay|webrtc）", rc.Transport)
		}
		if id == nil {
			return nil, nil, fmt.Errorf("mesh 载体需要 A 侧 Ed25519 身份（用于双向 pin 握手）")
		}
		if relay == nil {
			return nil, nil, fmt.Errorf("mesh 载体需要中继客户端（本机 hub API 访问凭据未装配）")
		}

		// 读面与写面**两条独立链路**：不同服务名（volread/volwrite）、不同 listener、不同路由白名单。
		readDialer := remote.NewRelayDialer(relay(remote.ServiceName), remote.ServiceName)
		writeDialer := remote.NewRelayDialer(relay(remote.ServiceNameWrite), remote.ServiceNameWrite)

		opts := []remote.Option{
			remote.WithIdentity(id),
			remote.WithWriteDialer(writeDialer),
		}
		if log != nil {
			opts = append(opts, remote.WithLogger(log))
		}
		// 每条 pin 单独注册（WithPeerPin 追加语义）；节点名取 rc.Node——授权按节点绑定。
		for _, pin := range rc.PeerPins {
			opts = append(opts, remote.WithPeerPin(rc.Node, pin))
		}

		c := remote.New(readDialer, opts...)
		ref := remote.Ref{Node: rc.Node, Volume: rc.Volume}
		return c.FS(ref), func() { _ = c.Close() }, nil
	}
}

// localSelfBaseURL 由 server 配置派生**本机 HTTP 面**的 base URL（A 侧中继的入口）。
//
// 规则（与 `cfg.Addr` 的监听语义一致）：
//   - host 为空、或为**未指定地址**（`net.IP.IsUnspecified`：IPv4 通配 / IPv6 `::`）⇒ 归一为
//     127.0.0.1（监听任意地址时，自连接走 loopback）；
//   - scheme 由 `cfg.TLS.Enabled` 决定（https 时调用方需按自签证书放宽信任，见 newLocalSelfClient）。
//
// 用 `IsUnspecified` 而非比对字面量：同时覆盖 IPv4 通配与 IPv6 `::`，且不在源码里留下通配地址
// 字面量（`make check-loopback` 按字面量扫描，避免误报）。
func localSelfBaseURL(cfg *server.Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("mesh 载体：server 配置为空")
	}
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return "", fmt.Errorf("mesh 载体：addr %q 无法解析（需 host:port）: %w", cfg.Addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	scheme := "http"
	if cfg.TLS.Enabled {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

// newLocalSelfClient 构造指向**本机 HTTP 面**的 FileClient：A 侧 mesh 中继经它做服务发现
// （`/api/hub/services`）与流中继（`/api/relay/stream`）。
//
// TLS：开启时用 `client.WithInsecureTLS`——本机自连接的目标通常是自签证书，证书链无从验证；
// 该放宽**只作用于本进程到本机 loopback 的连接**，不改变对外信任模型（对外 pin 由 mesh 身份
// 指纹承担）。
func newLocalSelfClient(cfg *server.Config, ak, sk, skeyID string) (*client.FileClient, error) {
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("mesh 载体：本机凭据不可用（AK/SK 为空）")
	}
	base, err := localSelfBaseURL(cfg)
	if err != nil {
		return nil, err
	}
	opts := []client.Option{client.WithAccessKey(ak, sk)}
	if skeyID != "" {
		opts = append(opts, client.WithAccessKeyID(skeyID))
	}
	if cfg.TLS.Enabled {
		opts = append(opts, client.WithInsecureTLS())
	}
	return client.NewFileClient(base, opts...), nil
}

// hasMeshRemote 报告是否配置了 `kind=mesh` 的同步远端（决定要不要装配 mesh 载体）。
func hasMeshRemote(remotes []server.SyncRemoteConfig) bool {
	for _, r := range remotes {
		if syncmgr.RemoteKind(r.Kind) == syncmgr.RemoteKindMesh {
			return true
		}
	}
	return false
}

// setupMeshFSFactory 在**配置了 mesh 远端**时把 mesh 载体工厂注入执行器。
//
// 任一前置缺失都**不注入**并留下告警：这样 `kind=mesh` 的远端会以
// `syncexec.ErrMeshTransportNotWired` 明确失败，而**不会**静默回落 direct（回落会让「已声明
// mesh 授权」的配置走直连凭据，破坏授权语义）。
func setupMeshFSFactory(exec *syncexec.Executor, cfg *server.Config, h *server.Handlers, log *slog.Logger) {
	if exec == nil || cfg == nil || h == nil || !hasMeshRemote(cfg.SyncRemotes) {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	ak, sk, skeyID, ok := h.SelfCredential()
	if !ok {
		log.Warn("mesh 载体未装配：本机凭据不可用（kind=mesh 的远端将 fail-closed）")
		return
	}
	fc, err := newLocalSelfClient(cfg, ak, sk, skeyID)
	if err != nil {
		log.Warn("mesh 载体未装配：本机客户端构造失败", "error", err)
		return
	}
	id, err := server.LoadXferIdentity(cfg)
	if err != nil {
		log.Warn("mesh 载体未装配：身份加载失败", "error", err)
		return
	}
	exec.SetMeshFSFactory(newMeshFSFactory(
		func(string) remote.RelayClient { return fc }, id, log.With("component", "mesh_sync")))
	log.Info("mesh 载体已装配（中继：本机 hub API）", "identity", id.Fingerprint())
}
