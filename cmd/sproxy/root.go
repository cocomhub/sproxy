// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/cmd/sproxy/internal/sproxycfg"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/files"
	_ "github.com/cocomhub/sproxy/pkg/integrity/ext/video" // 注册视频语义校验器（ffprobe）
	"github.com/cocomhub/sproxy/pkg/leader"
	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/state"
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
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/grpc" // 注册 gRPC 传输层（hub.transports.grpc）
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic" // 注册 QUIC 传输层（hub.transports.quic）
	quic "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic"
	wsxfer "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
	"github.com/cocomhub/sproxy/pkg/volume/ext/cluster"
	s3ext "github.com/cocomhub/sproxy/pkg/volume/ext/s3"
	"github.com/cocomhub/sproxy/pkg/volume/federated"
	"github.com/cocomhub/sproxy/pkg/volume/ftp"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
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

	// restartListener 是启动路径绑定成功的 HTTP listener（供 USR2 优雅重启经
	// ExtraFiles 继承给子进程；启动未完成/非启动路径为 nil）。
	restartListener atomic.Pointer[net.Listener]
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
	rootCmd.AddCommand(newCmdPikpak(cli.SystemIOStreams()))
}

func runServer(cmd *cobra.Command, args []string) error {
	if showVer, _ := cmd.Flags().GetBool(flagVersion); showVer {
		fmt.Printf("Version: %s\n", Version)
		fmt.Printf("BuildAt: %s\n", BuildAt)
		return nil
	}
	return runServerMain(cmd, args)
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
	if err := prepareXferTLS(cfg); err != nil {
		return nil, err
	}

	return startXferListeners(ctx, xferListenerCtx{cfg: cfg, key: key, identity: identity, tunnelHandler: tunnelHandler, logger: logger}, xferTLS, xferTCP)
}

// prepareXferTLS 装配 xfer TLS 默认配置（任一启用段需要 TLS 时）。
func prepareXferTLS(cfg *server.Config) error {
	if cfg.Hub.Transports.XferTLS.Enabled || cfg.Hub.Transports.XferTCP.TLSEnabled {
		tlsCfg, tErr := server.BuildXferTLSConfig(cfg)
		if tErr != nil {
			return fmt.Errorf("start xfer listener: %w", tErr)
		}
		builtin.SetDefaultTLSConfig(tlsCfg)
	}
	return nil
}

// xferListenerCtx 是 xfer listener 装配与服务循环共享的会话上下文
// （config/隧道密钥/服务端身份/隧道 handler/日志），收敛各段间的重复传递
// （避免 go:S107 参数爆炸）。
type xferListenerCtx struct {
	cfg           *server.Config
	key           []byte
	identity      *tunnel.Identity
	tunnelHandler http.Handler
	logger        *slog.Logger
}

// startXferListeners 逐个启动已启用的 xfer 段（xfer_tls 恒 TLS；xfer_tcp 段
// tls_enabled=true 升级为 TLS）。
func startXferListeners(ctx context.Context, xc xferListenerCtx, xferTLS, xferTCP server.XferTransportConfig) ([]xferListenerInfo, error) {
	var infos []xferListenerInfo
	if xferTLS.Enabled {
		// xfer_tls 段恒 TLS（段名即约定），不消费 TLSEnabled 字段。
		info, sErr := startOneXferListener(ctx, xc, "xfer_tls", xferTLS, true)
		if sErr != nil {
			return nil, sErr
		}
		infos = append(infos, info)
	}
	if xferTCP.Enabled {
		// xfer_tcp 段默认明文（显式 option），tls_enabled=true 升级为 TLS。
		info, sErr := startOneXferListener(ctx, xc, "xfer_tcp", xferTCP, xferTCP.TLSEnabled)
		if sErr != nil {
			return nil, sErr
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// startOneXferListener 启动单个 xfer accept 循环（同步绑定，绑定失败 fail-fast）。
// tlsEnabled 显式指定传输方式：xfer_tls 段恒传 true；xfer_tcp 段传 tc.TLSEnabled。
func startOneXferListener(ctx context.Context, xc xferListenerCtx, name string, tc server.XferTransportConfig, tlsEnabled bool) (xferListenerInfo, error) {
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

	serveXferAcceptLoop(ctx, ln, xc, name)

	xc.logger.Info("xfer listener 已启用", "name", name, "transport", transportName, "addr", addr)
	return xferListenerInfo{Name: name, Addr: addr, TLS: tlsEnabled, Fingerprint: xc.identity.Fingerprint()}, nil
}

// serveXferAcceptLoop 启动单个 xfer listener 的 accept 循环（goroutine 内运行）。
// 连接数上限走独立信号量（复用 hub.max_connections 语义）：xfer 是隧道帧，不走 hub
// 注册帧语义的 TryHandleConn（那会误读注册帧破坏隧道握手），超限立即关闭新连接
// （防未认证/慢连接拖垮进程，C-1 DoS 收敛）。
func serveXferAcceptLoop(ctx context.Context, ln xfer.Listener, xc xferListenerCtx, name string) {
	maxConns := xc.cfg.Hub.MaxConnections
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
				xc.logger.Error("xfer listener accept 退出", "name", name, "error", aErr)
				return
			}
			select {
			case sem <- struct{}{}:
			default:
				xc.logger.Warn("xfer 连接数达到上限，拒绝新连接", "name", name, "max", maxConns)
				_ = conn.Close()
				continue
			}
			go serveXferConn(ctx, conn, xc, name, sem)
		}
	}()
}

