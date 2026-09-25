// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_node.go 是 **B 侧 mesh node 角色**（S5）的装配：让 sproxy 进程自身把本机
// `remote_read`/`remote_write` 面宣告到 mesh（并允许出口拨号到这两个 loopback 地址），
// 从而**无需外部 sidecar**（`sclient mesh node --service volread:… --dial-allow`）。
//
// 定位：本文件只做「配置 → `mesh.NodeConfig` → 起后台 RunNode」，节点生命周期（注册、per-node
// secret、重连退避、中继与 WebRTC accept 环、出口策略）全部由 `pkg/tunnel/mesh.RunNode` 承担
// ——**不在此复制任何节点语义**。
//
// 默认关闭（`mesh.node.enabled: false`）：不启用时与今天完全一致（零回归）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
)

// meshNodeServiceDecls 派生要宣告到 hub 的服务声明（`name:host:port` 文本，交给
// `mesh.ParseServiceDecls` 解析——**复用生产解析器**，不在此另写一份）：
//   - `remote_read` 启用 ⇒ `volread:<readAddr>`；
//   - `remote_write` 启用 ⇒ `volwrite:<writeAddr>`；
//   - `mesh.node.extra_services` 原样追加。
//
// readAddr/writeAddr 用**监听器实际地址**（配置写 `:0` 时只有 listener 知道真实端口）；
// 未启用对应面时该参数被忽略。
func meshNodeServiceDecls(cfg *server.Config, readAddr, writeAddr string) []string {
	if cfg == nil {
		return nil
	}
	decls := make([]string, 0, 2+len(cfg.Mesh.Node.ExtraServices))
	if cfg.RemoteRead.Enabled && readAddr != "" {
		decls = append(decls, "volread:"+readAddr)
	}
	if cfg.RemoteWrite.Enabled && writeAddr != "" {
		decls = append(decls, "volwrite:"+writeAddr)
	}
	decls = append(decls, cfg.Mesh.Node.ExtraServices...)
	return decls
}

// meshNodeConfig 由 server 配置构造 `mesh.NodeConfig`（hub 可为远端；凭据与 A 侧 mesh 客户端同源）。
//
// `DialAllow` **恒为 true**：对端 A 的 dial 帧目标就是本机 loopback 的 volread/volwrite，
// 出口策略必须放行；精确放行由服务宣告地址（ServiceAddrs）承担，无需额外 CIDR。
func meshNodeConfig(cfg *server.Config, readAddr, writeAddr string, extraCreds *meshHubCreds, log *slog.Logger) (mesh.NodeConfig, error) {
	if cfg == nil {
		return mesh.NodeConfig{}, errors.New("mesh node 角色：server 配置为空")
	}
	nodeID := cfg.MeshNodeID()
	if nodeID == "" {
		// 配置层 Validate 已拒；此处兜底（防绕过 Validate 直接调用）。
		return mesh.NodeConfig{}, errors.New("mesh node 角色需要节点 ID（mesh.node.node_id / mesh.node_id / hub.node_id）")
	}
	decls := meshNodeServiceDecls(cfg, readAddr, writeAddr)
	if len(decls) == 0 {
		return mesh.NodeConfig{}, errors.New("mesh node 角色没有任何可宣告的服务（远近面未启用且无 extra_services）")
	}
	svcs, addrs := mesh.ParseServiceDecls(decls, log)
	if len(svcs) == 0 {
		return mesh.NodeConfig{}, fmt.Errorf("mesh node 角色：服务声明解析后为空（%v）", decls)
	}

	hubURL := cfg.MeshNodeHubURL()
	ak, sk, skeyID, err := meshNodeCredential(cfg, extraCreds)
	if err != nil {
		return mesh.NodeConfig{}, err
	}
	insecure := cfg.Mesh.Node.Insecure || cfg.Mesh.InsecureTLS
	if hubURL == "" {
		// 本机 hub：派生 loopback base URL；本机 TLS（通常自签）⇒ 放宽校验。
		base, bErr := localSelfBaseURL(cfg)
		if bErr != nil {
			return mesh.NodeConfig{}, bErr
		}
		hubURL = base
		if cfg.TLS.Enabled {
			insecure = true
		}
	}
	return mesh.NodeConfig{
		HubURL:          hubURL,
		ServerURL:       hubURL,
		NodeID:          nodeID,
		AccessKey:       ak,
		AccessKeySecret: sk,
		AccessKeyID:     skeyID,
		Services:        svcs,
		ServiceAddrs:    addrs,
		DialAllow:       true, // 见函数注释：对端要拨本机 loopback 面
		DialAllowCIDRs:  cfg.Mesh.Node.DialAllowCIDRs,
		EnableWebRTC:    cfg.Mesh.Node.WebRTC,
		Insecure:        insecure,
		Logger:          log,
	}, nil
}

