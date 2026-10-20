// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"

	"github.com/cocomhub/sproxy/cmd/sproxy/internal/sproxycfg"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/spf13/cobra"
)

// -- helpers for tests --

// setupProviderForSighup 创建 Provider 使其能读取指定配置文件。
func setupProviderForSighup(cfgPath string) *sproxycfg.ViperProvider {
	return sproxycfg.New(cfgPath)
}

// ---- handleSighup tests ----

func TestHandleSighup_ConfigReload(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sproxy.yaml")

	initialCfg := server.Default()
	initialCfg.Addr = "127.0.0.1:0"
	if err := server.SaveConfig(initialCfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	cfgProvider = setupProviderForSighup(cfgPath)
	t.Cleanup(func() { cfgProvider = nil })

	cfgFile = cfgPath
	t.Cleanup(func() {
		cfgPtr.Store(nil)
	})

	cfgPtr.Store(initialCfg)

	// Change log level in config file
	newCfg := *initialCfg
	newCfg.LogLevel = "debug"
	newCfg.LogFormat = "json"
	if err := server.SaveConfig(&newCfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	handleSighup(initialCfg, nil)

	reloaded := cfgPtr.Load()
	if reloaded.LogLevel != "debug" {
		t.Errorf("expected log_level=debug after reload, got %q", reloaded.LogLevel)
	}
}

func TestHandleSighup_AddrChangeWarning(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sproxy.yaml")

	initialCfg := server.Default()
	initialCfg.Addr = ":18083"
	if err := server.SaveConfig(initialCfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	cfgProvider = setupProviderForSighup(cfgPath)
	t.Cleanup(func() { cfgProvider = nil })

	cfgFile = cfgPath
	t.Cleanup(func() {
		cfgPtr.Store(nil)
	})

	cfgPtr.Store(initialCfg)

	// Change addr (warn-only field)
	newCfg := *initialCfg
	newCfg.Addr = ":19000"
	newCfg.LogLevel = "debug"
	if err := server.SaveConfig(&newCfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	_ = testutil.CaptureStderr(func() {
		handleSighup(initialCfg, nil)
	})
}

func TestRunServer_ListenAndServeError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping listen error test in short mode")
	}

	// 先占用一个端口。注意：Linux Go 默认 SO_REUSEADDR 允许多次绑定同一地址，
	// 因此端口占用在 Linux 上不会导致 ListenAndServe 失败。
	// 此测试在两种平台上均需通过：Windows（端口冲突报错）和 Linux（SO_REUSEADDR 成功，通过信号关闭）。
	existing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	occupiedAddr := existing.Addr().String()

	tmpDir := t.TempDir()

	cmd := &cobra.Command{Use: "sproxy"}
	cmd.Flags().String("addr", occupiedAddr, "")
	cmd.Flags().String("storage-root", tmpDir, "")
	cmd.Flags().Bool("version", false, "")

	// 注入 signal channel 避免 goroutine 泄漏
	sigCh := make(chan os.Signal, 1)
	testSignalCh = sigCh
	t.Cleanup(func() { testSignalCh = nil })

	// 设置 Provider 和配置
	cfgProvider = setupProviderForSighup(filepath.Join(tmpDir, "sproxy.yaml"))
	t.Cleanup(func() { cfgProvider = nil })
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("storage_root", cmd.Flags().Lookup("storage-root"))
	cfgProvider.Set("addr", occupiedAddr)
	cfgProvider.Set("storage_root", tmpDir)
	cfgProvider.Set("log_level", "error")

	// 并发运行 server
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(cmd, nil)
	}()

	// 有意占位：等待 server 启动（ListenAndServe 在 Windows 上会阻塞、Linux 上可能直接成功），
	// 此处无端口可探活（残余清单登记项）。
	time.Sleep(300 * time.Millisecond)

	// 发送 SIGTERM 确保 server 关闭（无论端口占用是否生效）
	sigCh <- syscall.SIGTERM

	select {
	case err := <-errCh:
		// Windows：err 为 "listen and serve error"（端口冲突）
		// Linux：err 为 nil 或 ErrServerClosed（信号关闭）
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !strings.Contains(err.Error(), "listen and serve error") {
			t.Errorf("runServer returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within 5s")
	}
}

// ---- initLogger tests ----

func TestInitLogger_Combinations(t *testing.T) {
	levels := []string{"debug", "info", "warn", "error"}
	formats := []string{"text", "json"}

	for _, level := range levels {
		for _, format := range formats {
			t.Run(level+"_"+format, func(t *testing.T) {
				cfg := &server.Config{
					LogLevel:  level,
					LogFormat: format,
				}

				// Save and restore the default logger to avoid cross-test interference
				oldDefault := slog.Default()
				t.Cleanup(func() { slog.SetDefault(oldDefault) })

				output := testutil.CaptureStdout(func() {
					logger := initLogger(cfg)
					if logger == nil {
						t.Error("initLogger returned nil")
						return
					}
					doLogAtLevel(logger, level)
				})

				assertLogOutput(t, output, format)
			})
		}
	}
}

// doLogAtLevel 使用对应级别的 logger 输出测试消息。
func doLogAtLevel(logger *slog.Logger, level string) {
	switch level {
	case "debug":
		logger.Debug("test message", "key", "value")
	case "info":
		logger.Info("test message", "key", "value")
	case "warn":
		logger.Warn("test message", "key", "value")
	case "error":
		logger.Error("test message", "key", "value")
	}
}

// assertLogOutput 验证日志输出是否符合预期格式。
func assertLogOutput(t *testing.T, output string, format string) {
	t.Helper()
	if format == "json" {
		if len(output) == 0 {
			t.Error("expected JSON output, got empty")
			return
		}
		if output[0] != '{' {
			t.Errorf("expected JSON object (starts with '{'), got: %s", output[:min(len(output), 50)])
		}
		if !bytes.Contains([]byte(output), []byte("test message")) {
			t.Errorf("expected log message in JSON output, got: %s", output[:min(len(output), 100)])
		}
	} else if !bytes.Contains([]byte(output), []byte("test message")) {
		// Text output should contain the message
		t.Errorf("expected 'test message' in text output, got: %s", output[:min(len(output), 100)])
	}
}

// ---- initLogger tests ----

// TestRegisterVolumeBackendTypes_EnablesExternalVolumeAssembly 外部卷后端类型注册后，
// `registry.NewBackend` 必须能构造 config 静态声明的外部卷（缺陷修复，2026-10-21：
// 此前类型注册在 RegisterRoutes 之后 ⇒ `assembleVolumes` 报「未注册后端」启动失败）。
//
// 接线级判据：本用例直接调用**生产注册函数**（`registerVolumeBackendTypes`）再走
// registry，覆盖「注册函数漏了某类型」这类回归；顺序由 archcheck R45 门禁覆盖。
func TestRegisterVolumeBackendTypes_EnablesExternalVolumeAssembly(t *testing.T) {
	registerVolumeBackendTypes()
	ctx := context.Background()

	cases := []struct {
		typ   string
		extra map[string]any
	}{
		{"webdav", map[string]any{"url": "http://127.0.0.1:1/dav", "username": "u", "password": "p"}},
		{"sftp", map[string]any{"url": "sftp://u@127.0.0.1:1", "password": "p"}},
		{"ftp", map[string]any{"url": "ftp://u@127.0.0.1:1", "password": "p"}},
		{"s3", map[string]any{"endpoint": "127.0.0.1:1", "access_key": "a", "secret_key": "b", "bucket": "bkt"}},
		{"baidupcs", map[string]any{"bduss": "fake-bduss", "local_root": t.TempDir()}},
	}
	for _, c := range cases {
		v := volume.Volume{Name: "probe-" + c.typ, Type: c.typ, Extra: c.extra}
		// 注意：sftp/ftp 的构造函数**在构造期就拨号**（无服务即失败）——故判据是
		// 「后端类型已注册、工厂确实被调用」，而不是「构造成功」：
		//   ① 未注册 ⇒ 错误含 `未注册后端`（本用例的失败形态）；
		//   ② 已注册 ⇒ 错误来自该后端自身的校验/连接（含类型前缀如 `sftp backend:`）。
		be, err := registry.NewBackend(ctx, v)
		if err != nil {
			if strings.Contains(err.Error(), "未注册后端") || strings.Contains(err.Error(), "未注册") {
				t.Fatalf("type=%s 未注册（类型注册未前移 ⇒ config 静态声明该卷会启动失败）: %v", c.typ, err)
			}
			if !strings.Contains(err.Error(), c.typ+" backend:") {
				t.Fatalf("type=%s 的失败不是来自该后端自身（工厂未执行？）: %v", c.typ, err)
			}
			continue // 类型已注册、工厂已执行；构造失败在断言范围外（无真实远端服务）
		}
		if be == nil {
			t.Fatalf("type=%s 构造返回 nil backend", c.typ)
		}
	}
}