// serveXferConn 处理一条已接受的 xfer 隧道连接：mux + Tunnel.Serve（ECDH 握手 +
// accept 循环）。ctx 取消时返回（优雅停机）；连接数信号量由调用方释放。
// 契约：Tunnel.Serve「ctx 取消 → nil，真错误 → 非 nil」——只在**真错误**且进程尚未
// 进入关闭流程（ctx 仍存活）时告警，避免把优雅停机的握手中断/accept 退出误报为异常。
func serveXferConn(ctx context.Context, conn xfer.Conn, xc xferListenerCtx, name string, sem chan struct{}) {
	defer func() { <-sem }()
	m := mux.NewWithOpts(conn, mux.RoleListener, server.MuxIdlePaddingOptions(xc.cfg)...)
	tun := tunnel.NewTunnel(m, xc.key, tunnel.WithIdentity(xc.identity))
	if sErr := tun.Serve(ctx, xc.tunnelHandler); sErr != nil && ctx.Err() == nil {
		xc.logger.Warn("xfer 隧道 Serve 退出", "name", name, "error", sErr)
	}
	_ = m.Close()
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

	// 优雅重启继承（Unix）：SPROXY_INHERIT_FD 存在时从 fd 重建，跳过 net.Listen
	// （避免 EADDRINUSE）；否则普通 net.Listen（零回归）。
	ln, err := inheritListener(cfg.Addr)
	if err != nil {
		return fmt.Errorf(errFmtListenServe, err)
	}
	storeRestartListener(ln)
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
	// 优雅重启继承（Unix）：SPROXY_INHERIT_FD 存在时从 fd 重建，跳过 net.Listen
	// （避免 EADDRINUSE）；否则普通 net.Listen（零回归）。
	ln, err := inheritListener(s.Addr)
	if err != nil {
		return fmt.Errorf(errFmtListenServe, err)
	}
	storeRestartListener(ln)
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

// storeRestartListener 记录启动路径绑定的 HTTP listener（USR2 优雅重启继承用）。
// 在 net.Listen 成功、Serve 前调用；旁路监听（hub TCP/QUIC/gRPC/xfer）不参与继承。
func storeRestartListener(ln net.Listener) {
	restartListener.Store(&ln)
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

// signalCtx 是信号处理循环的会话上下文（取消函数 + HTTP server + handlers + 日志
// + 配置），收敛 runSignalLoop 的入参（避免 go:S107 参数爆炸）。
type signalCtx struct {
	cancel context.CancelFunc
	s      *http.Server
	h      *server.Handlers
	logger *slog.Logger
	cfg    *server.Config
}

// runSignalHandler 启动信号处理 goroutine，返回 stopSigCh（关闭后通知 goroutine 退出）和 shutdownDone（清理完成后关闭）。
func runSignalHandler(cancel context.CancelFunc, s *http.Server, h *server.Handlers, logger *slog.Logger, cfg *server.Config) (chan struct{}, chan struct{}) {
	signalChan := make(chan os.Signal, 1)
	if testSignalCh != nil {
		signalChan = testSignalCh
	}
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP)
	registerRestartSignal(signalChan)

	stopSigCh := make(chan struct{})
	shutdownDone := make(chan struct{})
	go runSignalLoop(signalChan, stopSigCh, shutdownDone, signalCtx{cancel: cancel, s: s, h: h, logger: logger, cfg: cfg})
	return stopSigCh, shutdownDone
}

// runSignalLoop 处理信号扇循环：SIGHUP 热加载；USR2/重启信号走重启；其余走优雅关闭。
func runSignalLoop(signalChan chan os.Signal, stopSigCh, shutdownDone chan struct{}, sc signalCtx) {
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
				handleSighup(sc.cfg, sc.h)
				continue
			}
			if isRestartSignal(sig) {
				handleSignalRestart(sc.cancel, sc.s, sc.h, sc.logger, sc.cfg)
				return
			}
			handleSignalShutdown(sc.cancel, sc.s, sc.h)
			return
		}
	}
}

