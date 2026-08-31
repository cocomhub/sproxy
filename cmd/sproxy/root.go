// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/cmd/sproxy/internal/sproxycfg"
	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	kad "github.com/cocomhub/sproxy/pkg/tunnel/hub/ext/kad"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
	wsxfer "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
	"github.com/spf13/cobra"
)

const (
	flagConfig          = "config"
	flagAddr            = "addr"
	flagUploadsDir      = "uploads-dir"
	flagVersion         = "version"
	flagNoTLS           = "no-tls"
	flagAllowNoAuth     = "allow-no-auth"
	defaultConfig       = "sproxy.yaml"
	cfgAddr             = "addr"
	cfgUploadsDir       = "uploads_dir"
	logListenClosed     = "listen and serve closed"
	logHandlersCloseErr = "handlers close error"
	errFmtListenServe   = "listen and serve error: %w"
)

var (
	cfgFile     string
	cfgPtr      atomic.Pointer[server.Config]
	cfgProvider *sproxycfg.ViperProvider

	// testSignalCh 用于测试注入 signal channel；为 nil 时 runServer 创建自己的 channel。
	testSignalCh chan os.Signal
)

var rootCmd = &cobra.Command{
	Use:   "sproxy",
	Short: "轻量文件上传/下载/删除服务 + 加密隧道",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfgProvider = sproxycfg.New(cfgFile)
		cfgProvider.BindPFlag(cfgAddr, cmd.Flags().Lookup(flagAddr))
		cfgProvider.BindPFlag(cfgUploadsDir, cmd.Flags().Lookup(flagUploadsDir))
		// --no-tls 不绑定到 viper，在 buildServerConfig 中直接处理
		return nil
	},
	RunE: runServer,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize()

	rootCmd.PersistentFlags().StringVar(&cfgFile, flagConfig, defaultConfig, "配置文件路径")

	rootCmd.Flags().String(flagAddr, ":18083", "监听地址")
	rootCmd.Flags().String(flagUploadsDir, "./uploads", "上传目录")
	rootCmd.Flags().Bool(flagVersion, false, "打印版本与构建信息后退出")
	rootCmd.Flags().Bool(flagNoTLS, false, "禁用 TLS（覆盖 tls.enabled 配置）")
	rootCmd.Flags().Bool(flagAllowNoAuth, false, "允许无认证启动（无 access_keys/api_keys；仅限本地调试，生产勿用））")

	rootCmd.AddCommand(NewVersionSubcommand())
}

