// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile string
	cfgPtr  atomic.Pointer[server.Config]
)

var rootCmd = &cobra.Command{
	Use:   "sproxy",
	Short: "轻量文件上传/下载/删除服务 + 加密隧道",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// 初始化 viper
		v := viper.GetViper()
		v.SetConfigFile(cfgFile)
		v.SetConfigType("yaml")
		v.SetEnvPrefix("SPROXY")
		v.AutomaticEnv()

		// 忽略配置文件不存在的错误
		if err := v.ReadInConfig(); err != nil {
			var cfnfe viper.ConfigFileNotFoundError
			if !errors.As(err, &cfnfe) && !os.IsNotExist(err) {
				return fmt.Errorf("读取配置文件失败: %w", err)
			}
		}

		// 绑定 flag 到 viper key
		_ = v.BindPFlag("addr", cmd.Flags().Lookup("addr"))
		_ = v.BindPFlag("uploads_dir", cmd.Flags().Lookup("uploads-dir"))
		_ = v.BindPFlag("tunnel_key", cmd.Flags().Lookup("tunnel-key"))
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

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "sproxy.yaml", "配置文件路径")

	rootCmd.Flags().String("addr", ":18083", "监听地址")
	rootCmd.Flags().String("uploads-dir", "./uploads", "上传目录")
	rootCmd.Flags().String("tunnel-key", "", "隧道密钥 (64 hex chars)")
	rootCmd.Flags().Bool("version", false, "打印版本与构建信息后退出")
}

func runServer(cmd *cobra.Command, args []string) error {
	// --version 处理
	if showVer, _ := cmd.Flags().GetBool("version"); showVer {
		fmt.Printf("Version: %s\n", Version)
		fmt.Printf("BuildAt: %s\n", BuildAt)
		return nil
	}

	// 用 viper 解码配置
	v := viper.GetViper()
	cfg, err := server.LoadFromViper(v)
	if err != nil {
		return fmt.Errorf("配置解析失败: %w", err)
	}
	cfgPtr.Store(cfg)

	// 启动日志
	logger := initLogger(cfg)
	slog.Info("config loaded", "path", cfgFile, "log_level", levelString(cfg.LogLevel), "log_format", formatString(cfg.LogFormat))

	// 隧道密钥处理
	tunnelKey, err := resolveTunnelKey(cfg)
	if err != nil {
		return fmt.Errorf("隧道密钥处理失败: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.NewServeMux()
	h := server.RegisterRoutes(ctx, mux, &cfgPtr, Version, BuildAt, tunnelKey, logger)
	defer func() {
		if err := h.Close(); err != nil {
			slog.Warn("handlers close error", "error", err.Error())
		}
	}()

	protocol := "http"
	if cfg.TLS.Enabled {
		protocol = "https"
	}
	fmt.Printf("downserver start at: %s://localhost%s\n", protocol, cfg.Addr)
	fmt.Printf("uploads dir: %s\n", cfg.UploadsDir)

	maxHeaderBytes := cfg.MaxHeaderBytes
	if maxHeaderBytes <= 0 {
		maxHeaderBytes = 1 << 20
	}

	s := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.ServerTimeouts.ReadHeader,
		ReadTimeout:       cfg.ServerTimeouts.Read,
		WriteTimeout:      cfg.ServerTimeouts.Write,
		IdleTimeout:       cfg.ServerTimeouts.Idle,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP)

	// shutdownDone 在 graceful shutdown 流程完成后被关闭，便于主 goroutine 等待清理动作真正执行完成。
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		for sig := range signalChan {
			if sig == syscall.SIGHUP {
				handleSighup(cfg, logger)
				continue
			}
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
			return
		}
	}()

	if cfg.TLS.Enabled {
		certFile := cfg.TLS.CertFile
		keyFile := cfg.TLS.KeyFile
		if certFile == "" {
			certFile = "certs/_wildcard.sproxy.local.pem"
		}
		if keyFile == "" {
			keyFile = "certs/_wildcard.sproxy.local-key.pem"
		}
		if cfg.TLS.AutoTLS {
			if _, err := os.Stat(certFile); os.IsNotExist(err) {
				slog.Info("自动生成自签证书", "cert", certFile, "key", keyFile)
				if err := server.GenerateSelfSignedCert(certFile, keyFile); err != nil {
					return fmt.Errorf("自动生成自签证书失败: %w", err)
				}
			}
		}
		slog.Info("TLS enabled", "cert", certFile, "key", keyFile)
		if err := s.ListenAndServeTLS(certFile, keyFile); err != nil {
			if err == http.ErrServerClosed {
				slog.Info("listen and serve closed", "error", err.Error())
			} else {
				return fmt.Errorf("listen and serve error: %w", err)
			}
		}
	} else {
		if err := s.ListenAndServe(); err != nil {
			if err == http.ErrServerClosed {
				slog.Info("listen and serve closed", "error", err.Error())
			} else {
				return fmt.Errorf("listen and serve error: %w", err)
			}
		}
	}

	// 走到这里说明 ListenAndServe 已被 Shutdown 关闭（ErrServerClosed 路径）。
	// 等 signal handler goroutine 完成清理，避免 defer h.Close() 抢在 Shutdown 真正释放连接前执行。
	<-shutdownDone
	slog.Info("downserver exit")
	return nil
}

// resolveTunnelKey 处理隧道密钥：已配置则校验，未配置则自动生成并回写配置文件。
func resolveTunnelKey(cfg *server.Config) ([]byte, error) {
	if cfg.TunnelKey != "" {
		if len(cfg.TunnelKey) != 64 {
			return nil, fmt.Errorf("invalid tunnel_key: must be 64 hex characters")
		}
		key, err := hex.DecodeString(cfg.TunnelKey)
		if err != nil {
			return nil, fmt.Errorf("invalid tunnel_key: %w", err)
		}
		return key, nil
	}

	// 自动生成
	newKey, err := tunnel.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate tunnel key error: %w", err)
	}
	cfg.TunnelKey = newKey
	fmt.Fprintf(os.Stderr, "Generated tunnel key: %s\n", cfg.TunnelKey)
	if err := server.SaveConfig(cfg, cfgFile); err != nil {
		return nil, fmt.Errorf("save config error: %w", err)
	}
	return hex.DecodeString(cfg.TunnelKey)
}

// handleSighup 处理 SIGHUP 信号：使用 viper 重新读取配置文件，
// 仅 log_level/log_format/auth_token 等软配置生效。
func handleSighup(oldCfg *server.Config, _ *slog.Logger) {
	v := viper.GetViper()
	if err := v.ReadInConfig(); err != nil {
		slog.Error("SIGHUP config reload failed", "error", err)
		return
	}
	newCfg, err := server.LoadFromViper(v)
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
	if oldCfg.TunnelKey != newCfg.TunnelKey {
		slog.Warn("tunnel_key 修改在 SIGHUP 后不会生效，需要重启进程")
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
