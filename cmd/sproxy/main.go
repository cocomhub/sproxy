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
	"syscall"

	"github.com/cocomhub/sproxy/config"
	"github.com/cocomhub/sproxy/internal/handlers"
)

var (
	Version = "dev"
	BuildAt = "unknown"
)

var (
	cfgPath = flag.String("config", "config.yaml", "配置文件路径")
	showVer = flag.Bool("version", false, "打印版本与构建信息后退出")
	// 命令行标志定义
	uploadsDir = flag.String("uploads-dir", "./uploads", "uploads file dir")
	listenAddr = flag.String("addr", "", "listen address, e.g. :18080")
)

var (
	appCfg     *config.Config
	httpClient *http.Client
)

func main() {
	flag.Parse()

	if *showVer {
		fmt.Printf("Version: %s\n", Version)
		fmt.Printf("BuildAt: %s\n", BuildAt)
		os.Exit(0)
	}

	cfg, err := config.Load(*cfgPath)
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
	*uploadsDir = cfg.UploadsDir

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
	}

	appCfg = cfg
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
	}
	httpClient = &http.Client{
		Transport: tr,
		Timeout:   cfg.ClientTimeout,
	}

	mux := http.NewServeMux()
	handlers.RegisterRoutes(mux, cfg, httpClient, *uploadsDir, Version, BuildAt, tunnelKey)

	fmt.Printf("downserver start at: http://localhost%s\n", cfg.Addr)
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
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)

	go func() {
		<-signalChan

		if err := s.Shutdown(context.Background()); err != nil {
			slog.Error("shutdown error", "error", err.Error())
			os.Exit(1)
		}
	}()

	if err := s.ListenAndServe(); err != nil {
		if err == http.ErrServerClosed {
			slog.Info("listen and serve closed", "error", err.Error())
		} else {
			slog.Error("listen and serve error", "error", err.Error())
			os.Exit(1)
		}
	}

	slog.Info("downserver exit")
}

func initLogger(cfg *config.Config) {
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