func runServer(cmd *cobra.Command, args []string) error {
	// --version 处理
	if showVer, _ := cmd.Flags().GetBool(flagVersion); showVer {
		fmt.Printf("Version: %s\n", Version)
		fmt.Printf("BuildAt: %s\n", BuildAt)
		return nil
	}

	cfg, err := buildServerConfig(cmd)
	if err != nil {
		return err
	}
	// fail-fast：无 access_keys 且 api_keys 未启用时拒绝启动（无法提供认证）。
	// --allow-no-auth 显式跳过（本地无认证调试/开发，Web UI 无凭据直连仍需可用）。
	if len(cfg.AccessKeys) == 0 && !cfg.APIKeys.Enabled {
		allow, _ := cmd.Flags().GetBool(flagAllowNoAuth)
		if !allow {
			return fmt.Errorf("拒绝启动：未配置 access_keys（且 api_keys 未启用），无法提供认证")
		}
		slog.Warn("无 access_keys/api_keys——以允许无认证模式启动（请仅用于本地调试，勿在生产开放）")
	}
	// M-8：api_keys-only（多用户 Bearer）下隧道/hub 不可用——隧道密钥由 access_keys 派生、
	// hub 注册由 access_keys 准入。启用 hub 时强制 access_keys 非空，消除功能死角。
	if cfg.Hub.Enabled && len(cfg.AccessKeys) == 0 {
		return fmt.Errorf("拒绝启动：hub.enabled=true 但未配置 access_keys，中继节点注册需要 SproxySig 准入")
	}
	cfgPtr.Store(cfg)

	logger := initLogger(cfg)
	slog.Info("config loaded", "path", cfgFile, "log_level", levelString(cfg.LogLevel), "log_format", formatString(cfg.LogFormat))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.NewServeMux()
	var routeTable *hub.MeshRouteTable
	var persist *hub.Persister          // hub 状态持久化器（仅 hub.enabled 且 persist_file 非空时创建）
	var restoredMsgs []hub.MessageSnap  // 启动时从持久化恢复的信令收件箱（灌入 SignalBroker）
	var restoredSnap *hub.Snapshot      // 启动时从持久化恢复的完整快照（灌回虚拟 IP 分配器）
	var hubDHT hub.DHT                  // hub 节点发现表（hub.dht: kad 时装配；注入 HubServer 与 Handlers）
	var fedClient *hub.FederationClient // hub 联邦节点表同步客户端（hub.federation.enabled 时装配；注入 Handlers）
	// Hub 中继：先创建 MeshRouteTable + HubServer 收口（ws/tcp 传输共用注册/中继逻辑），
	// 再按传输配置挂载 WS 升级端点与裸 TCP listener，最后注册 HTTP 路由。
	if cfg.Hub.Enabled {
		routeTable = hub.NewMeshRouteTable()
		logger.Info("Hub 中继模式已启用", "node_id", cfg.Hub.NodeID)

		// hub 状态持久化：配置了 persist_file 时加载历史快照恢复节点注册，
		// 并让后续注册/移除变更异步落盘（sproxy 启动后经 handlers 的 SetOnChange 触发）。
		// 文件缺失或损坏均按空状态启动（不因损坏文件拒绝启动，见 Persister.Load）。
		if cfg.Hub.PersistFile != "" {
			persist = hub.NewPersister(cfg.Hub.PersistFile)
			if snap, err := persist.Load(); err != nil {
				return fmt.Errorf("读取 hub 持久化文件失败: %w", err)
			} else if snap != nil {
				hub.RestoreFromSnapshot(routeTable, snap)
				restoredMsgs = snap.Messages
				restoredSnap = snap
				if len(snap.Nodes) > 0 || len(snap.Messages) > 0 {
					logger.Info("hub 状态已从持久化恢复", "file", cfg.Hub.PersistFile, "nodes", len(snap.Nodes), "messages", len(snap.Messages))
				}
			}
		}
		// 节点注册准入：SproxySig AccessKey + HMAC proof（共享 token 已废除）。
		// hub 准入凭据来自顶层 access_keys 配置，转换后交给 hub.Authenticator。
		// ws 与 tcp 传输共用同一 HubServer（同一路由表/信号量/鉴权器）。
		aks := make([]hub.AccessKey, 0, len(cfg.AccessKeys))
		for _, k := range cfg.AccessKeys {
			aks = append(aks, hub.AccessKey{Key: k.Key, Secret: k.Secret})
		}
		hubSrv := hub.NewHubServer(routeTable, hub.NewAuthenticator(aks), logger.With("component", "hub"), cfg.Hub.MaxConnections)
		// 虚拟 IP 分配：按 hub.virtual_subnet 配置的子网构建分配器（默认 CGNAT
		// 100.64.0.0/10，config.Validate 已保证 IPv4）。分配权在 hub，节点不可自选。
		// S-5：防御兜底同时覆盖非法 CIDR 与 IPv6（NewHubAllocator 对非 IPv4 panic，
		// 此处避免把 IPv6 前缀传给它）。默认分配器已在 NewHubServer 建立。
		if prefix, perr := netip.ParsePrefix(cfg.Hub.VirtualSubnet); perr == nil {
			if prefix.Addr().Is4() {
				hubSrv.SetAllocator(hub.NewHubAllocator(prefix))
			} else {
				logger.Warn("hub.virtual_subnet 非 IPv4，使用默认子网", "virtual_subnet", cfg.Hub.VirtualSubnet)
			}
		} else {
			logger.Warn("hub.virtual_subnet 非法，使用默认子网", "virtual_subnet", cfg.Hub.VirtualSubnet, "error", perr)
		}
		// 重启快照重建分配表：把已持久化的 (mesh,nodeID)→VIP 灌回分配器，
		// 避免把已持久化的 VIP 再分给新节点（DoD 1）。
		// R-5：快照内虚拟 IP 冲突/越界（损坏或伪造持久化文件）时显式记录 Error——
		// 被拒条目不保留，可能使已持久化节点重启后拿新 VIP；清晰告警供运维定位，
		// 不再静默以空表启动掩盖问题。
		if restoredSnap != nil {
			if perr := hub.PreloadAllocator(hubSrv.Allocator(), restoredSnap); perr != nil {
				logger.Error("虚拟 IP 分配表快照重建冲突（冲突条目不保留；对应节点重启后可能拿到新虚拟 IP）", "error", perr)
			}
		}
		// DHT 节点发现表（hub.dht: kad）：装配 Kademlia，注册进 DHTRegistry，
		// 注入 HubServer（注册时喂入 DHT）与 Handlers（/api/hub/nodes 合并候选）。
		// 路由表仍 hub 权威；DHT 只提供候选节点/发现，不改路由表状态。
		if cfg.Hub.DHT == "kad" {
			dhtNodeID := cfg.Hub.NodeID
			if dhtNodeID == "" {
				dhtNodeID = "hub-dht"
			}
			// 装配 Kademlia 进 DHTRegistry（Active 返回最高优先级实现 = kad），
			// 随后经 DHTRegistry.Active() 注入 HubServer/Handlers——registry 是
			// 实际选择机制（非装饰性副作用）。
			hub.RegisterDHT("kad", kad.NewDHTNode(dhtNodeID, nil, logger.With("component", "dht")), 10)
			hubDHT = hub.DHTRegistry.Active()
			if len(cfg.Hub.DHTSeeds) > 0 {
				// 多 hub DHT 组网未实现，种子暂不引导（kad.Bootstrap 现会把种子
				// 当假 ID 节点插入路由表，污染发现列表）；预留配置，未来实现
				// 真实 bootstrap 时再消费。
				logger.Warn("hub.dht_seeds 预留（多 hub DHT 组网未实现），暂不引导", "seeds", cfg.Hub.DHTSeeds)
			}
			hubSrv.SetDHT(hubDHT)
			logger.Info("Hub DHT 已启用", "impl", "kad", "node_id", dhtNodeID)
		}
		// hub 联邦（hub-to-hub peering）：配置 hub.federation.peers 时周期拉取
		// 对端 hub 节点表（联邦候选），/api/hub/nodes 合并（路由表权威 +
		// DHT + 联邦候选，去重）。入站端点 /api/hub/federation/nodes 由
		// RegisterRoutes 在 hub.enabled 且 federation.enabled 时注册。
		// 拉取认证复用 SproxySig AccessKey（对端 hub 配置的 access_keys）；
		// peer URL 为空回落默认 loopback（远程 peering 需显式配置，见
		// Config.Validate）。联邦只提供发现/可达性，不改路由表状态。
		// federation.persist_file 非空时启用候选持久化（重启后恢复上次同步的
		// 候选节点，不冷启动；损坏/缺失文件按空候选启动）。
		if cfg.Hub.Federation.Enabled {
			peers := make([]hub.FederationPeer, 0, len(cfg.Hub.Federation.Peers))
			for _, p := range cfg.Hub.Federation.Peers {
				peers = append(peers, hub.FederationPeer{
					ID:                 p.ID,
					URL:                p.URL,
					AccessKey:          p.AccessKey,
					AccessKeySecret:    p.AccessKeySecret,
					CAFile:             p.CAFile,
					InsecureSkipVerify: p.InsecureSkipVerify,
				})
			}
			var ferr error
			fedClient, ferr = hub.NewFederationClientWithPersist(peers, cfg.Hub.Federation.Interval, cfg.Hub.Federation.Timeout, logger.With("component", "hub_federation"), cfg.Hub.Federation.PersistFile)
			if ferr != nil {
				return fmt.Errorf("初始化 hub 联邦客户端: %w", ferr)
			}
			fedClient.Start(ctx)
			defer fedClient.Close()
			logger.Info("Hub 联邦已启用", "peers", len(cfg.Hub.Federation.Peers), "interval", cfg.Hub.Federation.Interval, "persist_file", cfg.Hub.Federation.PersistFile)
		}
		if cfg.Hub.Transports.WS.Enabled {
			// S36：WS 升级路径固定为 /ws。hub.transports.ws.path 已废弃，
			// 非默认值时仅记录警告并忽略，避免可配置 path 与既有业务路由语义重叠。
			wsPath := "/ws"
			if configured := cfg.Hub.Transports.WS.Path; configured != "" && configured != wsPath {
				logger.Warn("hub.transports.ws.path 已废弃，WS 升级路径固定为 /ws，忽略配置值", "configured", configured)
			}
			// 挂载 WebSocket 升级端点到主 mux；连接后由 HubServer 处理注册与转发。
			hubNode := wsxfer.NewHandlerNode()
			hubNode.AddToMux(mux, wsPath)
			go func() {
				for {
					conn, aerr := hubNode.Accept(ctx)
					if aerr != nil {
						return
					}
					// I30：连接并发上限由 HubServer 信号量控制；超限立即关闭新连接。
					if !hubSrv.TryHandleConn(ctx, conn) {
						logger.Warn("Hub 连接数达到上限，拒绝新连接", "max", cfg.Hub.MaxConnections)
						_ = conn.Close()
						continue
					}
				}
			}()
		}
		if cfg.Hub.Transports.TCP.Enabled {
			// 裸 TCP 中继：独立 raw TCP listener（复用注册/鉴权/中继逻辑，传输层从
			// ws 扩到 tcp）。同步绑定（端口占用等错误 fail-fast，而非后台静默失败），
			// accept 循环在 goroutine 中运行。
			tcpListen := cfg.Hub.Transports.TCP.Listen
			if tcpListen == "" {
				tcpListen = server.DefaultHubTCPListen
			}
			tcpLn, lerr := hubSrv.ListenTCP(ctx, tcpListen)
			if lerr != nil {
				return fmt.Errorf("hub TCP 中继监听失败: %w", lerr)
			}
			defer tcpLn.Close()
			go func() {
				if aerr := hubSrv.AcceptTCP(ctx, tcpLn); aerr != nil && ctx.Err() == nil {
					logger.Error("Hub TCP 中继 accept 退出", "addr", tcpListen, "error", aerr)
				}
			}()
			logger.Info("Hub TCP 中继已启用", "addr", tcpListen)
		}
	}
	h := server.RegisterRoutes(ctx, server.RegisterRoutesOpts{
		Mux:                 mux,
		CfgPtr:              &cfgPtr,
		Version:             Version,
		BuildAt:             BuildAt,
		Logger:              logger,
		RouteTable:          routeTable,
		HubPersist:          persist,
		HubRestoredMessages: restoredMsgs,
	})
	if hubDHT != nil {
		h.SetDHT(hubDHT) // /api/hub/nodes 合并 DHT 候选节点（发现源：路由表权威 + DHT 候选）
	}
	if fedClient != nil {
		h.SetFederationClient(fedClient) // /api/hub/nodes 合并联邦候选节点（发现源：+ 联邦候选）
	}
	// 先停 SyncManager（drain 同步任务）再关 Handlers：defer LIFO，h.Close 先注册
	// （后执行），syncMgr.Stop 后注册（先执行）——同步任务收尾完成后才关 Handlers（审查 M-1）。
	defer func() {
		if err := h.Close(); err != nil {
			slog.Warn(logHandlersCloseErr, "error", err.Error())
		}
	}()
	// xfer listener（阶段 5 工作项 1）：接收 `sclient tunnel --xfer tcp/tcp+tls --hub <addr>`
	// 的会话，经 mux → tunnel 解密 → 路由到本地文件 API（h.LocalHandler() 的 localMux）。
	// 必须在 RegisterRoutes 之后启动（handler 彼时才构造）。注意用 LocalHandler() 而非
	// TunnelHandler()：xfer 隧道 handleStream 已解密请求体为明文，TunnelHandler() 是传统
	// POST /tunnel 的外层帧解密器（期望 ctx 带派生密钥 + 帧 body），直接使用会 401。
	// fail-closed：xfer 段启用但装配失败（无 access_keys / 无证书）→ 拒绝启动。
	if _, err := startXferListener(ctx, cfg, h.LocalHandler(), logger); err != nil {
		return err
	}
	// 文件同步 SyncManager：配置了 sync（sync.max_concurrent 或 sync_remotes 非空）时装配。
	// 远程访问用 HTTP 直连远程 sproxy（sync_remotes URL + SproxySig 凭据）；mesh 通道为后续增强。
	if cfg.Sync.MaxConcurrent > 0 || len(cfg.SyncRemotes) > 0 {
		remotes := make([]syncmgr.RemoteConfig, 0, len(cfg.SyncRemotes))
		for _, r := range cfg.SyncRemotes {
			remotes = append(remotes, syncmgr.RemoteConfig{
				Name: r.Name, URL: r.URL, AccessKey: r.AccessKey, AccessKeySecret: r.AccessKeySecret,
			})
		}
		syncMgr := syncmgr.NewManager(cfg.UploadsDir, h.SyncQuotaStore(), int(server.CategoryUserFiles),
			remotes, syncexec.NewExecutor(cfg.UploadsDir, logger.With("component", "sync_exec")),
			logger.With("component", "sync"),
			&syncmgr.Config{MaxConcurrent: cfg.Sync.MaxConcurrent, TaskTTL: cfg.Sync.TaskTTL})
		h.SetSyncMgr(syncMgr)
		defer syncMgr.Stop()
	}

	protocol := "http"
	if cfg.TLS.Enabled {
		protocol = "https"
	}
	displayHost, displayPort, _ := net.SplitHostPort(cfg.Addr)
	if displayHost == "" {
		displayHost = "127.0.0.1"
	}
	fmt.Printf("downserver start at: %s://%s:%s\n", protocol, displayHost, displayPort)
	fmt.Printf("uploads dir: %s\n", cfg.UploadsDir)

	srv := createHTTPServer(cfg, h.Handler())
	stopSigCh, shutdownDone := runSignalHandler(cancel, srv, h, logger, cfg)
	defer close(stopSigCh) // 确保所有退出路径上信号 goroutine 退出

	if cfg.TLS.Enabled {
		if err := startTLSListener(cfg, srv); err != nil {
			return err
		}
	} else {
		if err := startPlainListener(srv); err != nil {
			return err
		}
	}

	<-shutdownDone
	slog.Info("downserver exit")
	return nil
}

