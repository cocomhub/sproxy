// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// sighup_alerts_test.go 钉住 SIGHUP 告警规则热加载（roadmap 11.1-②）：
// handleSighup 重载配置后把新 notify.alerts 规则原子换入 AlertEngine
// （旧规则不再命中、新规则下个事件生效），并输出「alerts 规则已热加载」日志；
// alerts 未装配（引擎 nil）时跳过，零影响。
//
// 串行原因：操作包级变量 cfgProvider/cfgPtr/cfgFile，与既有 handleSighup
// 用例互斥（同一包级状态）。
// sproxy:serial: 操作包级 cfgProvider/cfgPtr/cfgFile，与既有 SIGHUP 用例互斥

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
)

// sighupFakeChannel 记录告警消息的假渠道（cmd 包内独立实现，server 包的不导出）。
type sighupFakeChannel struct {
	name string
	msgs chan string
}

func (f *sighupFakeChannel) Name() string { return f.name }

func (f *sighupFakeChannel) Send(ctx context.Context, m server.NotifyMessage) error {
	f.msgs <- m.Title + "|" + m.Text
	return nil
}

// newSighupHandlers 装配真实 Handlers（alerts 启用时含 AlertEngine），返回
// h 与独立 cfgPtr（handleSighup 不改写它）。
func newSighupHandlers(t *testing.T, cfg *server.Config) (*server.Handlers, *atomic.Pointer[server.Config]) {
	t.Helper()
	var handlerCfgPtr atomic.Pointer[server.Config]
	handlerCfgPtr.Store(cfg)
	h := server.RegisterRoutes(t.Context(), server.RegisterRoutesOpts{
		Mux:     http.NewServeMux(),
		CfgPtr:  &handlerCfgPtr,
		Version: "test",
		BuildAt: "test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(func() { _ = h.Close() })
	return h, &handlerCfgPtr
}

// TestHandleSighup_ReloadsAlertRules 验证 handleSighup 热加载告警规则：
// 初始规则 degraded→chA；SIGHUP 后新规则 degraded→chB——新事件走 chB、
// 旧渠道不再命中；日志含「alerts 规则已热加载」。
func TestHandleSighup_ReloadsAlertRules(t *testing.T) {
	// sproxy:serial: 操作包级 cfgProvider/cfgPtr/cfgFile，与既有 SIGHUP 用例互斥
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sproxy.yaml")

	initialCfg := server.Default()
	initialCfg.Addr = "127.0.0.1:0"
	initialCfg.StorageRoot = t.TempDir()
	initialCfg.Alerts.Enabled = true
	initialCfg.Alerts.Rules = []server.AlertRule{{
		Source: "volume_degraded", Channels: []string{"chA"},
	}}
	if err := server.SaveConfig(initialCfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	cfgProvider = setupProviderForSighup(cfgPath)
	t.Cleanup(func() { cfgProvider = nil })
	cfgFile = cfgPath
	t.Cleanup(func() { cfgPtr.Store(nil) })
	cfgPtr.Store(initialCfg)

	h, _ := newSighupHandlers(t, initialCfg)
	eng := h.AlertEngine()
	if eng == nil {
		t.Fatal("alerts.enabled=true + rules 应装配非 nil AlertEngine")
	}
	chA := &sighupFakeChannel{name: "chA", msgs: make(chan string, 64)}
	chB := &sighupFakeChannel{name: "chB", msgs: make(chan string, 64)}
	if !eng.Register(chA) {
		t.Fatal("注册 chA 失败")
	}
	if !eng.Register(chB) {
		t.Fatal("注册 chB 失败")
	}

	// 修改配置：规则渠道 chA → chB。
	newCfg := *initialCfg
	newCfg.Alerts.Rules = []server.AlertRule{{
		Source: "volume_degraded", Channels: []string{"chB"},
	}}
	if err := server.SaveConfig(&newCfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	restore := captureSlog(&buf)
	defer restore()

	handleSighup(initialCfg, h)

	if !strings.Contains(buf.String(), "alerts 规则已热加载") {
		t.Errorf("缺少热加载日志；输出:\n%s", buf.String())
	}

	// 新规则生效：事件走 chB，旧渠道 chA 不再命中。
	eng.OnVolumeDegraded(context.Background(), "vol1", "probe failed")
	select {
	case <-chB.msgs:
	default:
		t.Fatal("SIGHUP 后新规则应命中 chB")
	}
	select {
	case <-chA.msgs:
		t.Fatal("SIGHUP 后旧渠道 chA 不应再命中")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHandleSighup_AlertsDisabledNoEngine 验证 alerts 未装配（引擎 nil）时
// handleSighup 跳过规则热加载：不 panic、无热加载日志、行为零变化。
func TestHandleSighup_AlertsDisabledNoEngine(t *testing.T) {
	// sproxy:serial: 操作包级 cfgProvider/cfgPtr/cfgFile，与既有 SIGHUP 用例互斥
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sproxy.yaml")

	initialCfg := server.Default()
	initialCfg.Addr = "127.0.0.1:0"
	initialCfg.StorageRoot = t.TempDir()
	if err := server.SaveConfig(initialCfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	cfgProvider = setupProviderForSighup(cfgPath)
	t.Cleanup(func() { cfgProvider = nil })
	cfgFile = cfgPath
	t.Cleanup(func() { cfgPtr.Store(nil) })
	cfgPtr.Store(initialCfg)

	h, _ := newSighupHandlers(t, initialCfg)
	if h.AlertEngine() != nil {
		t.Fatal("alerts 未启用应返回 nil AlertEngine")
	}

	newCfg := *initialCfg
	newCfg.LogLevel = "debug"
	if err := server.SaveConfig(&newCfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	restore := captureSlog(&buf)
	defer restore()

	// h.AlertEngine() 为 nil → 跳过 ReloadRules（不 panic、零影响）。
	handleSighup(initialCfg, h)

	if strings.Contains(buf.String(), "alerts 规则已热加载") {
		t.Errorf("alerts 未装配不应输出热加载日志；输出:\n%s", buf.String())
	}
	if got := cfgPtr.Load().LogLevel; got != "debug" {
		t.Errorf("软配置 log_level 应仍生效, got %q", got)
	}
}
