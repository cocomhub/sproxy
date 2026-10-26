// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
)

// TestRegisterPikpakURLResolver_ShareURLMatches 验证装配层注册 pikpak URL 解析扩展后，
// 分享 URL 能被解析为 pikpak_share 扩展（而非通用回落 generic）——这是
// /api/resolve 对 PikPak 分享返回文件清单（含 hash）的**装配前提**。
//
// 回归背景（2026-10-26）：pkg/volume/ext/pikpak 的 URLResolver 已实现并有测试，
// 但 cmd/sproxy 装配层从未 RegisterResolver → /api/resolve 永远 kind=generic，
// sdserver 拿不到分享文件清单/hash（GCID 校验无法进行）。
func TestRegisterPikpakURLResolver_ShareURLMatches(t *testing.T) {
	t.Parallel()
	reg := downloader.NewRegistry()
	registerPikpakResolver(reg, false)

	shareURL := "https://mypikpak.com/s/EXAMPLEID123/token456"
	res, ok := reg.ResolverFor(shareURL)
	if !ok {
		t.Fatal("注册后分享 URL 应有匹配的解析扩展（否则回落 generic）")
	}
	if res.Name() != pikpak.URLResolverName {
		t.Fatalf("解析扩展名 = %q, want %q", res.Name(), pikpak.URLResolverName)
	}
}

// TestRegisterPikpakURLResolver_NonShareNotMatched 验证非分享 URL 不被 pikpak 扩展匹配
// （仍走通用回落，行为零回归）。
func TestRegisterPikpakURLResolver_NonShareNotMatched(t *testing.T) {
	t.Parallel()
	reg := downloader.NewRegistry()
	registerPikpakResolver(reg, false)

	for _, u := range []string{
		"https://example.com/movie.mp4",
		"https://mypikpak.com/drive/v1/files", // 非 /s/ 分享路径
	} {
		if _, ok := reg.ResolverFor(u); ok {
			t.Fatalf("%q 不应被 pikpak 扩展匹配", u)
		}
	}
}
