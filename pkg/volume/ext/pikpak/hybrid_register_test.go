// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// TestHybridDownloader_RegisterPriority 锁定 F1a：hybrid 注册 Priority 高于旧 pikpak，
// 分享 URL 自动发现应优先路由 hybrid。
// 注意：注册目标用**本地 Registry 实例**（downloader.NewRegistry()）而非全局
// DefaultRegistry——全局能力可测试化策略（R18，2026-10-05）：不触碰全局 → 测试可并行。
func TestHybridDownloader_RegisterPriority(t *testing.T) {
	t.Parallel()
	reg := downloader.NewRegistry() // 本地实例，等价生产全局语义
	// 注册 hybrid（Priority 11）+ 旧 pikpak（Priority 10）
	hybridDL := &fakeSupporter{name: "pikpak-hybrid", support: true}
	oldDL := &fakeSupporter{name: "pikpak", support: true}
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name: "pikpak-hybrid", Instance: hybridDL, Priority: 11,
	})
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name: "pikpak", Instance: oldDL, Priority: 10,
	})
	// downloaderFor 语义：遍历 Names，最高优先级命中（注册表 Active）
	active := reg.Active()
	if active == nil || active.Name() != "pikpak-hybrid" {
		t.Fatalf("Active() = %v, want pikpak-hybrid (highest priority)", active)
	}
}

// TestRegisterHybridDownloader_InjectsLocalRegistry 锁定可测试化注册入口：
// RegisterHybridDownloader(reg, cfg) 把 hybrid 注册进**传入的**注册表（本地实例），
// 不触碰全局 DefaultRegistry → 与 RegisterPriority 可并行。
func TestRegisterHybridDownloader_InjectsLocalRegistry(t *testing.T) {
	t.Parallel()
	reg := downloader.NewRegistry()
	// 注册旧 pikpak 占位（Priority 10）——验证 hybrid（11）经注入入口注册后胜出
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name: "pikpak", Instance: &fakeSupporter{name: "pikpak", support: true}, Priority: 10,
	})
	// 最小真实配置（resolver/api 零值可构造，无需服务器——NewHybridDownloader 不建连）
	if err := RegisterHybridDownloader(reg, HybridConfig{
		Resolver: NewShareResolver(ShareResolverConfig{}),
		API:      NewAPI(APIConfig{}, nil),
	}); err != nil {
		t.Fatalf("RegisterHybridDownloader error: %v", err)
	}
	if active := reg.Active(); active == nil || active.Name() != "pikpak-hybrid" {
		t.Fatalf("Active() = %v, want pikpak-hybrid via injected local registry", active)
	}
	// 全局注册表不受影响：不包含 pikpak-hybrid
	if d := downloader.DefaultRegistry.Active(); d != nil && d.Name() == "pikpak-hybrid" {
		t.Fatal("global registry must not be polluted by local-registry test")
	}
}

// fakeSupporter 是 fake downloader（测试注册表）。
type fakeSupporter struct {
	name    string
	support bool
}

// Download 实现。
func (f *fakeSupporter) Download(ctx context.Context, source, dest string, p downloader.ProgressFunc) (*downloader.Result, error) {
	return &downloader.Result{}, nil
}

// Supports 实现。
func (f *fakeSupporter) Supports(source string) bool { return f.support }

// Name 实现。
func (f *fakeSupporter) Name() string { return f.name }

// TestHybridDownloader_HybridCounters 锁定 🟠3：下载器实例实现 HybridCounters()
// （writeHybridMetrics 断言必需，否则 Prometheus 指标永不导出）。
func TestHybridDownloader_HybridCounters(t *testing.T) {
	t.Parallel()
	m := &HybridMetrics{}
	m.ShareBytesSaved.Add(1024)
	m.DowngradeTotal.Add(3)
	hd := &HybridDownloader{metrics: m}
	// 断言接口（writeHybridMetrics 的断言形态）
	hm, ok := any(hd).(interface {
		HybridCounters() map[string]int64
	})
	if !ok {
		t.Fatal("HybridDownloader should implement HybridCounters() (🟠3)")
	}
	counters := hm.HybridCounters()
	if counters["pikpak_hybrid_share_bytes_saved"] != 1024 {
		t.Errorf("share_bytes_saved = %d, want 1024", counters["pikpak_hybrid_share_bytes_saved"])
	}
	if counters["pikpak_hybrid_downgrade_total"] != 3 {
		t.Errorf("downgrade_total = %d, want 3", counters["pikpak_hybrid_downgrade_total"])
	}
}
