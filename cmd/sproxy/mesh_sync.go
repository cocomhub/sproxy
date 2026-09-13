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
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/server"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// carrierStats 是**本次 FS 实例**的载体计数（并发安全）。
//
// 语义（与 syncexec.CarrierReporter 契约一致）：每次**成功建立链路**计一次；可同时出现多个键
// （`auto` 下同一次任务里既有直连又有回落）。
type carrierStats struct {
	mu sync.Mutex
	m  map[string]int
}

func newCarrierStats() *carrierStats { return &carrierStats{m: map[string]int{}} }

// record 记一次载体使用。
func (s *carrierStats) record(carrier string) {
	if s == nil || carrier == "" {
		return
	}
	s.mu.Lock()
	if s.m == nil {
		s.m = map[string]int{}
	}
	s.m[carrier]++
	s.mu.Unlock()
}

// snapshot 返回副本（调用方不得改动内部状态）。
func (s *carrierStats) snapshot() map[string]int {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) == 0 {
		return map[string]int{}
	}
	out := make(map[string]int, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

// carrierReportingFS 把远端 FS 包一层，附带载体统计（实现 `syncexec.CarrierReporter`）。
type carrierReportingFS struct {
	syncpkg.FS
	stats *carrierStats
}

// CarrierStats 实现 `syncexec.CarrierReporter`。
func (f carrierReportingFS) CarrierStats() map[string]int { return f.stats.snapshot() }

// countingDialer 给「载体静态可知」的拨号器（纯中继）套一层计数：成功 Dial 即记一次。
//
// 为什么需要它：`relay` 载体用 `remote.RelayDialer`（无回调接缝），但其载体**必然**是中继
// ⇒ 由本装饰器补计，使三载体在统计口径上一致（不带装饰器时 relay 路径会「无统计」而误导 UI）。
type countingDialer struct {
	inner   remote.Dialer
	carrier string
	stats   *carrierStats
}

func (c countingDialer) Dial(ctx context.Context, node string) (net.Conn, error) {
	conn, err := c.inner.Dial(ctx, node)
	if err == nil {
		c.stats.record(c.carrier)
	}
	return conn, err
}

// meshFactoryDeps 是 mesh 载体工厂的**装配依赖**（显式入参：便于单测注入替身）。
type meshFactoryDeps struct {
	// RelayFor 按服务名给出 hub 能力客户端（服务发现 `/api/hub/services` + 中继 `/api/relay/stream`）。
	RelayFor func(service string) remote.RelayClient
	// Signaler 是 WebRTC 信令客户端；nil = 无打洞能力（`transport: webrtc` 将 fail-closed）。
	//
	// **类型必须是接口**：若声明为 `*hub.HubSignaler`，把未配置的 nil 指针赋给本字段、再传入
	// `mesh.RemoteDialerConfig.Signaler`（接口）会得到「非 nil 接口 + nil 指针」的 typed nil
	// ——拨号器会误判「有信令」而在真拨号时 panic（本片实测踩到）。
	Signaler webrtc.Signaler
	// ICE 是**实例级** ICE 配置（nil = 用 webrtc 包级全局）。见 server.MeshConfig。
	ICE *webrtc.ICEOptions
	// Identity 是 A 侧 Ed25519 身份（双向 pin 握手）。
	Identity *tunnel.Identity
	Logger   *slog.Logger
}

// newMeshFSFactory 构造 mesh 载体的 FS 工厂。
//
// 依赖全部**显式入参**（便于测试与装配解耦）：RelayFor 给出 hub 客户端（可指向**远端 hub**），
// Signaler 提供 WebRTC 信令，ICE 为实例级 ICE 配置。
//
// 校验全部 fail-closed（`syncmgr.Validate` / `server.Config.Validate` 已查一遍，这里是**最后一道**）：
// 缺 node/volume/pins/身份/中继客户端 ⇒ 拒绝；`transport` 未知 ⇒ 拒绝；`transport=webrtc` 缺信令
// ⇒ 拒绝（不静默降级为 auto/relay——否则「声明了直连」被悄悄改写，掩盖配置问题）。
func newMeshFSFactory(deps meshFactoryDeps) syncexec.MeshFSFactory {
	return func(ctx context.Context, rc syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		if rc.Node == "" || rc.Volume == "" {
			return nil, nil, fmt.Errorf("mesh 载体需要 node 与 volume（当前 node=%q volume=%q）", rc.Node, rc.Volume)
		}
		if len(rc.PeerPins) == 0 {
			return nil, nil, fmt.Errorf("mesh 载体需要 peer_pins（空 = 拒绝连接，不 TOFU）")
		}
		if deps.Identity == nil {
			return nil, nil, fmt.Errorf("mesh 载体需要 A 侧 Ed25519 身份（用于双向 pin 握手）")
		}
		if deps.RelayFor == nil {
			return nil, nil, fmt.Errorf("mesh 载体需要 hub 客户端（服务发现 + 中继回落）")
		}

		// 读面与写面**两条独立链路**：不同服务名（volread/volwrite）、不同 listener、不同路由白名单。
		// 每次装配一份**本 FS 专属**的载体统计（任务级语义：这次任务走了什么载体）。
		stats := newCarrierStats()
		readDialer, writeDialer, err := buildMeshDialers(deps, rc.Transport, stats)
		if err != nil {
			return nil, nil, err
		}

		opts := []remote.Option{
			remote.WithIdentity(deps.Identity),
			remote.WithWriteDialer(writeDialer),
		}
		if deps.Logger != nil {
			opts = append(opts, remote.WithLogger(deps.Logger))
		}
		// 每条 pin 单独注册（WithPeerPin 追加语义）；节点名取 rc.Node——授权按节点绑定。
		for _, pin := range rc.PeerPins {
			opts = append(opts, remote.WithPeerPin(rc.Node, pin))
		}

		c := remote.New(readDialer, opts...)
		ref := remote.Ref{Node: rc.Node, Volume: rc.Volume}
		return carrierReportingFS{FS: c.FS(ref), stats: stats}, func() { _ = c.Close() }, nil
	}
}

// buildMeshDialers 按 `transport` 语义构造**读/写两面**拨号器（Y 二期载体矩阵）：
//
//	""/"auto" → mesh 拨号器（打洞优先 + 回落中继）
//	"relay"   → 纯中继（RelayDialer；与写批次之前的唯一形态一致）
//	"webrtc"  → mesh 拨号器（打洞优先 + **不回落**；缺信令即拒）
//
// 未知值兜底拒绝（配置层 ValidateForTask 已拒，此处防绕过）。
func buildMeshDialers(deps meshFactoryDeps, transport string, stats *carrierStats) (read, write remote.Dialer, err error) {
	switch transport {
	case "", "auto":
		return meshFaceDialer(deps, remote.ServiceName, true, stats), meshFaceDialer(deps, remote.ServiceNameWrite, true, stats), nil
	case "relay":
		// 纯中继：载体静态已知，用计数装饰器补齐统计（与 mesh 拨号器的 OnCarrier 口径一致）。
		return countingDialer{inner: remote.NewRelayDialer(deps.RelayFor(remote.ServiceName), remote.ServiceName), carrier: "relay", stats: stats},
			countingDialer{inner: remote.NewRelayDialer(deps.RelayFor(remote.ServiceNameWrite), remote.ServiceNameWrite), carrier: "relay", stats: stats}, nil
	case "webrtc":
		if !mesh.SignalerUsable(deps.Signaler) {
			return nil, nil, fmt.Errorf("transport=webrtc 需要 mesh 信令（配置 mesh.node_id；" +
				"无信令时请用 transport=auto|relay，而不是期望静默降级）")
		}
		return meshFaceDialer(deps, remote.ServiceName, false, stats), meshFaceDialer(deps, remote.ServiceNameWrite, false, stats), nil
	default:
		return nil, nil, fmt.Errorf("未知 transport %q（可选：auto|relay|webrtc）", transport)
	}
}

// meshFaceDialer 构造单个服务面的 mesh 拨号器（打洞 + 按策略回落），并接线载体回传。
func meshFaceDialer(deps meshFactoryDeps, service string, allowRelayFallback bool, stats *carrierStats) remote.Dialer {
	return mesh.NewRemoteDialer(mesh.RemoteDialerConfig{
		Client:             deps.RelayFor(service),
		Signaler:           deps.Signaler,
		Service:            service,
		AllowRelayFallback: allowRelayFallback,
		ICE:                deps.ICE,
		Logger:             deps.Logger,
		OnCarrier:          stats.record,
	})
}

// newMeshRuntimeInfoProvider 构造 `GET /api/mesh/status` 的**运行态**提供者：
// 实际监听地址（配置写 `:0` 时只有 listener 知道）+ node 角色是否成功启动。
//
// nodeRoleRunning 传函数而非布尔：装配层在角色启动**之后**才知道结果，视图必须读到最新值
// （传值会把启动前的 false 固定下来，导致「角色其实起了但状态显示未运行」）。
func newMeshRuntimeInfoProvider(readAddr, writeAddr string, nodeRoleRunning func() bool) func() server.MeshRuntimeInfo {
	return func() server.MeshRuntimeInfo {
		info := server.MeshRuntimeInfo{RemoteReadAddr: readAddr, RemoteWriteAddr: writeAddr}
		if nodeRoleRunning != nil {
			info.NodeRoleRunning = nodeRoleRunning()
		}
		return info
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
	deps, err := buildMeshFactoryDeps(cfg, h, log.With("component", "mesh_sync"))
	if err != nil {
		// 不注入：`kind=mesh` 的远端将以 ErrMeshTransportNotWired 明确失败（**绝不回落 direct**）。
		log.Warn("mesh 载体未装配（kind=mesh 的远端将 fail-closed）", "error", err)
		return
	}
	exec.SetMeshFSFactory(newMeshFSFactory(deps))
	log.Info("mesh 载体已装配",
		"identity", deps.Identity.Fingerprint(),
		"hub", meshHubEndpoint(cfg), "remote_hub", cfg.Mesh.HubURL != "",
		"signaling", deps.Signaler != nil, "instance_ice", deps.ICE != nil)
}

// buildMeshFactoryDeps 由 server 配置构造装配依赖（Y 二期）：
//
//	hub 客户端 ← mesh.hub_url（**空 = 本机自连**，凭据取本机自用凭据；非空 = 远端 hub + 配置凭据）
//	信令      ← mesh.node_id（空 = 无信令 ⇒ 只能中继；transport=webrtc 时配置层已拒绝）
//	ICE       ← mesh.stun/turn（**实例级**，不污染 webrtc 包级全局）
//
// 任一必需项缺失都返回错误（调用方据此不注入并告警，保持 fail-closed）。
func buildMeshFactoryDeps(cfg *server.Config, h *server.Handlers, log *slog.Logger) (meshFactoryDeps, error) {
	ak, sk, skeyID, err := meshHubCredential(cfg, h)
	if err != nil {
		return meshFactoryDeps{}, err
	}
	hc, err := newMeshHubClient(cfg, ak, sk, skeyID)
	if err != nil {
		return meshFactoryDeps{}, err
	}
	id, err := server.LoadXferIdentity(cfg)
	if err != nil {
		return meshFactoryDeps{}, fmt.Errorf("身份加载失败: %w", err)
	}
	deps := meshFactoryDeps{
		RelayFor: func(string) remote.RelayClient { return hc },
		Identity: id,
		ICE:      meshICEFromConfig(cfg.Mesh),
		Logger:   log,
	}
	if cfg.Mesh.NodeID != "" {
		// 仅当确实配了 node_id 才构造信令；`buildMeshDialers` 侧另有 `mesh.SignalerUsable`
		// 守卫（同时排除 nil 接口与 typed nil——后者是接口入参最易踩的陷阱）。
		sig, sErr := newMeshSignaler(cfg, ak, sk, skeyID)
		if sErr != nil {
			return meshFactoryDeps{}, sErr
		}
		deps.Signaler = sig
	}
	return deps, nil
}

// meshHubEndpoint 返回 hub 的实际 base URL（远端用配置值；本机用派生值；派生失败返回空串，
// 仅用于日志，不参与判定）。
func meshHubEndpoint(cfg *server.Config) string {
	if cfg.Mesh.HubURL != "" {
		return cfg.Mesh.HubURL
	}
	base, err := localSelfBaseURL(cfg)
	if err != nil {
		return ""
	}
	return base
}

// meshHubCredential 取访问 hub 的 SproxySig 凭据：
// 远端 hub ⇒ 配置里的 mesh.access_key/secret/skey_id（配置层已校验齐备）；
// 本机 hub ⇒ 本机自用凭据（`Handlers.SelfCredential`，Ring 为空则 fail-closed）。
func meshHubCredential(cfg *server.Config, h *server.Handlers) (ak, sk, skeyID string, err error) {
	if cfg.Mesh.HubURL != "" {
		return cfg.Mesh.AccessKey, cfg.Mesh.AccessKeySecret, cfg.Mesh.SkeyID, nil
	}
	ak, sk, skeyID, ok := h.SelfCredential()
	if !ok {
		return "", "", "", fmt.Errorf("本机 hub 模式需要本机凭据（credential Ring 为空）")
	}
	return ak, sk, skeyID, nil
}

// newMeshHubClient 构造 hub API 客户端：远端 hub 用配置 URL + 凭据；本机 hub 复用自连接客户端
// （同一份实现：base URL 派生 + 自签证书放宽信任）。
func newMeshHubClient(cfg *server.Config, ak, sk, skeyID string) (*client.FileClient, error) {
	if cfg.Mesh.HubURL == "" {
		return newLocalSelfClient(cfg, ak, sk, skeyID)
	}
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("远端 hub 需要 mesh.access_key/access_key_secret（fail-closed）")
	}
	opts := []client.Option{client.WithAccessKey(ak, sk)}
	if skeyID != "" {
		opts = append(opts, client.WithAccessKeyID(skeyID))
	}
	if cfg.Mesh.InsecureTLS {
		opts = append(opts, client.WithInsecureTLS())
	}
	return client.NewFileClient(cfg.Mesh.HubURL, opts...), nil
}

// newMeshSignaler 构造 WebRTC hub 信令客户端（打洞必需）：base URL 与凭据同 hub 客户端；
// `node_id` 为信令对端识别用的本节点 ID。
//
// TLS：远端 hub 的 `insecure_tls`、或本机 hub + 本机 TLS（自签）场景下注入放宽校验的
// http.Client——否则信令长轮询会因证书链校验失败而永远拿不到 Offer/Answer。
func newMeshSignaler(cfg *server.Config, ak, sk, skeyID string) (*hub.HubSignaler, error) {
	base := cfg.Mesh.HubURL
	needInsecure := cfg.Mesh.InsecureTLS
	if base == "" {
		var err error
		if base, err = localSelfBaseURL(cfg); err != nil {
			return nil, fmt.Errorf("信令 base URL 派生失败: %w", err)
		}
		// 本机 hub + 本机 TLS（通常自签）⇒ 放宽校验（只影响本进程到本机的连接）。
		if cfg.TLS.Enabled {
			needInsecure = true
		}
	}
	sig := hub.NewHubSignaler(base, ak, cfg.Mesh.NodeID)
	sig.SetAccessKeySecret(sk)
	if skeyID != "" {
		sig.SetAccessKeyID(skeyID)
	}
	if needInsecure {
		sig.SetHTTPClient(&http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 显式选择：自签场景
			},
		})
	}
	return sig, nil
}

// meshICEFromConfig 由 `mesh` 段构造**实例级** ICE 配置；全空 ⇒ nil（= 用 webrtc 包级全局，
// 即「未配实例 ICE」的零回归路径）。
func meshICEFromConfig(m server.MeshConfig) *webrtc.ICEOptions {
	if len(m.STUN) == 0 && len(m.TURN) == 0 && m.TURNUser == "" && m.TURNPassword == "" {
		return nil
	}
	return &webrtc.ICEOptions{
		STUNServers:  m.STUN,
		TURNServers:  m.TURN,
		TURNUser:     m.TURNUser,
		TURNPassword: m.TURNPassword,
	}
}
