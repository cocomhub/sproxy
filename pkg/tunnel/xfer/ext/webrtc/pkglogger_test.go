// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestSetLogger_InjectsPackageLevel 验证 SetLogger(discard) 后包级 Warn 走注入 logger
// （无全局输出——此前 4 处 slog.Warn 直写 slog.Default，基准/测试注入的 discard 关不掉）。
func TestSetLogger_InjectsPackageLevel(t *testing.T) {
	// sproxy:serial: 修改包级 pkgLogger 全局状态——与其它 SetLogger 测试串行（并发覆盖互扰）
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	SetLogger(discard)
	t.Cleanup(SetLoggerDefault)
	// 注入后读取回同一实例（行为由注入 handler 拦截——不再直写 slog.Default）。
	got := getLogger()
	if got != discard {
		t.Fatalf("getLogger = %v, want 注入的 discard", got)
	}
}

// TestSetLogger_EmptyRestoresDefault 验证 SetLoggerDefault 恢复 slog.Default。
func TestSetLogger_EmptyRestoresDefault(t *testing.T) {
	// sproxy:serial: 修改包级 pkgLogger 全局状态——与其它 SetLogger 测试串行
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	SetLogger(discard)
	SetLoggerDefault()
	if l := getLogger(); l == discard {
		t.Fatalf("SetLoggerDefault 后仍是指向 discard 的 logger")
	}
}

// 触发包级 Warn 的辅助（直接调 validateSTUNURL 内部路径——非法 URL 触发 Warn）。
func TestPkgLoggerWarn_InvalidURL(t *testing.T) {
	// sproxy:serial: 修改包级 pkgLogger 全局状态——与其它 SetLogger 测试串行
	var buf strings.Builder
	// 注入一个捕获 handler——验证包级 Warn 被捕获（非 slog.Default 输出）。
	capture := slog.New(slog.NewTextHandler(&buf, nil))
	SetLogger(capture)
	t.Cleanup(SetLoggerDefault)
	// 触发 SetSTUNServers 的 Warn：urls 含非法 STUN URL（validSTUNURL 循环）。
	SetSTUNServers([]string{"not-a-url"})
	if !strings.Contains(buf.String(), "忽略非法的 STUN/TURN URL") {
		t.Fatalf("包级 Warn 未被注入 logger 捕获, got %q", buf.String())
	}
}