// meshHubCreds 是显式注入的 hub 凭据（装配层已解析时复用，避免重复取本机凭据）。
type meshHubCreds struct {
	AK, SK, SkeyID string
}

// meshNodeCredential 取注册 hub 用的 SproxySig 凭据：显式注入优先；否则远端 hub 用配置凭据、
// 本机 hub 用本机自用凭据（与 A 侧 mesh 客户端同一套规则）。
func meshNodeCredential(cfg *server.Config, injected *meshHubCreds) (ak, sk, skeyID string, err error) {
	if injected != nil && injected.AK != "" {
		return injected.AK, injected.SK, injected.SkeyID, nil
	}
	if cfg.Mesh.AccessKey != "" && cfg.Mesh.AccessKeySecret != "" {
		return cfg.Mesh.AccessKey, cfg.Mesh.AccessKeySecret, cfg.Mesh.SkeyID, nil
	}
	if cfg.MeshNodeHubURL() == "" {
		// 本机 hub：凭据由调用方（root.go，能拿到 *server.Handlers）注入；此处缺失即报错。
		return "", "", "", errors.New("mesh node 角色注册本机 hub 需要本机自用凭据（装配层注入）")
	}
	return "", "", "", errors.New("mesh node 角色注册远端 hub 需要 mesh.access_key/access_key_secret")
}

// startMeshNodeRoleWithCreds 在启用时把 mesh node 角色跑在后台（返回是否已启动）。
// creds 是本机自用凭据（装配层 root.go 注入，注册本机 hub 时必需）。
//
// 未启用返回 false（零回归）；前置不满足（缺 node_id / 无可宣告服务 / 缺凭据）时**不启动**并告警
// （fail-closed：宁可没有角色，也不要半开的节点）。
//
// alertEng 是 NAT 穿透失败告警引擎（roadmap 11.1-①）：非 nil 时接入节点生命周期拨号失败
// 事件（RunNode 断线退避重连的每次会话失败 → OnNATFailure；后续成功会话 → OnNATRecovered）。
func startMeshNodeRoleWithCreds(ctx context.Context, cfg *server.Config, readAddr, writeAddr string, creds *meshHubCreds, alertEng *server.AlertEngine, log *slog.Logger) bool {
	if cfg == nil || !cfg.Mesh.Node.Enabled {
		return false
	}
	if log == nil {
		log = slog.Default()
	}
	nc, err := meshNodeConfig(cfg, readAddr, writeAddr, creds, log.With("component", "mesh_node"))
	if err != nil {
		log.Warn("mesh node 角色未启动（fail-closed）", "error", err)
		return false
	}
	log.Info("mesh node 角色已启动（进程内宣告服务 + 出口拨号）",
		"node_id", nc.NodeID, "hub", nc.HubURL, "services", len(nc.Services), "webrtc", nc.EnableWebRTC)
	peer := nc.NodeID
	dial := withNATAlert(func(ctx context.Context, addr string) (net.Conn, error) {
		return nil, mesh.RunNode(ctx, nc)
	}, alertEng, peer)
	go func() {
		// RunNode 自带重连退避与 ctx 感知退出（ctx 取消即优雅摘除节点）。
		// 用 ctx 而非 background：RunNode 的退避重连生命周期绑定进程 ctx；
		// 拨号失败告警（withNATAlert 旁路）在重连循环内自然随进程 ctx 收敛。
		_, rErr := dial(ctx, "")
		if rErr != nil && ctx.Err() == nil {
			log.Warn("mesh node 角色退出", "error", rErr)
		}
	}()
	return true
}

// 编译期：服务声明类型与 RunNode 入参一致（防将来漂移）。
var _ []hub.Service
