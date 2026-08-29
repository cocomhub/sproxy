// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/cmd/sproxy/internal/sproxycfg"
	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
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
	var persist *hub.Persister         // hub 状态持久化器（仅 hub.enabled 且 persist_file 非空时创建）
	var restoredMsgs []hub.MessageSnap // 启动时从持久化恢复的信令收件箱（灌入 SignalBroker）
	// Hub 中继：先注册 xfer/ws 传输（开启 transports.ws 时），
	// 创建 MeshRouteTable + HubServer 收口，再注册 HTTP 路由。
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
				if len(snap.Nodes) > 0 || len(snap.Messages) > 0 {
					logger.Info("hub 状态已从持久化恢复", "file", cfg.Hub.PersistFile, "nodes", len(snap.Nodes), "messages", len(snap.Messages))
				}
			}
		}
		if cfg.Hub.Transports.WS.Enabled {
			// 节点注册准入：SproxySig AccessKey + HMAC proof（共享 token 已废除）。
			// hub 准入凭据来自顶层 access_keys 配置，转换后交给 hub.Authenticator。
			aks := make([]hub.AccessKey, 0, len(cfg.AccessKeys))
			for _, k := range cfg.AccessKeys {
				aks = append(aks, hub.AccessKey{Key: k.Key, Secret: k.Secret})
			}
			hubSrv := hub.NewHubServer(routeTable, hub.NewAuthenticator(aks), logger.With("component", "hub"), cfg.Hub.MaxConnections)
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
	defer func() {
		if err := h.Close(); err != nil {
			slog.Warn(logHandlersCloseErr, "error", err.Error())
		}
	}()

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
