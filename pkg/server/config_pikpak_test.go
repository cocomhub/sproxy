// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
)

// TestConfig_PikpakDefaults 钉住 PikPak 配置默认值与类型：
//  1. 默认（零值）时各字段为空/零值（超时由下载器层兜底 2h，非 Config 层）；
//  2. Pikpak.Timeout 是 time.Duration 字段，YAML 字符串（如 "3h"）经 viper
//     duration hook 解码——此处验证字段类型与语义（解析路径见 sproxycfg）。
func TestConfig_PikpakDefaults(t *testing.T) {
	t.Parallel()
	c := Default()
	if c.Pikpak.BinaryPath != "" {
		t.Fatalf("expected empty binary_path default, got %q", c.Pikpak.BinaryPath)
	}
	if c.Pikpak.AutoInstall {
		t.Fatal("expected auto_install default false")
	}
	if c.Pikpak.AutoDelete {
		t.Fatal("expected auto_delete default false")
	}
	// Timeout 零值 = 下载器层用默认 2h；类型必须是 time.Duration（YAML duration 语法）。
	// （编译期即验证字段类型；此处仅断言零值语义。）
	if c.Pikpak.Timeout != 0 {
		t.Fatalf("expected zero timeout default (downloader falls back to 2h), got %v", c.Pikpak.Timeout)
	}
}

// TestConfig_PikpakHybridDefaults 锁定 F1b：hybrid 配置默认值（Disable 零值语义 + AutoDelete）。
func TestConfig_PikpakHybridDefaults(t *testing.T) {
	t.Parallel()
	c := Default()
	// Disable 零值 = false → 功能默认打开（注册层 !Disable = 注册 hybrid）。
	if c.Pikpak.Hybrid.Disable {
		t.Fatal("expected hybrid.disable default false (= enabled)")
	}
	// ChunkSize 默认 32MiB（2026-10-07 对齐：分片更细中断粒度更小，与 hybrid.go 常量一致）；
	// ShareRatio/Concurrency 保持零默认（下载器内部回落）。
	if c.Pikpak.Hybrid.ChunkSize != 32<<20 {
		t.Fatalf("expected hybrid.chunk_size default 32MiB, got %d", c.Pikpak.Hybrid.ChunkSize)
	}
	if c.Pikpak.Hybrid.ShareRatio != 0 || c.Pikpak.Hybrid.Concurrency != 0 {
		t.Fatal("expected hybrid share_ratio/concurrency zero defaults (downloader falls back)")
	}
	// AutoDelete 默认 true（2026-10-05 修正：与 config.go 注释一致，6GB 空间必须释放）。
	if !c.Pikpak.Hybrid.AutoDelete {
		t.Fatal("expected hybrid.auto_delete default true (6GB 空间释放；NH-P1 已防误删用户文件)")
	}
}