// handleSignalShutdown 执行优雅关闭：取消 context、关闭 HTTP 服务器和 handlers。
func handleSignalShutdown(cancel context.CancelFunc, s *http.Server, h *server.Handlers) {
	cancel()
	currentCfg := cfgPtr.Load()
	shutdownTimeout := 30 * time.Second
	if currentCfg != nil && currentCfg.ServerTimeouts.Shutdown > 0 {
		shutdownTimeout = currentCfg.ServerTimeouts.Shutdown
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
// 软配置生效：log_level/log_format 与 notify.alerts 规则（AlertEngine.ReloadRules
// 原子换规则，保留既有 firing 状态；阈值类规则下个轮询 tick 生效，事件类
// 下个事件即生效）；tunnel_key 已废除。渠道实例（notify 段）不重建，
// alerts.enabled 翻转需重启进程（装配期决策）。
func handleSighup(oldCfg *server.Config, h *server.Handlers) {
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
	if !reflect.DeepEqual(oldCfg.RateLimit, newCfg.RateLimit) {
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

	// alerts.enabled 翻转（true↔false）是装配期决策（渠道 AdoptNotifyChannels 在
	// 装配时完成，SIGHUP 不重建装配）→ 仅警告；规则集仍按新 Rules 加载（规则集
	// 独立于 enabled 位，行为一致）。
	if oldCfg.Alerts.Enabled != newCfg.Alerts.Enabled {
		slog.Warn("alerts.enabled 修改在 SIGHUP 后不会生效（渠道装配期决策），需要重启进程",
			"old", oldCfg.Alerts.Enabled, "new", newCfg.Alerts.Enabled)
	}
	// 告警规则热加载（roadmap 11.1-②）：AlertEngine 存在（alerts 已装配）时
	// 原子换规则；nil（alerts 未启用）跳过，零影响。
	if h != nil {
		if eng := h.AlertEngine(); eng != nil {
			eng.ReloadRules(newCfg.Alerts.Rules)
			slog.Info("alerts 规则已热加载", "rules", len(newCfg.Alerts.Rules))
		}
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

// xferMetricsProvider 是传输层扩展指标提供者（go.work 联动 ext/ws + ext/quic）。
type xferMetricsProvider struct{}

// XferMetrics 返回 WS/QUIC 连接级统计快照（/metrics 聚合）。
func (xferMetricsProvider) XferMetrics() server.XferMetrics {
	return server.XferMetrics{
		WS:   wsMetricsOf(wsxfer.Metrics()),
		QUIC: quicMetricsOf(quic.Metrics()),
	}
}

// wsMetricsOf 把 ws.WSMetrics 映射为 server.XferConnMetrics（同构字段）。
func wsMetricsOf(m wsxfer.WSMetrics) server.XferConnMetrics {
	return server.XferConnMetrics{
		ConnsOpened:  m.ConnsOpened,
		ConnsClosed:  m.ConnsClosed,
		MessagesSent: m.MessagesSent,
		MessagesRecv: m.MessagesRecv,
		BytesSent:    m.BytesSent,
		BytesRecv:    m.BytesRecv,
	}
}

// quicMetricsOf 把 quic.QUICMetrics 映射为 server.XferConnMetrics（同构字段）。
func quicMetricsOf(m quic.QUICMetrics) server.XferConnMetrics {
	return server.XferConnMetrics{
		ConnsOpened:  m.ConnsOpened,
		ConnsClosed:  m.ConnsClosed,
		MessagesSent: m.MessagesSent,
		MessagesRecv: m.MessagesRecv,
		BytesSent:    m.BytesSent,
		BytesRecv:    m.BytesRecv,
	}
}

// runServerRuntime 汇总服务端装配期的运行时状态。cleanups 按「注册顺序」收集，
// runTeardown 逆序执行（对应原 runServer 内按出现顺序注册、LIFO 执行的 defer 链，
// 保证析构顺序与重构前一致）。
type runServerRuntime struct {
	//nolint:containedctx // S8242 已评估：长期驻留装配/测试底座生命周期 ctx，非请求作用域——与下行 NOSONAR 双抑制
	ctx          context.Context // NOSONAR: S8242 — 长期驻留结构体持有 ctx（装配/测试底座生命周期），非请求作用域
	cfg          *server.Config
	logger       *slog.Logger
	mux          *http.ServeMux
	credRing     *accesskey.Ring
	credStore    accesskey.CredentialStorer
	tracer       telemetry.Tracer
	routeTable   *hub.MeshRouteTable
	persist      *hub.Persister
	restoredMsgs []hub.MessageSnap
	restoredSnap *hub.Snapshot
	hubDHT       hub.DHT
	fedClient    *hub.FederationClient
	cloudDial    func(ctx context.Context, addr string) (net.Conn, error)
	writeGuard   *leader.WriteGuard
	h            *server.Handlers
	cleanups     []func()
}

// addCleanup 按注册顺序追加一条析构闭包（nil 跳过）。
func (rt *runServerRuntime) addCleanup(fn func()) {
	if fn != nil {
		rt.cleanups = append(rt.cleanups, fn)
	}
}

// runTeardown 逆序执行全部已注册的析构闭包（模拟 defer LIFO）。
func (rt *runServerRuntime) runTeardown() {
	for _, v := range slices.Backward(rt.cleanups) {
		v()
	}
}

// runServerMain 完成服务端装配并启动 HTTP 服务的调度入口（runServer 的委托目标）。
func runServerMain(cmd *cobra.Command, args []string) error {
	cfg, err := buildServerConfig(cmd)
	if err != nil {
		return err
	}
	cfgPtr.Store(cfg)
	logger := initLogger(cfg)
	slog.Info("config loaded", "path", cfgFile, "log_level", levelString(cfg.LogLevel), "log_format", formatString(cfg.LogFormat))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &runServerRuntime{ctx: ctx, cfg: cfg, logger: logger, mux: http.NewServeMux()}
	defer rt.runTeardown()
	if err := rt.assemble(); err != nil {
		return err
	}
	return rt.boot(cancel)
}

// assemble 依次完成装配段：协议盐 → 凭据 Ring → telemetry → hub → 云端出口 →
// 集群写面 → handlers/mux。各段把析构闭包按「注册顺序」收集进 rt.cleanups。
func (rt *runServerRuntime) assemble() error {
	if err := rt.setupProtocolSalt(); err != nil {
		return err
	}
	if err := rt.setupCredentials(); err != nil {
		return err
	}
	if err := rt.setupTelemetry(); err != nil {
		return err
	}
	if err := rt.setupHub(); err != nil {
		return err
	}
	if err := rt.setupCloudExit(); err != nil {
		return err
	}
	if err := rt.setupClusterWriteGuard(); err != nil {
		return err
	}
	return rt.setupServerCore()
}

// setupProtocolSalt 装配协议盐（防协议指纹识别）：配置 protocol_salt_key → 派生替换域分离盐。
func (rt *runServerRuntime) setupProtocolSalt() error {
	cfg := rt.cfg
	// 协议盐自定义（防协议指纹识别）：配置 protocol_salt_key → 派生替换域分离盐。
	// ⚠️ 与客户端/stealth 使用相同 key 才能握手（ECDH 会话密钥派生一致）。
	if cfg.ProtocolSaltKey != "" {
		if sk, derr := hex.DecodeString(cfg.ProtocolSaltKey); derr == nil && len(sk) == 32 {
			tunnel.SetProtocolSalts(tunnel.DeriveProtocolSalts(sk))
		} else {
			return fmt.Errorf("protocol_salt_key 应为 64 hex（32B 密钥）")
		}
	}
	return nil
}

// setupCredentials 装配 SproxySig 凭据 Ring（store 化）。U3 零凭据启动——
// store 为空不生成 anonymous 凭据，系统以零凭据等 register 公开端点接入首个 admin。
func (rt *runServerRuntime) setupCredentials() error {
	credRing, credStore, err := server.BootstrapServerCredentials(rt.cfg, slog.Default())
	if err != nil {
		return fmt.Errorf("装配凭据 Ring 失败: %w", err)
	}
	rt.credRing = credRing
	rt.credStore = credStore
	return nil
}

// setupTelemetry 装配 OTel tracer provider（enabled=false 时返回 nil tracer，零回归）。
func (rt *runServerRuntime) setupTelemetry() error {
	cfg, logger := rt.cfg, rt.logger
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
		rt.addCleanup(func() {
			if cerr := tp.Shutdown(context.Background()); cerr != nil {
				logger.Warn("telemetry provider 关停失败", "error", cerr)
			}
		})
		tracer = tp.Tracer("sproxy")
		logger.Info("telemetry 已启用", "sample_ratio", cfg.Telemetry.SampleRatio, "otlp_endpoint", cfg.Telemetry.OTLPEndpoint)
	}
	rt.tracer = tracer
	return nil
}

// setupHub 装配 Hub 中继：MeshRouteTable + HubServer + 各传输（ws/tcp/quic/grpc），
// 并把对应析构闭包按「注册顺序」收进 rt.cleanups。
func (rt *runServerRuntime) setupHub() error {
	cfg, logger := rt.cfg, rt.logger
	if !cfg.Hub.Enabled {
		return nil
	}
	rt.routeTable = hub.NewMeshRouteTable()
	logger.Info("Hub 中继模式已启用", "node_id", cfg.Hub.NodeID)

	if err := rt.setupHubPersistence(); err != nil {
		return err
	}
	// 节点注册准入：SproxySig AccessKey + HMAC proof（共享 token 已废除）。
	// hub 与 HTTP 面共用同一个凭据 Ring（credRing，单一事实源）。
	hubSrv := hub.NewHubServer(rt.routeTable, hub.NewAuthenticator(rt.credRing), logger.With("component", "hub"), cfg.Hub.MaxConnections)
	rt.setupHubAllocator(hubSrv)

	hubDHT, dhtCleanup, err := rt.setupHubDHT(hubSrv)
	if err != nil {
		return err
	}
	rt.hubDHT = hubDHT
	rt.addCleanup(dhtCleanup)

	fedClient, fedCleanup, err := rt.setupHubFederation()
	if err != nil {
		return err
	}
	rt.fedClient = fedClient
	rt.addCleanup(fedCleanup)

	if err = rt.setupHubWSTransport(hubSrv); err != nil {
		return err
	}
	tcpCleanup, err := rt.setupHubTCPTransport(hubSrv)
	if err != nil {
		return err
	}
	rt.addCleanup(tcpCleanup)
	quicCleanup, err := rt.setupHubQUICTransport(hubSrv)
	if err != nil {
		return err
	}
	rt.addCleanup(quicCleanup)
	grpcCleanup, err := rt.setupHubGRPCTransport(hubSrv)
	if err != nil {
		return err
	}
	rt.addCleanup(grpcCleanup)
	return nil
}

// setupHubPersistence 加载 hub 状态持久化快照（缺失/损坏按空状态启动）。
func (rt *runServerRuntime) setupHubPersistence() error {
	cfg, logger := rt.cfg, rt.logger
	if cfg.Hub.PersistFile == "" {
		return nil
	}
	persist := hub.NewPersister(cfg.Hub.PersistFile)
	if snap, err := persist.Load(); err != nil {
		return fmt.Errorf("读取 hub 持久化文件失败: %w", err)
	} else if snap != nil {
		hub.RestoreFromSnapshot(rt.routeTable, snap)
		rt.restoredMsgs = snap.Messages
		rt.restoredSnap = snap
		if len(snap.Nodes) > 0 || len(snap.Messages) > 0 {
			logger.Info("hub 状态已从持久化恢复", "file", cfg.Hub.PersistFile, "nodes", len(snap.Nodes), "messages", len(snap.Messages))
		}
	}
	rt.persist = persist
	return nil
}

// setupHubAllocator 装配虚拟 IP 分配器并按持久化快照重建分配表。
func (rt *runServerRuntime) setupHubAllocator(hubSrv *hub.HubServer) {
	cfg, logger, restoredSnap := rt.cfg, rt.logger, rt.restoredSnap
	// 虚拟 IP 分配：按 hub.virtual_subnet 配置的子网构建分配器（默认 CGNAT
	// 100.64.0.0/10，config.Validate 已保证 IPv4）。
	if prefix, perr := netip.ParsePrefix(cfg.Hub.VirtualSubnet); perr == nil {
		if prefix.Addr().Is4() {
			hubSrv.SetAllocator(hub.NewHubAllocator(prefix))
		} else {
			logger.Warn("hub.virtual_subnet 非 IPv4，使用默认子网", "virtual_subnet", cfg.Hub.VirtualSubnet)
		}
	} else {
		logger.Warn("hub.virtual_subnet 非法，使用默认子网", "virtual_subnet", cfg.Hub.VirtualSubnet, "error", perr)
	}
	if restoredSnap != nil {
		if perr := hub.PreloadAllocator(hubSrv.Allocator(), restoredSnap); perr != nil {
			logger.Error("虚拟 IP 分配表快照重建冲突（冲突条目不保留；对应节点重启后可能拿到新虚拟 IP）", "error", perr)
		}
	}
}

// setupHubDHT 装配 Kademlia DHT 发现表（hub.dht: kad）。未启用时返回 (nil,nil,nil)。
func (rt *runServerRuntime) setupHubDHT(hubSrv *hub.HubServer) (hub.DHT, func(), error) {
	cfg, logger := rt.cfg, rt.logger
	if cfg.Hub.DHT != "kad" {
		return nil, nil, nil
	}
	dhtNodeID := cfg.Hub.NodeID
	if dhtNodeID == "" {
		dhtNodeID = "hub-dht"
	}
	kadDHT := kad.NewDHT(dhtNodeID, nil, logger.With("component", "dht"))
	if cfg.Hub.DHTPersistFile != "" {
		if perr := kadDHT.EnablePersistence(cfg.Hub.DHTPersistFile); perr != nil {
			return nil, nil, fmt.Errorf("初始化 kad DHT 持久化失败: %w", perr)
		}
		logger.Info("kad DHT k-bucket 持久化已启用", "file", cfg.Hub.DHTPersistFile)
	}
	hub.RegisterDHT("kad", kadDHT, 10)
	hubDHT := hub.DHTRegistry.Active()
	if len(cfg.Hub.DHTSeeds) > 0 {
		logger.Warn("hub.dht_seeds 预留（多 hub DHT 组网未实现），暂不引导", "seeds", cfg.Hub.DHTSeeds)
	}
	hubSrv.SetDHT(hubDHT)
	logger.Info("Hub DHT 已启用", "impl", "kad", "node_id", dhtNodeID)
	return hubDHT, func() {
		if cerr := hubDHT.Close(); cerr != nil {
			logger.Error("kad DHT 关停 flush 失败", "err", cerr)
		}
	}, nil
}

// setupHubFederation 装配 hub 联邦（hub-to-hub peering）。未启用时返回 (nil,nil,nil)。
func (rt *runServerRuntime) setupHubFederation() (*hub.FederationClient, func(), error) {
	ctx, cfg, logger := rt.ctx, rt.cfg, rt.logger
	if !cfg.Hub.Federation.Enabled {
		return nil, nil, nil
	}
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
	fedClient, ferr := hub.NewFederationClientWithPersist(peers, cfg.Hub.Federation.Interval, cfg.Hub.Federation.Timeout, logger.With("component", "hub_federation"), cfg.Hub.Federation.PersistFile)
	if ferr != nil {
		return nil, nil, fmt.Errorf("初始化 hub 联邦客户端: %w", ferr)
	}
	fedClient.Start(ctx)
	logger.Info("Hub 联邦已启用", "peers", len(cfg.Hub.Federation.Peers), "interval", cfg.Hub.Federation.Interval, "persist_file", cfg.Hub.Federation.PersistFile)
	return fedClient, func() { fedClient.Close() }, nil
}

// setupHubWSTransport 挂载 WebSocket 升级端点并启动 accept 循环。未启用时 no-op。
func (rt *runServerRuntime) setupHubWSTransport(hubSrv *hub.HubServer) error {
	ctx, cfg, logger, mux := rt.ctx, rt.cfg, rt.logger, rt.mux
	if !cfg.Hub.Transports.WS.Enabled {
		return nil
	}
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
	hubNode := wsxfer.NewHandlerNode(wsxfer.WithUpgradeHeader(cfg.Hub.Transports.WS.UpgradeHeader))
	hubNode.AddToMux(mux, wsPath)
	go func() {
		for {
			conn, aerr := hubNode.Accept(ctx)
			if aerr != nil {
				return
			}
			if !hubSrv.TryHandleConn(ctx, conn) {
				logger.Warn("Hub 连接数达到上限，拒绝新连接", "max", cfg.Hub.MaxConnections)
				_ = conn.Close()
				continue
			}
		}
	}()
	return nil
}

// setupHubTCPTransport 启动裸 TCP 中继（同步绑定，绑定失败 fail-fast）。未启用时 no-op。
func (rt *runServerRuntime) setupHubTCPTransport(hubSrv *hub.HubServer) (func(), error) {
	ctx, cfg, logger := rt.ctx, rt.cfg, rt.logger
	if !cfg.Hub.Transports.TCP.Enabled {
		return nil, nil
	}
	tcpListen := cfg.Hub.Transports.TCP.Listen
	if tcpListen == "" {
		tcpListen = server.DefaultHubTCPListen
	}
	tcpLn, lerr := hubSrv.ListenTCP(ctx, tcpListen)
	if lerr != nil {
		return nil, fmt.Errorf("hub TCP 中继监听失败: %w", lerr)
	}
	go func() {
		if aerr := hubSrv.AcceptTCP(ctx, tcpLn); aerr != nil && ctx.Err() == nil {
			logger.Error("Hub TCP 中继 accept 退出", "addr", tcpListen, "error", aerr)
		}
	}()
	logger.Info("Hub TCP 中继已启用", "addr", tcpListen)
	return func() { _ = tcpLn.Close() }, nil
}

// setupHubQUICTransport 启动 QUIC 中继（裸 UDP listener）。未启用时 no-op。
func (rt *runServerRuntime) setupHubQUICTransport(hubSrv *hub.HubServer) (func(), error) {
	ctx, cfg, logger := rt.ctx, rt.cfg, rt.logger
	if !cfg.Hub.Transports.QUIC.Enabled {
		return nil, nil
	}
	quicListen := cfg.Hub.Transports.QUIC.Listen
	if quicListen == "" {
		quicListen = server.DefaultHubQUICListen
	}
	quicTP := xfer.Get("quic")
	if quicTP == nil {
		return nil, fmt.Errorf("quic 传输层未注册（装配引入 ext/quic 触发 init 注册）")
	}
	qln, qerr := quicTP.Listen(ctx, quicListen)
	if qerr != nil {
		return nil, fmt.Errorf("hub QUIC 中继监听失败: %w", qerr)
	}
	go func() {
		if aerr := hubSrv.AcceptTCP(ctx, qln); aerr != nil && ctx.Err() == nil {
			logger.Error("Hub QUIC 中继 accept 退出", "addr", quicListen, "error", aerr)
		}
	}()
	logger.Info("Hub QUIC 中继已启用", "addr", quicListen)
	return func() { _ = qln.Close() }, nil
}

// setupHubGRPCTransport 启动 gRPC 传输（HTTP/2 形态）。未启用时 no-op。
func (rt *runServerRuntime) setupHubGRPCTransport(hubSrv *hub.HubServer) (func(), error) {
	ctx, cfg, logger := rt.ctx, rt.cfg, rt.logger
	if !cfg.Hub.Transports.GRPC.Enabled {
		return nil, nil
	}
	grpcListen := cfg.Hub.Transports.GRPC.Listen
	if grpcListen == "" {
		grpcListen = server.DefaultHubGRPCListen
	}
	grpcTP := xfer.Get("grpc")
	if grpcTP == nil {
		return nil, fmt.Errorf("grpc 传输层未注册（装配引入 ext/grpc 触发 init 注册）")
	}
	gln, gerr := grpcTP.Listen(ctx, grpcListen)
	if gerr != nil {
		return nil, fmt.Errorf("hub gRPC 中继监听失败: %w", gerr)
	}
	go func() {
		if aerr := hubSrv.AcceptTCP(ctx, gln); aerr != nil && ctx.Err() == nil {
			logger.Error("Hub gRPC 中继 accept 退出", "addr", grpcListen, "error", aerr)
		}
	}()
	logger.Info("Hub gRPC 中继已启用", "addr", grpcListen)
	return func() { _ = gln.Close() }, nil
}

// setupCloudExit 装配云端下载经 mesh 出口的拨号函数（失败回落本地直连下载）。
func (rt *runServerRuntime) setupCloudExit() error {
	cfg, logger := rt.cfg, rt.logger
	if cfg.CloudDownloadExitNode != "" {
		dial, derr := buildCloudExitDial(cfg)
		if derr != nil {
			logger.Warn("cloud_download_exit_node 装配失败，回落本地直连下载", "error", derr)
		} else {
			rt.cloudDial = dial
		}
	}
	return nil
}

// setupClusterWriteGuard 装配集群写面门（LocalLeaderElector + WriteGuard）。
// 未启用（cluster.enabled=false）= nil 零回归（写面全放行）。
func (rt *runServerRuntime) setupClusterWriteGuard() error {
	ctx, cfg, logger := rt.ctx, rt.cfg, rt.logger
	if !cfg.Cluster.Enabled {
		return nil
	}
	stateDir := filepath.Join(filepath.Dir(cfg.StorageRoot), "cluster-state")
	elector := leader.NewLocalLeaderElector(stateDir)
	rt.writeGuard = leader.NewWriteGuard(elector, cfg.Cluster.NodeID, logger)
	if cfg.Cluster.Role == server.ClusterRoleReplica {
		// 只读副本：不竞争主，恒 follower（写面 503）。
		rt.writeGuard.SetLeader(false)
	} else {
		// master：TryAcquire 竞争主（本地 flock 单机恒成功）；失败（锁被占）
		// 启动警告但继续（装配期 fail-open——运行期 Authorize 按 isLeader 拒）。
		ctx2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
		ok, err := elector.TryAcquire(ctx2, cfg.Cluster.NodeID, 30*time.Second)
		cancel2()
		switch {
		case err != nil:
			logger.Warn("leader 获取失败，回落 follower（写面 503）", "error", err)
		case !ok:
			logger.Warn("leader 已被其他节点持有，本节点为 follower（写面 503）")
		default:
			rt.writeGuard.SetLeader(true)
		}
	}
	return nil
}

// setupServerCore 完成 RegisterRoutes 与全部 handler 级装配（StateStore/AI/集群注册表/
// xfer/远端读写面/mesh node 角色/文件同步），并把对应析构闭包按「注册顺序」收进 rt.cleanups。
func (rt *runServerRuntime) setupServerCore() error {
	ctx, cfg, logger := rt.ctx, rt.cfg, rt.logger
	// secret 加密卷后端类型注册：必须早于 RegisterRoutes → assembleVolumes（config 声明
	// `type: secretdata/secrets` 卷时 assembleVolumes 会对它们调用 registry.NewBackend，
	// 未注册即「未注册后端」启动 panic）。与 sync 开关解耦（Imp-1 装配门控修复：默认配置
	// sync 关闭时 secret 后端不再不可达）。
	registerSecretVolumeBackends()
	h := server.RegisterRoutes(ctx, server.RegisterRoutesOpts{
		Mux:                 rt.mux,
		CfgPtr:              &cfgPtr,
		CloudExitDial:       rt.cloudDial,
		WriteGuard:          rt.writeGuard,
		Version:             Version,
		BuildAt:             BuildAt,
		Logger:              logger,
		RouteTable:          rt.routeTable,
		HubPersist:          rt.persist,
		HubRestoredMessages: rt.restoredMsgs,
		Tracer:              rt.tracer,
		CredentialRing:      rt.credRing,
		CredentialStore:     rt.credStore,
		XferMetrics:         xferMetricsProvider{},
	})
	rt.h = h
	// secret 加密卷装配（无条件，与 sync 开关解耦——Imp-1）：确保默认 secrets 卷 +
	// Store 已装配卷集到 secretDataSet（供 secretdata 工厂懒解析密钥）。RegisterRoutes
	// 内部 assembleVolumes 已处理 config 声明卷；此处补默认卷与运行时密钥解析接线。
	// 失败即 boot fail（fail-closed，I1 修复）：setupSecretBackends 返回的 error **仅**来自
	// 配置声明的 secretdata/secrets 卷补装失败（operator 显式声明的加密封装不得静默消失——
	// 与本地卷 load failure 同层）；默认 secrets 卷失败已在函数内降级为 WARN。
	if err := setupSecretBackends(ctx, h.Volumes(), cfg.StorageRoot, logger); err != nil {
		return fmt.Errorf("secret 卷装配失败（配置声明的加密卷补装 boot fail）：%w", err)
	}
	// 云端下载下载器注册（cloud 独立于 sync：注册不依赖 SyncManager 装配）。
	// registerPikpakDownloader 内部用 sync.Once 保证只注册一次。
	registerPikpakDownloader(cfg)
	// 视频关键帧分块注册（video-keyframe 提供者，解析器按环境二选一：ffprobe 优先、
	// go-mp4 兜底；sync.Once 幂等）。注入 fMP4 统计 hook → /metrics 的
	// sproxy_keyframe_fmp4_total（供「是否切 mp4ff」真实场景决策）。
	registerKeyframeBackend()
	if m := h.Metrics(); m != nil {
		registerKeyframeMetricHooks(m.RecordKeyframeFragmented)
	}
	if err := rt.setupStateStore(h); err != nil {
		return err
	}
	rt.setupHubInject(h)
	rt.setupAIIntegrations(h)
	if err := rt.setupClusterNodeRegistry(h); err != nil {
		return err
	}
	// 先停 SyncManager（drain 同步任务）再关 Handlers：析构顺序见 runTeardown（LIFO）。
	rt.addCleanup(func() {
		if err := h.Close(); err != nil {
			slog.Warn(logHandlersCloseErr, "error", err.Error())
		}
	})
	if _, err := startXferListener(ctx, cfg, rt.credRing, h.LocalHandler(), logger); err != nil {
		return err
	}
	if err := rt.setupRemoteListeners(h); err != nil {
		return err
	}
	if err := rt.setupSync(h); err != nil {
		return err
	}
	return nil
}

// setupStateStore 装配 StateStore（cluster.enabled 或 state_store.type != local 时）。
func (rt *runServerRuntime) setupStateStore(h *server.Handlers) error {
	cfg, logger := rt.cfg, rt.logger
	if cfg.Cluster.Enabled || cfg.StateStore.Type != "" && cfg.StateStore.Type != "local" {
		stateDir := cfg.StateStore.Dir
		if stateDir == "" {
			stateDir = filepath.Join(cfg.StorageRoot, "state")
		}
		st, serr := state.NewStateStore(cfg.StateStore.Type, state.StateStoreConfig{
			Type:  cfg.StateStore.Type,
			Dir:   stateDir,
			Mongo: state.MongoConfig{URI: cfg.StateStore.Mongo.URI, Database: cfg.StateStore.Mongo.Database, Collection: cfg.StateStore.Mongo.Collection},
		}, logger)
		if serr != nil {
			return fmt.Errorf("装配 StateStore 失败（集群模式分享/索引必选 StateStore）: %w", serr)
		}
		h.SetStateStore(st)
		logger.Info("分享/索引后端切换 StateStore", "type", cfg.StateStore.Type, "dir", stateDir)
	}
	return nil
}

// setupHubInject 把 DHT/联邦候选注入 Handlers（/api/hub/nodes 合并候选节点）。
func (rt *runServerRuntime) setupHubInject(h *server.Handlers) {
	if rt.hubDHT != nil {
		h.SetDHT(rt.hubDHT) // /api/hub/nodes 合并 DHT 候选节点（发现源：路由表权威 + DHT 候选）
	}
	if rt.fedClient != nil {
		h.SetFederationClient(rt.fedClient) // /api/hub/nodes 合并联邦候选节点（发现源：+ 联邦候选）
	}
}

// setupAIIntegrations 装配 AI 向量索引 / 洞察 / 隐私 / 事件流水线（各自 enabled=false → 零回归）。
func (rt *runServerRuntime) setupAIIntegrations(h *server.Handlers) {
	cfg, logger := rt.cfg, rt.logger
	if cfg.Notify.AISearch.Enabled {
		vs := files.NewVectorStore(h.InsightCacheDir())
		h.SetVectorStore(vs)
	}
	if ai := server.NewAIInsightFromConfig(cfg.Notify.AIInsight, h.InsightCacheDir(), logger); ai != nil {
		// AI 配额（roadmap 11.9-⑦；ai.quota.enabled=false → nil 零回归）。
		ai.SetQuota(server.NewAIQuota(cfg.Notify.AIQuota, logger))
		h.SetAIInsight(ai)
	}
	if cfg.Notify.AIPrivacy.Enabled {
		if pr := server.NewAIPrivacy(true, h.PrivacyRoot(), logger); pr != nil {
			h.SetAIPrivacy(pr)
		}
	}
	if cfg.Notify.AIEvents.Enabled {
		consumer := server.NewAIEventConsumer(h.EventsBus(),
			func(owner, rel, op string) {
				h.RecordAudit(server.BackgroundContext(), server.AuditEvent{
					Action: "ai.events", ObjectType: "file", Object: owner + "/" + rel,
					Result: "enqueued", Detail: op,
				})
			},
			cfg.Notify.AIEvents, logger)
		consumer.Start()
		h.SetAIEventConsumer(consumer)
	}
}

// setupClusterNodeRegistry 装配集群节点注册表（cluster.enabled=false → nil 零回归）。
func (rt *runServerRuntime) setupClusterNodeRegistry(h *server.Handlers) error {
	cfg, logger := rt.cfg, rt.logger
	if !cfg.Cluster.Enabled {
		return nil
	}
	stateDir := filepath.Join(filepath.Dir(cfg.StorageRoot), "cluster-state")
	st := state.NewLocalStateStore(stateDir, logger)
	h.SetNodeRegistry(server.NewNodeRegistry(st, logger))
	server.NewEventIndexBridge(h.EventsBus(), h.FileService(), logger).Start()
	return nil
}

// setupRemoteListeners 启动跨节点只读/写面 listener，并把关闭闭包收进 rt.cleanups。
func (rt *runServerRuntime) setupRemoteListeners(h *server.Handlers) error {
	ctx, cfg, logger := rt.ctx, rt.cfg, rt.logger
	rrLn, rrErr := server.StartRemoteReadListener(ctx, cfg, h, logger)
	if rrErr != nil {
		return fmt.Errorf("remote_read 启动失败: %w", rrErr)
	}
	if rrLn != nil {
		rt.addCleanup(func() { _ = rrLn.Close() })
	}
	rwLn, rwErr := server.StartRemoteWriteListener(ctx, cfg, h, logger)
	if rwErr != nil {
		return fmt.Errorf("remote_write 启动失败: %w", rwErr)
	}
	if rwLn != nil {
		rt.addCleanup(func() { _ = rwLn.Close() })
	}
	rt.setupMeshRuntimeAndRole(rrLn, rwLn, h)
	return nil
}

// setupMeshRuntimeAndRole 填充跨节点面运行态（listener 实际地址 + node 角色）并装配 B 侧 mesh node 角色。
func (rt *runServerRuntime) setupMeshRuntimeAndRole(rrLn *server.RemoteReadListener, rwLn *server.RemoteWriteListener, h *server.Handlers) {
	ctx, cfg, logger := rt.ctx, rt.cfg, rt.logger
	nodeRoleRunning := &atomic.Bool{}
	readFaceAddr, writeFaceAddr := "", ""
	if rrLn != nil {
		readFaceAddr = rrLn.Addr()
	}
	if rwLn != nil {
		writeFaceAddr = rwLn.Addr()
	}
	h.SetMeshRuntimeInfo(newMeshRuntimeInfoProvider(readFaceAddr, writeFaceAddr, nodeRoleRunning.Load))

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
		if startMeshNodeRoleWithCreds(ctx, cfg, readAddr, writeAddr, creds, h.AlertEngine(), logger) {
			nodeRoleRunning.Store(true)
		}
	}
}

// setupSync 装配文件同步 SyncManager（sync.max_concurrent > 0 或 sync_remotes 非空时）。
func (rt *runServerRuntime) setupSync(h *server.Handlers) error {
	cfg, logger := rt.cfg, rt.logger
	if cfg.Sync.MaxConcurrent <= 0 && len(cfg.SyncRemotes) == 0 {
		return nil
	}
	remotes := buildSyncRemotes(cfg)
	exec := syncexec.NewExecutor(h.SyncTenantResolver(), logger.With("component", "sync_exec"))
	exec.SetTenantScopeResolver(h.SyncQuotaScope())
	exec.SetScopeResolver(h.SyncScopeFor())
	// 写保护（用户语义 #6，旁路闭环 2026-10-06）：同步本地写侧（pull/双向）直写默认卷 user
	// 桶，不经 files 域 guard——注入默认卷占用写保护（命中被封装卷占用子目录 → 该文件失败）。
	exec.SetWriteGuard(h.DefaultVolumeWriteGuard())
	rt.setupSyncConflictIndex(exec, h, logger)
	setupMeshFSFactory(exec, cfg, h, logger)
	rt.setupSyncVolumeBackends(exec, h, logger)
	uvStore := rt.setupSyncUserStore(h, logger)
	return rt.buildSyncManager(h, remotes, exec, uvStore, logger)
}

// buildSyncRemotes 把 cfg.SyncRemotes 转换为 syncmgr.RemoteConfig 列表。
func buildSyncRemotes(cfg *server.Config) []syncmgr.RemoteConfig {
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
	return remotes
}

// setupSyncConflictIndex 装配 merge3 冲突索引（失败降级为仅内存标记文件）。
func (rt *runServerRuntime) setupSyncConflictIndex(exec *syncexec.Executor, h *server.Handlers, logger *slog.Logger) {
	cfg := rt.cfg
	conflictIdx, idxErr := syncmgr.NewConflictIndex(filepath.Join(cfg.StorageRoot, "anonymous", "meta", "sync"))
	if idxErr != nil {
		logger.Warn("冲突索引初始化失败（冲突登记降级为仅内存标记文件）", "error", idxErr)
	} else {
		exec.ConflictIndex = conflictIdx
		h.SetConflictIndex(conflictIdx)
	}
}

// setupSyncVolumeBackends 装配 V3 插件后端（baidupcs/webdav/sftp/ftp/s3/federated）。
func (rt *runServerRuntime) setupSyncVolumeBackends(exec *syncexec.Executor, h *server.Handlers, logger *slog.Logger) {
	cfg := rt.cfg
	registerBaidupcsBackend()
	setupBaidupcsFSFactory(exec, h.Volumes(), logger.With("component", "baidupcs_sync"), h.SyncQuotaScope())
	webdav.RegisterWebDAVBackend()
	sftp.RegisterSFTPBackend()
	ftp.RegisterFTPBackend()
	s3ext.RegisterS3Backend()
	// linked（外部）类型基础建卷 schema（D2）：这些后端未实现 SchemaProvider，登记静态
	// **建卷表单** schema（url/凭据字段）使 Web UI「卷管理」建卷表单可提交（后端自带
	// fail-fast 校验 extra）。与类型注册同步，/api/backends 恒先见类型后有正常表单。
	registerLinkedBackendSchemas()
	// 注：secret 加密卷装配已从本函数移出——见 setupServerCore（registerSecretVolumeBackends
	// 早于 RegisterRoutes 注册后端类型 + RegisterRoutes 后无条件 setupSecretBackends 确保
	// 默认 secrets 卷），不再被 setupSync 早退门控（Imp-1 装配门控修复）。
	if hubC, err := newMeshHubClient(cfg, cfg.Mesh.AccessKey, cfg.Mesh.AccessKeySecret, cfg.Mesh.SkeyID); err == nil && hubC != nil {
		// 联邦卷回写（roadmap P2）：写面走独立服务名 volwrite（#494 写面会话）。
		federated.RegisterBackend(remote.NewRelayDialer(hubC, remote.ServiceName),
			remote.WithWriteDialer(remote.NewRelayDialer(hubC, remote.ServiceNameWrite)))
		// **集群出口（2026-10-05 用户裁定凭证下发）**：出口节点装配 type: egress 卷——
		// 访问持有节点真实卷（本端 Ed25519 身份 + 持有指纹 pin）。凭证签发/验签仅
		// 在持有节点本地（cluster.credentials + cluster.credential_sign_key），出口侧
		// 不持有签发 SK（评审 I1：此前传 64-hex 串致装配恒失败，且扩大 SK 泄露面）。
		if id, idErr := server.LoadXferIdentity(cfg); idErr == nil {
			cluster.RegisterBackend(remote.NewRelayDialer(hubC, remote.ServiceName), id)
		} else {
			// **评审 I8（静默不注册）**：身份加载失败时 egress 后端不注册，随后任何
			// `type: egress` 卷会在装配层以混乱错误失败——显式告警，避免运维把
			// 「无出口能力」当成「未配置出口卷」。
			slog.Error("集群出口装配失败：加载本端 xfer 身份失败（egress 卷后端未注册）",
				"error", idErr)
		}
	}
}

// setupSyncUserStore 装配用户卷 store 并恢复重启前的用户卷（单卷失败跳过 + 告警）。
func (rt *runServerRuntime) setupSyncUserStore(h *server.Handlers, logger *slog.Logger) *server.UserVolumeStore {
	cfg := rt.cfg
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
	return uvStore
}

// buildSyncManager 构造 SyncManager 并装配配额/告警/用户卷归属校验（析构闭包收进 rt.cleanups）。
func (rt *runServerRuntime) buildSyncManager(h *server.Handlers, remotes []syncmgr.RemoteConfig, exec *syncexec.Executor, uvStore *server.UserVolumeStore, logger *slog.Logger) error {
	cfg := rt.cfg
	syncMgr := syncmgr.NewManager(syncmgr.ManagerOptions{TenantRoot: h.SyncTenantResolver(), ListTenants: h.SyncTenantList(), Quota: nil, QuotaCat: int(capacity.CategoryUserFiles), Remotes: remotes, Executor: exec, Logger: logger.With("component", "sync"), Config: &syncmgr.Config{
		MaxConcurrent:  cfg.Sync.MaxConcurrent,
		TaskTTL:        cfg.Sync.TaskTTL,
		MaxRetries:     cfg.Sync.MaxRetries,
		RetryDelay:     cfg.Sync.RetryDelay,
		RetryBackoff:   cfg.Sync.RetryBackoff,
		PerFileReserve: true,
	}})
	syncMgr.SetQuotaResolver(h.SyncQuotaStore())
	// 同步失败告警挂点（roadmap P1 阈值告警）：任务转 failed → 告警引擎（nil = 未启用）。
	if h.AlertEngine() != nil {
		syncMgr.OnTaskFailed = func(taskID, detail string) {
			h.AlertEngine().OnSyncFailed(context.Background(), taskID, detail)
		}
	}
	// 用户卷 owner 归属校验（U4）：remote.volume 是用户卷名时，task.Owner 必须匹配卷.Owner。
	syncMgr.SetUserVolumeOwner(syncUserVolumeOwnerFunc(h.Volumes(), uvStore))
	h.SetSyncMgr(syncMgr)
	rt.addCleanup(syncMgr.Stop)
	return nil
}

// syncUserVolumeOwnerFunc 构造 SyncManager 的用户卷 owner 归属校验闭包。
func syncUserVolumeOwnerFunc(volSet *registry.Set, uvStore *server.UserVolumeStore) func(owner, volumeName string) bool {
	return func(owner, volumeName string) bool {
		// 1. 用户卷：store 有且 Owner == owner → 归属。
		v, gErr := uvStore.Get(owner, volumeName)
		if gErr == nil && v != nil && v.Owner == owner {
			return true
		}
		// 2. 系统盘：Set.External 有，且该卷名**不属于任何用户卷**。
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
	}
}

// displayBanner 打印启动横幅。
func displayBanner(cfg *server.Config) {
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
}

// setupMetricsServer 装配独立指标端口（cfg.MetricsPort > 0 时；否则返回 (nil, nil)）。
func (rt *runServerRuntime) setupMetricsServer() (*http.Server, func()) {
	h := rt.h
	if rt.cfg.MetricsPort <= 0 {
		return nil, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		h.MetricsHandler(w, r)
	})
	ms := &http.Server{
		Addr:              fmt.Sprintf(":%d", rt.cfg.MetricsPort),
		Handler:           h.MetricsAuth(http.Handler(mux)),
		ReadHeaderTimeout: rt.cfg.ServerTimeouts.ReadHeader,
		IdleTimeout:       rt.cfg.ServerTimeouts.Idle,
	}
	go func() {
		slog.Info("独立指标端口启动", "addr", ms.Addr)
		if err := ms.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("独立指标端口退出", "error", err)
		}
	}()
	return ms, func() { ms.Close() }
}

// boot 打印横幅、启动 HTTP 服务与信号处理，并阻塞至优雅停机完成。
func (rt *runServerRuntime) boot(cancel context.CancelFunc) error {
	cfg, logger, h := rt.cfg, rt.logger, rt.h
	displayBanner(cfg)
	srv := createHTTPServer(cfg, h.Handler())
	_, mCleanup := rt.setupMetricsServer()
	rt.addCleanup(mCleanup)
	stopSigCh, shutdownDone := runSignalHandler(cancel, srv, h, logger, cfg)
	rt.addCleanup(func() { close(stopSigCh) })

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
