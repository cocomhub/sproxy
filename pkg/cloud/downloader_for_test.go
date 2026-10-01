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
