// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
)

// TestRegisterPikpakResolver_InsecureTLS 验证 insecure_tls=true 时注册的解析器
// 能识别 keepshare 分享 URL（insecure 只影响出站 TLS 校验，不影响识别）。
func TestRegisterPikpakResolver_InsecureTLS(t *testing.T) {
	t.Parallel()
	reg := downloader.NewRegistry()
	registerPikpakResolver(reg, true)
	res, ok := reg.ResolverFor("https://keepshare.org/k1/magnet:?xt=urn:btih:abc")
	if !ok || res == nil {
		t.Fatal("keepshare URL 应被解析器识别")
	}
	if res.Name() != pikpak.URLResolverName {
		t.Fatalf("解析器名 = %q", res.Name())
	}
	// insecure 模式注册不应影响普通 mypikpak 识别
	res2, ok2 := reg.ResolverFor("https://mypikpak.com/s/abc")
	if !ok2 || res2 == nil {
		t.Fatal("mypikpak URL 应被识别")
	}
}
