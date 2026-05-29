// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

var (
	Version = "dev"
	BuildAt = "unknown"
)

var (
	cfgPath = flag.String("config", "sproxy.yaml", "配置文件路径")
	showVer = flag.Bool("version", false, "打印版本与构建信息后退出")
	// 命令行标志定义
	uploadsDir    = flag.String("uploads-dir", "", "uploads file dir")
	listenAddr    = flag.String("addr", "", "listen address, e.g. :18083")
	tunnelKeyFlag = flag.String("tunnel-key", "", "tunnel key, must be 64 hex characters")
)

var (
	cfgPtr atomic.Pointer[server.Config]
)

func main() {
	flag.Parse()

	if *showVer {
		fmt.Printf("Version: %s\n", Version)
		fmt.Printf("BuildAt: %s\n", BuildAt)
		os.Exit(0)
	}

	cfg, err := server.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Printf("load config error: %s\n", err.Error())
		os.Exit(1)
	}
	if *listenAddr != "" {
		cfg.Addr = *listenAddr
	}
	if *uploadsDir != "" {
		cfg.UploadsDir = *uploadsDir
	}
	if *tunnelKeyFlag != "" {
		cfg.TunnelKey = *tunnelKeyFlag
	}
	*uploadsDir = cfg.UploadsDir
	cfgPtr.Store(cfg)

	initLogger(cfg)
	slog.Info("config loaded", "path", *cfgPath, "log_level", levelString(cfg.LogLevel), "log_format", formatString(cfg.LogFormat))

	var tunnelKey []byte
	if cfg.TunnelKey != "" {
		if len(cfg.TunnelKey) != 64 {
			fmt.Fprintf(os.Stderr, "invalid tunnel_key: must be 64 hex characters\n")
			os.Exit(1)
		}
		key, err := hex.DecodeString(cfg.TunnelKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid tunnel_key: %s\n", err.Error())
			os.Exit(1)
		}
		tunnelKey = key
	} else {
		// 自动生成 tunnel key 并回写配置文件
		newKey, err := tunnel.GenerateKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate tunnel key error: %s\n", err.Error())
			os.Exit(1)
		}
		cfg.TunnelKey = newKey
		fmt.Printf("Generated tunnel key: %s\n", cfg.TunnelKey)
		if err := server.SaveConfig(cfg, *cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "save config error: %s\n", err.Error())
			os.Exit(1)
		}
		tunnelKey, err = hex.DecodeString(cfg.TunnelKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "decode tunnel key error: %s\n", err.Error())
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.NewServeMux()
	server.RegisterRoutes(ctx, mux, &cfgPtr, Version, BuildAt, tunnelKey)

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

	go func() {
		for sig := range signalChan {
			if sig == syscall.SIGHUP {
				newCfg, err := server.LoadConfig(*cfgPath)
				if err != nil {
					slog.Error("config reload failed", "error", err)
					continue
				}
				if *listenAddr != "" {
					newCfg.Addr = *listenAddr
				}
				if *uploadsDir != "" {
					newCfg.UploadsDir = *uploadsDir
				}
				// 仅 log_level/log_format/auth_token 等"软配置"会热生效；
				// addr/uploads_dir/tunnel_key/rate_limit 等需要重启进程
				oldCfg := cfgPtr.Load()
				if oldCfg != nil {
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
				}
				initLogger(newCfg)
				slog.Info("config reloaded via SIGHUP", "path", *cfgPath, "log_level", levelString(newCfg.LogLevel))
				cfgPtr.Store(newCfg)
				continue
			}
			cancel()
			if err := s.Shutdown(context.Background()); err != nil {
				slog.Error("shutdown error", "error", err.Error())
				os.Exit(1)
			}
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
		slog.Info("TLS enabled", "cert", certFile, "key", keyFile)
		if err := s.ListenAndServeTLS(certFile, keyFile); err != nil {
			if err == http.ErrServerClosed {
				slog.Info("listen and serve closed", "error", err.Error())
			} else {
				slog.Error("listen and serve error", "error", err.Error())
				os.Exit(1)
			}
		}
	} else {
		if err := s.ListenAndServe(); err != nil {
			if err == http.ErrServerClosed {
				slog.Info("listen and serve closed", "error", err.Error())
			} else {
				slog.Error("listen and serve error", "error", err.Error())
				os.Exit(1)
			}
		}
	}

	slog.Info("downserver exit")
}

func initLogger(cfg *server.Config) {
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
	slog.SetDefault(slog.New(h))
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
