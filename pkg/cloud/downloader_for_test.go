// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// urlMatchDownloader 按 URL 前缀匹配的下载器（测试用）。
type urlMatchDownloader struct {
	prefix string
	name   string
	calls  int
}

func (d *urlMatchDownloader) Name() string { return d.name }
func (d *urlMatchDownloader) Supports(source string) bool {
	return strings.HasPrefix(source, d.prefix)
}
func (d *urlMatchDownloader) Download(_ context.Context, source, destPath string, _ downloader.ProgressFunc) (*downloader.Result, error) {
	d.calls++
	return &downloader.Result{Size: 1}, nil
}

// TestDownloaderFor_AutoDiscover 验证按 URL 自动发现（用本地注册表，不碰全局）：
//  1. 注册了匹配下载器（如 pikpak 匹配 mypikpak 分享 URL）→ 命中；
//  2. 未注册的 URL → 回落配置默认下载器 m.dl；
//  3. 内置 "http" 不参与自动发现（回落 m.dl 保配置）。
func TestDownloaderFor_AutoDiscover(t *testing.T) {
	t.Parallel()
	reg := downloader.NewRegistry()
	// 注册匹配 mypikpak 分享 URL 的下载器到本地注册表
	pikpakDL := &urlMatchDownloader{name: "pikpak", prefix: "https://mypikpak.com/s/"}
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name: "pikpak", Instance: pikpakDL, Priority: 100,
	})

	mgr, _ := newCloudTestManager(t, t.TempDir(), nil, &CloudDownloadConfig{})
	// 默认下载器（m.dl）：未命中时回落
	defaultDL := &urlMatchDownloader{name: "default", prefix: "http://"}
	mgr.dl = defaultDL
	mgr.registry = reg

	// 1. mypikpak 分享 URL → 命中 pikpak
	if got := mgr.downloaderFor("https://mypikpak.com/s/abc123/xyz"); got.Name() != "pikpak" {
		t.Fatalf("expected pikpak downloader for mypikpak URL, got %q", got.Name())
	}
	// 2. 普通 http URL → 回落默认
	if got := mgr.downloaderFor("http://example.com/file.bin"); got.Name() != "default" {
		t.Fatalf("expected default downloader for http URL, got %q", got.Name())
	}
	// 3. 内置 http（注册表兜底）不参与发现 → 也回落默认
	if got := mgr.downloaderFor("https://example.com/x.bin"); got.Name() != "default" {
		t.Fatalf("expected default downloader (http excluded), got %q", got.Name())
	}
}

// TestDefaultDownloader_NotHijackedByPlugin 锁定默认下载器语义不被插件注册劫持：
// `newDefaultDownloader` 对默认（空/"http"）配置**必须直接返回内置 HTTP**，而非经
// `NewFromConfig` 的 Active() 回退到最高优先级插件。
// 否则 pikpak 等外部下载器（Priority>0）注册进 DefaultRegistry 后，cloud manager 的
// m.dl 会被劫持成 pikpak：SSRF/超时/出口拨号 clone 配置全部丢失，且普通 URL 回落
// m.dl 会因 parseShareID 失败而全灭。
func TestDefaultDownloader_NotHijackedByPlugin(t *testing.T) {
	t.Parallel()
	// 契约：空名称（默认）→ 内置 HTTP。
	if got := newDefaultDownloader(&CloudDownloadConfig{}); got.Name() != "http" {
		t.Fatalf("default downloader (empty config) must be http, got %q", got.Name())
	}
	// 契约：显式 "http" → 内置 HTTP。
	if got := newDefaultDownloader(&CloudDownloadConfig{Downloader: "http"}); got.Name() != "http" {
		t.Fatalf("default downloader (http config) must be http, got %q", got.Name())
	}
	// 契约：已注册的外部下载器名 → 按名精确取回（不回落 Active）。
	reg := downloader.NewRegistry()
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "pikpak",
		Instance: &urlMatchDownloader{name: "pikpak", prefix: "https://mypikpak.com/s/"},
		Priority: 100,
	})
	// newDefaultDownloader 的按名分支走 DefaultRegistry，此处验证取回行为不依赖 Priority。
	if d, ok := reg.Get("pikpak"); !ok || d.Name() != "pikpak" {
		t.Fatalf("registry Get by name must return pikpak regardless of priority, ok=%v name=%q", ok, d.Name())
	}
}
