// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"path/filepath"

	"github.com/cocomhub/sproxy/cmd/sproxy/internal/sproxycfg"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/telemetry"
	oteltracing "github.com/cocomhub/sproxy/pkg/telemetry/ext/otel"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	kad "github.com/cocomhub/sproxy/pkg/tunnel/hub/ext/kad"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic" // 注册 QUIC 传输层（hub.transports.quic）
	wsxfer "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
	s3ext "github.com/cocomhub/sproxy/pkg/volume/ext/s3"
	"github.com/cocomhub/sproxy/pkg/volume/federated"
	"github.com/cocomhub/sproxy/pkg/volume/sftp"
	"github.com/cocomhub/sproxy/pkg/volume/webdav"
	"github.com/spf13/cobra"
)

const (
	flagConfig          = "config"
	flagAddr            = "addr"
	flagStorageRoot     = "storage-root"
	flagVersion         = "version"
	flagNoTLS           = "no-tls"
	flagAllowNoAuth     = "allow-no-auth"
	defaultConfig       = "sproxy.yaml"
	cfgAddr             = "addr"
	cfgStorageRoot      = "storage_root"
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
		cfgProvider.BindPFlag(cfgStorageRoot, cmd.Flags().Lookup(flagStorageRoot))
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
	rootCmd.Flags().String(flagStorageRoot, "./storage", "存储根目录")
	rootCmd.Flags().Bool(flagVersion, false, "打印版本与构建信息后退出")
	rootCmd.Flags().Bool(flagNoTLS, false, "禁用 TLS（覆盖 tls.enabled 配置）")
	rootCmd.Flags().Bool(flagAllowNoAuth, false, "允许无认证启动（无任何凭据 api_keys/store 时回环调试放行；仅限本地调试，生产勿用）")

	rootCmd.AddCommand(NewVersionSubcommand())
	rootCmd.AddCommand(newCmdDav())
	rootCmd.AddCommand(newCmdBaidupcs())
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
	// 凭据 store 化装配：SproxySig 权威表 = Ring（取代 yaml access_keys）。
	// 载入 <storage_root>/anonymous/meta/credentials.json；**U3：零凭据启动**——store
	// 为空不生成 anonymous 凭据，系统以零凭据等 register 公开端点接入首个 admin
	// （首个经回环注册的用户由 AddRegistration 原子授 admin）。
	// api_keys.enabled 时 Ring 仍装配（hub 准入与隧道派生仍需 AK/SK）。
	credRing, credStore, err := server.BootstrapServerCredentials(cfg, slog.Default())
	if err != nil {
		return fmt.Errorf("装配凭据 Ring 失败: %w", err)
	}
	cfgPtr.Store(cfg)

	logger := initLogger(cfg)
	slog.Info("config loaded", "path", cfgFile, "log_level", levelString(cfg.LogLevel), "log_format", formatString(cfg.LogFormat))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// telemetry 装配：telemetry.enabled=true 时创建 OTel provider（autoexport 按环境
	// 变量驱动 exporter），其 Tracer 注入 server 请求路径——requestLogMiddleware 的
	// trace/span id 改由 OTel 生成，同时经 SpanContextKey 同步进 slog 日志（见
	// telemetry.WithContextHandler）。enabled=false（默认）时 tracer 为 nil，server
	// 保持自生成 id 的既有行为。provider 装配失败（非法采样率/端点）fail-fast 拒绝启动。
	var tracer telemetry.Tracer // nil = 关闭（默认）
	if cfg.Telemetry.Enabled {
		tp, terr := oteltracing.NewProvider(
			oteltracing.WithSampleRatio(cfg.Telemetry.SampleRatio),
			oteltracing.WithOTLPEndpoint(cfg.Telemetry.OTLPEndpoint),
		)
		if terr != nil {
			return fmt.Errorf("初始化 telemetry provider 失败: %w", terr)
		}
		// 停服时冲刷并关闭 TracerProvider（幂等）。
		defer func() {
			if cerr := tp.Shutdown(context.Background()); cerr != nil {
				logger.Warn("telemetry provider 关停失败", "error", cerr)
			}
		}()
		tracer = tp.Tracer("sproxy")
		logger.Info("telemetry 已启用", "sample_ratio", cfg.Telemetry.SampleRatio, "otlp_endpoint", cfg.Telemetry.OTLPEndpoint)
	}

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
		// hub 与 HTTP 面共用同一个凭据 Ring（credRing，单一事实源）：
		// rotate / 过期在 ring 上动态生效，无需在 hub 侧做任何同步。
		hubSrv := hub.NewHubServer(routeTable, hub.NewAuthenticator(credRing), logger.With("component", "hub"), cfg.Hub.MaxConnections)
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
			kadDHT := kad.NewDHT(dhtNodeID, nil, logger.With("component", "dht"))
			// k-bucket 持久化（缓存语义，路由表仍 hub 权威）：配置
			// hub.dht_persist_file 时启动 Load 恢复上次发现缓存（不冷启动），
			// 后续 Register/Remove 变更经去抖异步落盘。文件缺失/损坏/超限按
			// 空桶启动（kad.Load 语义）；Load 的其余 I/O 错误 fail-fast。
			if cfg.Hub.DHTPersistFile != "" {
				if perr := kadDHT.EnablePersistence(cfg.Hub.DHTPersistFile); perr != nil {
					return fmt.Errorf("初始化 kad DHT 持久化失败: %w", perr)
				}
				logger.Info("kad DHT k-bucket 持久化已启用", "file", cfg.Hub.DHTPersistFile)
			}
			hub.RegisterDHT("kad", kadDHT, 10)
			hubDHT = hub.DHTRegistry.Active()
			// 停服时 flush 去抖窗口内未落盘的 k-bucket 变更（hubDHT.Close 委托
			// kad.FlushPersist；内存 DHT 的 Close 是 no-op）。
			// 审查 PR-3 M-1：flush 失败（写盘错误）记录 Error，不静默吞（禁止静默失败）。
			defer func() {
				if cerr := hubDHT.Close(); cerr != nil {
					logger.Error("kad DHT 关停 flush 失败", "err", cerr)
				}
			}()
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
		// 拉取认证复用 SproxySig AccessKey（对端 hub 凭据 Ring 登记的 AK/SK）；
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
					AccessKeyID:        p.AccessKeyID,
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
			// WS 升级路径（roadmap §5.3 P1 被动伪装层：形态对齐，贴近业务路径可配）。
			// 默认 /ws 零回归；显式配置生效（客户端须同步同一路径）。
			wsPath := cfg.Hub.Transports.WS.Path
			if wsPath == "" {
				wsPath = "/ws"
			}
			if !strings.HasPrefix(wsPath, "/") {
				wsPath = "/" + wsPath
			}
			if wsPath != "/ws" {
				logger.Info("WS 升级路径已自定义（形态对齐）", "path", wsPath)
			}
			// 挂载 WebSocket 升级端点到主 mux；连接后由 HubServer 处理注册与转发。
			hubNode := wsxfer.NewHandlerNode(wsxfer.WithUpgradeHeader(cfg.Hub.Transports.WS.UpgradeHeader))
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
		if cfg.Hub.Transports.QUIC.Enabled {
			// QUIC 中继：独立 raw UDP listener（复用注册/鉴权/中继逻辑——AcceptTCP
			// 的 xfer.Listener 抽象与传输无关，QUIC listener 直接传入）。同步绑定
			// fail-fast，accept 循环在 goroutine 中运行。
			// QUIC 自带 TLS（ALPN sproxy-quic）：生产应显式配置
			// SPROXY_QUIC_CERT_FILE/KEY_FILE，客户端经 SPROXY_QUIC_CA_CERT 校验；
			// 未配置时 ext/quic 回落开发用自签证书。
			quicListen := cfg.Hub.Transports.QUIC.Listen
			if quicListen == "" {
				quicListen = server.DefaultHubQUICListen
			}
			quicTP := xfer.Get("quic")
			if quicTP == nil {
				return fmt.Errorf("quic 传输层未注册（装配引入 ext/quic 触发 init 注册）")
			}
			qln, qerr := quicTP.Listen(ctx, quicListen)
			if qerr != nil {
				return fmt.Errorf("hub QUIC 中继监听失败: %w", qerr)
			}
			defer qln.Close()
			go func() {
				if aerr := hubSrv.AcceptTCP(ctx, qln); aerr != nil && ctx.Err() == nil {
					logger.Error("Hub QUIC 中继 accept 退出", "addr", quicListen, "error", aerr)
				}
			}()
			logger.Info("Hub QUIC 中继已启用", "addr", quicListen)
		}
	}
	// 云端下载经 mesh 出口（cloud_download_exit_node 启用）：构造经出口拨号函数注入
	// （main 包装配——pkg/server 不 import pkg/client 避免包级环）。
	var cloudExitDial func(context.Context, string) (net.Conn, error)
	if cfg.CloudDownloadExitNode != "" {
		dial, derr := buildCloudExitDial(cfg)
		if derr != nil {
			logger.Warn("cloud_download_exit_node 装配失败，回落本地直连下载", "error", derr)
		} else {
			cloudExitDial = dial
		}
	}
	h := server.RegisterRoutes(ctx, server.RegisterRoutesOpts{
		Mux:                 mux,
		CfgPtr:              &cfgPtr,
		CloudExitDial:       cloudExitDial,
		Version:             Version,
		BuildAt:             BuildAt,
		Logger:              logger,
		RouteTable:          routeTable,
		HubPersist:          persist,
		HubRestoredMessages: restoredMsgs,
		Tracer:              tracer,
		CredentialRing:      credRing,
		CredentialStore:     credStore,
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
	// 隧道的 h.tunnelHandler（Handlers 的未导出字段）：xfer 隧道 handleStream 已解密请求体为明文，
	// h.tunnelHandler 是 POST /tunnel 的外层帧解密器（期望 ctx 带派生密钥 + 帧 body），直接使用会 401。
	// fail-closed：xfer 段启用但装配失败（无有效凭据 Ring / 无证书）→ 拒绝启动。
	if _, err := startXferListener(ctx, cfg, credRing, h.LocalHandler(), logger); err != nil {
		return err
	}
	// 跨节点只读面（Y 一期）：remote_read.enabled 时起 loopback listener（每连接建
	// mux + Tunnel，真握手/真加密/双向 pin）。未启用时返回 (nil, nil)，零开销零回归。
	//
	// 关闭顺序（I3 订正）：**不依赖本 defer 链的 LIFO**。正常信号停机走
	// handleSignalShutdown（下方 runSignalHandler），它先 cancel(ctx) → s.Shutdown →
	// **直接调用 h.Close()**（关卷根、清 volSet/globalRoot），此时 RunE 的 defer 链还
	// 没跑，本文件这个 defer 反而在它之后才执行——「h.Close 先注册→后执行，本 listener
	// 后注册→先关闭」在真实停机路径上不成立。真正保证「先停 accept、再关卷根」的是
	// listener 自身的 ctx 感知 accept 循环（pkg/server/remote_read_listener.go:
	// ctx 取消即 Close listener 解开阻塞的 Accept；cancel 后 accept 到的竞态连接也直接
	// 丢弃）——cancel 恒早于 h.Close()。本 defer 只覆盖 RunE 提前返回的路径（该路径下
	// LIFO 顺序恰好也是 rrLn 先于 h.Close 关闭，与 ctx 路径行为一致）。
	rrLn, rrErr := server.StartRemoteReadListener(ctx, cfg, h, logger)
	if rrErr != nil {
		return fmt.Errorf("remote_read 启动失败: %w", rrErr)
	}
	if rrLn != nil {
		defer func() { _ = rrLn.Close() }()
	}
	// 跨节点写面（Y 二期 P3-b2）：与只读面**独立开关/监听**；pin 只收「授写」指纹，
	// 只读对端连握手都建立不了。关闭路径与只读面完全一致（ctx 感知 accept）。
	rwLn, rwErr := server.StartRemoteWriteListener(ctx, cfg, h, logger)
	if rwErr != nil {
		return fmt.Errorf("remote_write 启动失败: %w", rwErr)
	}
	if rwLn != nil {
		defer func() { _ = rwLn.Close() }()
	}
	// 跨节点面**运行态**（W1）：listener 实际地址 + node 角色是否成功启动 ⇒ 供
	// `GET /api/mesh/status` 显示真实状态（配置了但没起成功时必须诚实显示，否则误导排障）。
	nodeRoleRunning := &atomic.Bool{}
	readFaceAddr, writeFaceAddr := "", ""
	if rrLn != nil {
		readFaceAddr = rrLn.Addr()
	}
	if rwLn != nil {
		writeFaceAddr = rwLn.Addr()
	}
	h.SetMeshRuntimeInfo(newMeshRuntimeInfoProvider(readFaceAddr, writeFaceAddr, nodeRoleRunning.Load))

	// B 侧 mesh node 角色（S5，默认关闭）：把本机的只读/写面宣告到 mesh 并允许出口拨号，
	// 使对端 A 无需依赖外部 sidecar（`sclient mesh node …`）即可经服务发现到达本机。
	// 用**监听器实际地址**（配置写 `:0` 时只有 listener 知道真实端口）；凭据取本机自用凭据
	// （与 A 侧 mesh 客户端同源）。未启用时本调用是 no-op（零回归）。
	if cfg.Mesh.Node.Enabled {
		readAddr, writeAddr := "", ""
		if rrLn != nil {
			readAddr = rrLn.Addr()
		}
		if rwLn != nil {
			writeAddr = rwLn.Addr()
		}
		var creds *meshHubCreds
		if ak, sk, skeyID, ok := h.SelfCredential(); ok {
			creds = &meshHubCreds{AK: ak, SK: sk, SkeyID: skeyID}
		}
		if startMeshNodeRoleWithCreds(ctx, cfg, readAddr, writeAddr, creds, logger) {
			nodeRoleRunning.Store(true)
		}
	}
	// 文件同步 SyncManager：配置了 sync（sync.max_concurrent 或 sync_remotes 非空）时装配。
	// 远程访问用 HTTP 直连远程 sproxy（sync_remotes URL + SproxySig 凭据）；mesh 通道为后续增强。
	if cfg.Sync.MaxConcurrent > 0 || len(cfg.SyncRemotes) > 0 {
		remotes := make([]syncmgr.RemoteConfig, 0, len(cfg.SyncRemotes))
		for _, r := range cfg.SyncRemotes {
			remotes = append(remotes, syncmgr.RemoteConfig{
				Name: r.Name, Kind: syncmgr.RemoteKind(r.Kind),
				// direct 组
				URL: r.URL, AccessKey: r.AccessKey, AccessKeySecret: r.AccessKeySecret,
				AccessKeyID: r.AccessKeyID,
				// mesh 组（写批次装配）
				Node: r.Node, Volume: r.Volume, PeerPins: r.PeerPins, Transport: r.Transport,
			})
		}
		// P4/P5：quota 以 nil 注入（NewManager 内部回退 noop），随后 SetQuotaResolver 注入
		// per-owner resolver（sync pull 按任务 owner 在 user 桶 Scope 上预留/对账，owner_quotas 生效）。
		// 逐文件写前 guard：由 syncexec.Executor 装配 h.SyncQuotaScope() 与 h.SyncScopeFor()——
		// pull 本地写侧每个文件按实际 rel 路由到 bucket_limits 子目录子 Scope 后 TryReserve(size)
		// 再落盘，配额不足该文件失败（不中止整体）；子目录配额对 sync pull 生效。
		exec := syncexec.NewExecutor(h.SyncTenantResolver(), logger.With("component", "sync_exec"))
		exec.SetTenantScopeResolver(h.SyncQuotaScope())
		exec.SetScopeResolver(h.SyncScopeFor())
		// merge3 冲突索引：<storage_root>/anonymous/meta/sync（与凭据/审计同层持久化）。
		conflictIdx, idxErr := syncmgr.NewConflictIndex(filepath.Join(cfg.StorageRoot, "anonymous", "meta", "sync"))
		if idxErr != nil {
			logger.Warn("冲突索引初始化失败（冲突登记降级为仅内存标记文件）", "error", idxErr)
		} else {
			exec.ConflictIndex = conflictIdx
			h.SetConflictIndex(conflictIdx)
		}
		// Y 二期 P3-d：mesh 载体（`kind=mesh` 的远端）。仅在配置了 mesh 远端时装配；任一前置
		// 缺失都不注入并告警（保持 fail-closed：mesh 远端报 ErrMeshTransportNotWired，不回落 direct）。
		setupMeshFSFactory(exec, cfg, h, logger)
		// P4/V3：baidupcs 载体（`kind=baidupcs` 的本机网盘卷，经 volumes[] type=baidupcs 装配）。
		// 1. 先注册 baidupcs 后端插件（RegisterBackend，可插拔）；
		// 2. 卷集合（h.Volumes()）由 assembleVolumes 装配——type=baidupcs 的卷已经 registry.NewBackend
		//    构造并持有在 Set.external；工厂查 Set.External(remote.Volume) 统一寻址。
		// 3. set.External 无 baidupcs 卷时工厂不注入并告警（kind=baidupcs 远端报 ErrBaidupcsNotWired，不回落 direct）。
		registerBaidupcsBackend()
		setupBaidupcsFSFactory(exec, h.Volumes(), logger.With("component", "baidupcs_sync"), h.SyncQuotaScope())
		// WebDAV 后端（V3 plugin，第二个真实外部后端）：RegisterBackend("webdav") 可插拔注册——
		// volumes[] type=webdav 的卷由 assembleVolumes 经 registry.NewBackend 构造持有在 Set.external；
		// kind=volume 远端查 Set.External(volume) 统一寻址（与 baidupcs 同构，见 volume/webdav/backend.go）。
		webdav.RegisterWebDAVBackend()
		// SFTP 后端（V3 plugin，第四个真实外部后端；pkg/volume/sftp）：
		// RegisterBackend("sftp") 可插拔注册——volumes[] type=sftp 的卷由 assembleVolumes
		// 经 registry.NewBackend 构造持有在 Set.external；kind=volume 远端查 Set.External(volume)
		// 统一寻址（与 baidupcs/webdav 同构）。健康探针（registry.HealthProbe）随装配生效：
		// GET /api/volumes 时 state=healthy/degraded/unknown 可观测（roadmap 3.3 P1）。
		sftp.RegisterSFTPBackend()
		// S3 后端（V3 plugin，第三个真实外部后端；pkg/volume/ext/s3 独立 module）：
		// RegisterBackend("s3") 可插拔注册——volumes[] type=s3 的卷由 assembleVolumes 经
		// registry.NewBackend 构造持有在 Set.external；kind=volume 远端查 Set.External(volume)
		// 统一寻址（与 baidupcs/webdav 同构）。
		s3ext.RegisterS3Backend()
		// federated 后端（V3 plugin，第五个真实外部后端；pkg/volume/federated）：
		// RegisterBackend("federated") 可插拔注册——volumes[] type=federated 的卷由
		// assembleVolumes 经 registry.NewBackend 构造持有在 Set.external；Extra 读
		// node/volume/path（远端 mesh 节点卷只读挂载，roadmap 3.3 P2）。dialer 经
		// hub 中继（newMeshHubClient + NewRelayDialer）——远端卷读走 mesh 加密链路。
		if hubC, err := newMeshHubClient(cfg, cfg.Mesh.AccessKey, cfg.Mesh.AccessKeySecret, cfg.Mesh.SkeyID); err == nil && hubC != nil {
			federated.RegisterBackend(remote.NewRelayDialer(hubC, remote.ServiceName))
		}
		// 用户卷重启恢复（U4）：扫描 <storage_root>/<owner>/meta/volume/ 恢复用户卷到 Set.external
		// （单卷失败跳过 + 告警），并注入 store + owner 归属校验（跨 owner 创建任务 404）。
		// 敏感 Extra 加密（审查 P2）：credential_store.encrypt=true 时用同一 master key 加密
		// 用户卷 Extra（bduss 等凭据）；未启用时 nil（明文兼容，权限 0600 兜底）。
		var uvMasterKey []byte
		if cfg.CredentialStore.Encrypt {
			if mk, mkErr := server.ResolveCredentialMasterKey(cfg); mkErr == nil {
				uvMasterKey = mk
			} else {
				logger.Warn("credential_store.encrypt=true 但解析 master key 失败，用户卷 Extra 保持明文（建议修复配置后重启）", "error", mkErr)
			}
		}
		uvStore := server.NewUserVolumeStore(cfg.StorageRoot, uvMasterKey)
		if rErr := restoreUserVolumes(h.Volumes(), uvStore, logger.With("component", "user_volumes")); rErr != nil {
			logger.Warn("用户卷恢复扫描失败（用户卷功能降级为不可用）", "error", rErr)
		}
		h.SetUserVolumeStore(uvStore)
		syncMgr := syncmgr.NewManager(h.SyncTenantResolver(), h.SyncTenantList(), nil, int(capacity.CategoryUserFiles),
			remotes, exec,
			logger.With("component", "sync"),
			&syncmgr.Config{
				MaxConcurrent:  cfg.Sync.MaxConcurrent,
				TaskTTL:        cfg.Sync.TaskTTL,
				MaxRetries:     cfg.Sync.MaxRetries,
				RetryDelay:     cfg.Sync.RetryDelay,
				RetryBackoff:   cfg.Sync.RetryBackoff,
				PerFileReserve: true,
			})
		syncMgr.SetQuotaResolver(h.SyncQuotaStore())
		// 同步失败告警挂点（roadmap P1 阈值告警）：任务转 failed → 告警引擎（nil = 未启用）。
		if h.AlertEngine() != nil {
			syncMgr.OnTaskFailed = func(taskID, detail string) {
				h.AlertEngine().OnSyncFailed(context.Background(), taskID, detail)
			}
		}
		// 用户卷 owner 归属校验（U4）：remote.volume 是用户卷名时，task.Owner 必须匹配卷.Owner
		// （跨 owner 404 防枚举）。闭包判定：
		//   1. 系统盘（config volumes[] type=baidupcs，Set.external 已有且 store 无该卷）→ 用户可用（true）；
		//   2. 用户卷（store 有且 Owner == owner）→ true；
		//   3. 其它（store 无该卷且非系统盘）→ false（未知卷，跨 owner 语义 404）。
		volSet := h.Volumes()
		syncMgr.SetUserVolumeOwner(func(owner, volumeName string) bool {
			// 1. 用户卷：store 有且 Owner == owner → 归属。
			v, gErr := uvStore.Get(owner, volumeName)
			if gErr == nil && v != nil && v.Owner == owner {
				return true
			}
			// 2. 系统盘：Set.External 有，且该卷名**不属于任何用户卷**（排除用户卷——
			//    Set.External 同时含系统盘与用户卷；动态 ScanRestore 取全局用户卷名，
			//    任务创建低频可接受；优化空间：API 创建/删除时更新快照）。
			isUserVol := false
			if allUVs, sErr := uvStore.ScanRestore(); sErr == nil {
				for _, uv := range allUVs {
					if uv.Name == volumeName {
						isUserVol = true
						break
					}
				}
			}
			if !isUserVol && volSet != nil && volSet.External(volumeName) != nil {
				return true
			}
			// 3. 其它（未知卷/跨 owner 用户卷）→ 拒绝（404 防枚举语义）。
			return false
		})
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
	fmt.Printf("storage root: %s\n", cfg.StorageRoot)

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
//   - 握手密钥 = server.HubXferKey(cfg, ring)（AD-3：凭据 Ring 首个存活条目 SK + mesh
//     派生，与客户端一致）；
//   - 服务端身份 = server.LoadXferIdentity(cfg)（AD-4：Ed25519，指纹供客户端 pin）；
//   - fail-closed：xfer 段启用但 Ring 无有效凭据 / 无证书 → 返回 error（拒绝启动）；
//   - 默认绑 loopback（127.0.0.1:<port>），远程可达须显式 listen；
//   - 连接并入数量上限（cfg.Hub.MaxConnections 信号量）；**不注册路由表**（xfer 是
//     文件 API 隧道面，非中继节点面，不参与节点注册/VIP/DHT——hub 的 TryHandleConn
//     走注册帧语义，与 xfer 隧道帧不兼容，故独立信号量控制并发）。
func startXferListener(ctx context.Context, cfg *server.Config, ring *accesskey.Ring, tunnelHandler http.Handler, logger *slog.Logger) ([]xferListenerInfo, error) {
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
	// fail-closed：xfer listener 需要凭据 Ring 首个有效条目派生隧道密钥（AD-3）。
	key, err := server.HubXferKey(cfg, ring)
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
				m := mux.NewWithOpts(conn, mux.RoleListener, server.MuxIdlePaddingOptions(cfg)...)
				tun := tunnel.NewTunnel(m, key, tunnel.WithIdentity(identity))
				// Serve 同步执行 ECDH 握手（listener 侧）+ accept 循环；ctx 取消时返回。
				// 契约：Tunnel.Serve「ctx 取消 → nil，真错误 → 非 nil」（见
				// pkg/tunnel/tunnel_mux.go）。判空守卫有意义：只在**真错误**且进程尚未
				// 进入关闭流程（ctx 仍存活）时告警——避免把优雅停机的握手中断/accept
				// 退出误报为异常。
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
		cfgProvider.BindPFlag(cfgStorageRoot, cmd.Flags().Lookup(flagStorageRoot))
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
	// 被动伪装层（roadmap §5.3 P1）：TLS 参数对齐注入主 HTTP listener。
	// 生效状态启动日志输出（可观测铁律：禁静默降级——配了没生效必须能发现）。
	if maskingChanged, maskErr := server.ApplyTLSMasking(tlsCfg, &cfg.TLS); maskErr != nil {
		return fmt.Errorf("应用伪装层 TLS 参数失败: %w", maskErr)
	} else if maskingChanged {
		slog.Info("TLS 伪装层已启用（形态对齐）", "cipher_order", cfg.TLS.CipherOrder, "alpn", cfg.TLS.ALPN)
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
	// storage_root / owner_quotas 是多租户布局装配期消费的硬配置（OpenRoot + 配额 Scope 在建时
	// 固定），SIGHUP 后不重建 → 需重启进程。viper 键 storage_root/owner_quotas 由
	// LoadFromProvider 的 yaml/mapstructure 标签自动解码；环境变量 SPROXY_STORAGE_ROOT /
	// SPROXY_OWNER_QUOTAS 由 sproxycfg.New 的 AutomaticEnv 自动绑定。
	if oldCfg.StorageRoot != newCfg.StorageRoot {
		slog.Warn("storage_root 修改在 SIGHUP 后不会生效（存储根不重建），需要重启进程", "old", oldCfg.StorageRoot, "new", newCfg.StorageRoot)
	}
	if !maps.Equal(oldCfg.OwnerQuotas, newCfg.OwnerQuotas) {
		slog.Warn("owner_quotas 修改在 SIGHUP 后不会生效（配额 Scope 不重建），需要重启进程")
	}
	if !maps.Equal(oldCfg.BucketLimits, newCfg.BucketLimits) {
		slog.Warn("bucket_limits 修改在 SIGHUP 后不会生效（配额 Scope 不重建），需要重启进程")
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