// xferListenerInfo 记录已启动的 xfer listener 信息（供测试与观测）。
type xferListenerInfo struct {
	// Name 是配置段名（xfer_tcp / xfer_tls）。
	Name string
	// Addr 是实际监听地址（listen 配置 :0 随机端口时为真实端口）。
	Addr string
	// TLS 表示该 listener 是否承载 TLS（xfer_tls 恒 true；xfer_tcp + tls_enabled 也为 true）。
	TLS bool
	// Fingerprint 是服务端 Ed25519 身份指纹（供客户端 `sclient config set
	// peer_fingerprints` 固化，AD-4）。
	Fingerprint string
}

// startXferListener 装配服务端 xfer listener（阶段 5 工作项 1 PR-3）。
//
// 接收 `sclient tunnel --xfer tcp/tcp+tls --hub <addr>` 的会话：accept 循环 →
// mux(RoleListener) → tunnel.NewTunnel(key, WithIdentity) → tun.Serve(ctx, tunnelHandler)。
// tunnelHandler 是 server.RegisterRoutes 构造的 localApiHandler（本地文件 API）。
//
// 关键正确性点：
//   - 握手密钥 = server.HubXferKey(cfg)（AD-3：access_keys 首对 SK + mesh 派生，与客户端一致）；
//   - 服务端身份 = server.LoadXferIdentity(cfg)（AD-4：Ed25519，指纹供客户端 pin）；
//   - fail-closed：xfer 段启用但无 access_keys / 无证书 → 返回 error（拒绝启动）；
//   - 默认绑 loopback（127.0.0.1:<port>），远程可达须显式 listen；
//   - 连接并入数量上限（cfg.Hub.MaxConnections 信号量）；**不注册路由表**（xfer 是
//     文件 API 隧道面，非中继节点面，不参与节点注册/VIP/DHT——hub 的 TryHandleConn
//     走注册帧语义，与 xfer 隧道帧不兼容，故独立信号量控制并发）。
func startXferListener(ctx context.Context, cfg *server.Config, tunnelHandler http.Handler, logger *slog.Logger) ([]xferListenerInfo, error) {
	var infos []xferListenerInfo
	if cfg == nil {
		return nil, fmt.Errorf("start xfer listener: 配置为 nil")
	}
	if tunnelHandler == nil {
		return nil, fmt.Errorf("start xfer listener: 隧道 handler 为 nil（需在 RegisterRoutes 之后调用）")
	}
	xferTLS := cfg.Hub.Transports.XferTLS
	xferTCP := cfg.Hub.Transports.XferTCP
	if !xferTLS.Enabled && !xferTCP.Enabled {
		return infos, nil // 未启用 xfer listener：无操作
	}
	// fail-closed：xfer listener 需要 access_keys 首对派生隧道密钥（AD-3）。
	key, err := server.HubXferKey(cfg)
	if err != nil {
		return nil, fmt.Errorf("start xfer listener: %w", err)
	}
	// 服务端 Ed25519 身份（AD-4），打印指纹供 `sclient config set peer_fingerprints` 固化。
	identity, err := server.LoadXferIdentity(cfg)
	if err != nil {
		return nil, fmt.Errorf("start xfer listener: 加载服务端身份失败: %w", err)
	}
	logger.Info("xfer 服务端身份已就绪", "fingerprint", identity.Fingerprint(), "file", server.XferIdentityPath(cfg))

	// TLS 传输（xfer_tls 恒 TLS；xfer_tcp 段 tls_enabled=true 升级）需要默认 *tls.Config。
	// 走 registry：builtin.SetDefaultTLSConfig 后经 xfer.Get("tcp+tls").Listen。
	// defaultTLSConfig 是 internal/tcp 包级全局，服务端进程内证书单一（同一 cfg），
	// 多段共享同一 TLS 配置无冲突。
	if xferTLS.Enabled || xferTCP.TLSEnabled {
		tlsCfg, tErr := server.BuildXferTLSConfig(cfg)
		if tErr != nil {
			return nil, fmt.Errorf("start xfer listener: %w", tErr)
		}
		builtin.SetDefaultTLSConfig(tlsCfg)
	}

	if xferTLS.Enabled {
		// xfer_tls 段恒 TLS（段名即约定），不消费 TLSEnabled 字段。
		info, sErr := startOneXferListener(ctx, cfg, "xfer_tls", xferTLS, true, key, identity, tunnelHandler, logger)
		if sErr != nil {
			return nil, sErr
		}
		infos = append(infos, info)
	}
	if xferTCP.Enabled {
		// xfer_tcp 段默认明文（显式 option），tls_enabled=true 升级为 TLS。
		info, sErr := startOneXferListener(ctx, cfg, "xfer_tcp", xferTCP, xferTCP.TLSEnabled, key, identity, tunnelHandler, logger)
		if sErr != nil {
			return nil, sErr
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// startOneXferListener 启动单个 xfer accept 循环（同步绑定，绑定失败 fail-fast）。
// tlsEnabled 显式指定传输方式：xfer_tls 段恒传 true；xfer_tcp 段传 tc.TLSEnabled。
func startOneXferListener(ctx context.Context, cfg *server.Config, name string, tc server.XferTransportConfig, tlsEnabled bool, key []byte, identity *tunnel.Identity, tunnelHandler http.Handler, logger *slog.Logger) (xferListenerInfo, error) {
	transportName := "tcp"
	if tlsEnabled {
		transportName = "tcp+tls"
	}
	listenAddr := tc.Listen
	if listenAddr == "" {
		if tlsEnabled {
			listenAddr = server.DefaultXferTLSListen
		} else {
			listenAddr = server.DefaultXferTCPListen
		}
	}
	tp := xfer.Get(transportName)
	if tp == nil {
		return xferListenerInfo{}, fmt.Errorf("start xfer listener %s: 传输层 %q 未注册", name, transportName)
	}
	ln, err := tp.Listen(ctx, listenAddr)
	if err != nil {
		return xferListenerInfo{}, fmt.Errorf("start xfer listener %s: 监听失败（%s %s）: %w", name, transportName, listenAddr, err)
	}
	addr := xferListenerAddr(ln)

	// 连接数上限信号量（复用 hub.max_connections 语义）。xfer 是隧道帧，不走 hub
	// 注册帧语义的 TryHandleConn（那会误读注册帧破坏隧道握手），故独立信号量控制
	// 并发，超限立即关闭新连接（防未认证/慢连接拖垮进程，C-1 DoS 收敛）。
	maxConns := cfg.Hub.MaxConnections
	if maxConns <= 0 {
		maxConns = 256
	}
	sem := make(chan struct{}, maxConns)

	go func() {
		defer func() { _ = ln.Close() }()
		for {
			conn, aErr := ln.Accept(ctx)
			if aErr != nil {
				if ctx.Err() != nil {
					return
				}
				logger.Error("xfer listener accept 退出", "name", name, "error", aErr)
				return
			}
			select {
			case sem <- struct{}{}:
			default:
				logger.Warn("xfer 连接数达到上限，拒绝新连接", "name", name, "max", maxConns)
				_ = conn.Close()
				continue
			}
			go func() {
				defer func() { <-sem }()
				m := mux.New(conn, mux.RoleListener)
				tun := tunnel.NewTunnel(m, key, tunnel.WithIdentity(identity))
				// Serve 同步执行 ECDH 握手（listener 侧）+ accept 循环；ctx 取消时返回。
				if sErr := tun.Serve(ctx, tunnelHandler); sErr != nil && ctx.Err() == nil {
					logger.Warn("xfer 隧道 Serve 退出", "name", name, "error", sErr)
				}
				_ = m.Close()
			}()
		}
	}()

	logger.Info("xfer listener 已启用", "name", name, "transport", transportName, "addr", addr)
	return xferListenerInfo{Name: name, Addr: addr, TLS: tlsEnabled, Fingerprint: identity.Fingerprint()}, nil
}

// xferListenerAddr 从 xfer.Listener 提取实际监听地址（支持实现暴露 Addr() 的
// listener，如内置 TcpListener/TlsListener）；否则返回空串。
func xferListenerAddr(ln xfer.Listener) string {
	if a, ok := ln.(interface{ Addr() net.Addr }); ok {
		return a.Addr().String()
	}
	return ""
}

// buildServerConfig 从 CLI 标志和配置文件构建服务器配置。
func buildServerConfig(cmd *cobra.Command) (*server.Config, error) {
	if cfgProvider == nil {
		configPath := cfgFile
		if configPath == "" {
			configPath = defaultConfig
		}
		cfgProvider = sproxycfg.New(configPath)
		cfgProvider.BindPFlag(cfgAddr, cmd.Flags().Lookup(flagAddr))
		cfgProvider.BindPFlag(cfgUploadsDir, cmd.Flags().Lookup(flagUploadsDir))
		if cfgFile == "" {
			cfgFile = configPath
		}
	}
	cfg, err := server.LoadFromProvider(cfgProvider)
	if err != nil {
		return nil, fmt.Errorf("配置解析失败: %w", err)
	}

	// --no-tls flag 覆盖 tls.enabled 配置
	if noTLS, _ := cmd.Flags().GetBool(flagNoTLS); noTLS {
		cfg.TLS.Enabled = false
	}

	return cfg, nil
}

// createHTTPServer 根据配置创建 *http.Server。
func createHTTPServer(cfg *server.Config, handler http.Handler) *http.Server {
	maxHeaderBytes := cfg.MaxHeaderBytes
	if maxHeaderBytes <= 0 {
		maxHeaderBytes = 1 << 20 // 1 MiB
	}
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadTimeout:       cfg.ServerTimeouts.Read,
		WriteTimeout:      cfg.ServerTimeouts.Write,
		IdleTimeout:       cfg.ServerTimeouts.Idle,
		ReadHeaderTimeout: cfg.ServerTimeouts.ReadHeader,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

// startTLSListener 启动 TLS/HTTPS 监听，使用 certmgr 管理证书生命周期。
// 支持自签证书、文件证书和 mTLS 配置。
func startTLSListener(cfg *server.Config, s *http.Server) error {
	cmCfg := &certmgr.Config{
		CertFile: cfg.TLS.CertFile,
		KeyFile:  cfg.TLS.KeyFile,
		AutoTLS:  cfg.TLS.AutoTLS,
		ClientCA: cfg.TLS.ClientCA,
		ACME: certmgr.ACMEConfig{
			Enabled:    cfg.TLS.ACME.Enabled,
			Domains:    cfg.TLS.ACME.Domains,
			Email:      cfg.TLS.ACME.Email,
			CacheDir:   cfg.TLS.ACME.CacheDir,
			HTTP01:     cfg.TLS.ACME.HTTP01,
			HTTP01Port: cfg.TLS.ACME.HTTP01Port,
		},
	}
	mgr, err := certmgr.New(cmCfg)
	if err != nil {
		return fmt.Errorf("创建证书管理器失败: %w", err)
	}
	defer func() {
		if closeErr := mgr.Close(); closeErr != nil {
			slog.Warn("证书管理器关闭失败", "error", closeErr)
		}
	}()
	tlsCfg, err := mgr.TLSConfig()
	if err != nil {
		return fmt.Errorf("获取 TLS 配置失败: %w", err)
	}
	s.TLSConfig = tlsCfg
	slog.Info("TLS enabled", "cert_file", cfg.TLS.CertFile, "auto_tls", cfg.TLS.AutoTLS, "client_ca", cfg.TLS.ClientCA, "acme", cfg.TLS.ACME.Enabled)

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf(errFmtListenServe, err)
	}
	writeBackActualAddr(ln.Addr().String())
	if err := s.ServeTLS(ln, "", ""); err != nil {
		if err == http.ErrServerClosed {
			slog.Info(logListenClosed, "error", err.Error())
		} else {
			return fmt.Errorf(errFmtListenServe, err)
		}
	}
	return nil
}

// startPlainListener 启动非 TLS HTTP 监听。
func startPlainListener(s *http.Server) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf(errFmtListenServe, err)
	}
	writeBackActualAddr(ln.Addr().String())
	if err := s.Serve(ln); err != nil {
		if err == http.ErrServerClosed {
			slog.Info(logListenClosed, "error", err.Error())
		} else {
			return fmt.Errorf(errFmtListenServe, err)
		}
	}
	return nil
}

// writeBackActualAddr 把实际监听地址写回 cfgPtr（配置 :0 随机端口时反映真实端口，
// 供测试与观测获取实际监听地址；固定端口时值不变）。
func writeBackActualAddr(addr string) {
	if old := cfgPtr.Load(); old != nil {
		updated := *old
		updated.Addr = addr
		cfgPtr.Store(&updated)
	}
}

// runSignalHandler 启动信号处理 goroutine，返回 stopSigCh（关闭后通知 goroutine 退出）和 shutdownDone（清理完成后关闭）。
func runSignalHandler(cancel context.CancelFunc, s *http.Server, h *server.Handlers, logger *slog.Logger, cfg *server.Config) (chan struct{}, chan struct{}) {
	signalChan := make(chan os.Signal, 1)
	if testSignalCh != nil {
		signalChan = testSignalCh
	}
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP)

	stopSigCh := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		defer signal.Stop(signalChan)
		for {
			select {
			case <-stopSigCh:
				return
			case sig, ok := <-signalChan:
				if !ok {
					return
				}
				if sig == syscall.SIGHUP {
					handleSighup(cfg)
					continue
				}
				handleSignalShutdown(cancel, s, h)
				return
			}
		}
	}()
	return stopSigCh, shutdownDone
}

// handleSignalShutdown 执行优雅关闭：取消 context、关闭 HTTP 服务器和 handlers。
func handleSignalShutdown(cancel context.CancelFunc, s *http.Server, h *server.Handlers) {
	cancel()
	currentCfg := cfgPtr.Load()
	shutdownTimeout := currentCfg.ServerTimeouts.Shutdown
	if shutdownTimeout <= 0 {
		shutdownTimeout = 30 * time.Second
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	if err := s.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err.Error(), "timeout", shutdownTimeout)
	}
	shutdownCancel()
	if err := h.Close(); err != nil {
		slog.Warn(logHandlersCloseErr, "error", err.Error())
	}
}

// handleSighup 处理 SIGHUP 信号：使用 Provider 重新读取配置文件，
// 仅 log_level/log_format 等软配置生效（tunnel_key 已废除）。
func handleSighup(oldCfg *server.Config) {
	if err := cfgProvider.Refresh(); err != nil {
		slog.Error("SIGHUP config reload failed", "error", err)
		return
	}
	newCfg, err := server.LoadFromProvider(cfgProvider)
	if err != nil {
		slog.Error("SIGHUP config parse failed", "error", err)
		return
	}

	if oldCfg.Addr != newCfg.Addr {
		slog.Warn("addr 修改在 SIGHUP 后不会生效，需要重启进程", "old", oldCfg.Addr, "new", newCfg.Addr)
	}
	if oldCfg.UploadsDir != newCfg.UploadsDir {
		slog.Warn("uploads_dir 修改在 SIGHUP 后不会生效（ChecksumStore 不重建），需要重启进程", "old", oldCfg.UploadsDir, "new", newCfg.UploadsDir)
	}
	if oldCfg.RateLimit != newCfg.RateLimit {
		slog.Warn("rate_limit 修改在 SIGHUP 后不会生效，需要重启进程")
	}
	if oldCfg.ServerTimeouts != newCfg.ServerTimeouts {
		slog.Warn("server_timeouts 修改在 SIGHUP 后不会生效（http.Server 未重建），需要重启进程")
	}
	if oldCfg.MaxHeaderBytes != newCfg.MaxHeaderBytes {
		slog.Warn("max_header_bytes 修改在 SIGHUP 后不会生效（http.Server 未重建），需要重启进程")
	}
	if oldCfg.TLS.Enabled != newCfg.TLS.Enabled {
		slog.Warn("tls.enabled 修改在 SIGHUP 后不会生效（http.Server 未重建），需要重启进程",
			"old", oldCfg.TLS.Enabled, "new", newCfg.TLS.Enabled)
	}

	initLogger(newCfg)
	slog.Info("config reloaded via SIGHUP", "path", cfgFile, "log_level", levelString(newCfg.LogLevel))
	cfgPtr.Store(newCfg)
}

func initLogger(cfg *server.Config) *slog.Logger {
	level := slog.LevelInfo
	switch levelString(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch formatString(cfg.LogFormat) {
	case "json":
		h = slog.NewJSONHandler(os.Stdout, opts)
	default:
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger
}

func levelString(s string) string {
	switch s {
	case "debug", "info", "warn", "error":
		return s
	default:
		return "info"
	}
}

func formatString(s string) string {
	switch s {
	case "json", "text":
		return s
	default:
		return "text"
	}
}
