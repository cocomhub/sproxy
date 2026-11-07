// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// TestDownloadCmd_RuntimeErrorDoesNotDumpUsage 验证运行期错误只报错、不打印 Usage
// （开源 CLI 惯例；此前 `sclient download` 失败会把整段 help 刷到 stderr）。
// 无 config.yaml ⇒ factory 在发起任何网络请求前 fail-closed，正好覆盖 RunE 报错路径。
func TestDownloadCmd_RuntimeErrorDoesNotDumpUsage(t *testing.T) {
	// sproxy:serial: 隔离 HOME/XDG 与全局 resolvedContext/cfgFile（见 t.Setenv 豁免）。
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows 的 os.UserHomeDir 走 USERPROFILE
	t.Setenv("SCLIENT_ENV", "")
	setXDGConfigHome(t)

	root := NewRootCmd()
	var errBuf bytes.Buffer
	root.SetOut(&errBuf)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"download", "missing.bin"})

	err := root.Execute()
	if err == nil {
		t.Fatal("无配置时应返回错误（fail-closed）")
	}
	out := errBuf.String()
	if strings.Contains(out, "Usage:") {
		t.Fatalf("运行期错误不应打印 Usage：\n%s", out)
	}
	if strings.Contains(out, "Error:") {
		t.Fatalf("cobra 不应自行打印错误（由 main 单点打印）：\n%s", out)
	}
}

// TestRootCmd_SilencesUsageAndErrors 是行为守卫：根命令必须关闭 cobra 的 Usage/Error
// 自动打印（改回默认值即红）。
func TestRootCmd_SilencesUsageAndErrors(t *testing.T) {
	root := NewRootCmd()
	if !root.SilenceUsage {
		t.Error("root.SilenceUsage 应为 true（运行期错误不得刷 help）")
	}
	if !root.SilenceErrors {
		t.Error("root.SilenceErrors 应为 true（错误由 main 单点打印）")
	}
}

// TestRenameReplaceWith_RetriesTransientSharingViolation 锁定 Windows 上「覆盖被占用
// 目标瞬时失败」的重试逻辑（E2E 实测 --decrypt 最终替换约 2/5 概率 "Access is denied"）。
func TestRenameReplaceWith_RetriesTransientSharingViolation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	rename := func(_, _ string) error {
		if calls.Add(1) < 3 {
			return errors.New("Access is denied")
		}
		return nil
	}
	if err := renameReplaceWith(rename, "tmp", "dst"); err != nil {
		t.Fatalf("瞬时失败应在重试后成功: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("应重试至第 3 次成功，实际调用 %d 次", got)
	}
}

// TestRenameReplaceWith_PropagatesPersistentError 持续失败时返回错误（不吞掉）。
func TestRenameReplaceWith_PropagatesPersistentError(t *testing.T) {
	t.Parallel()
	rename := func(_, _ string) error { return errors.New("persistent denied") }
	if err := renameReplaceWith(rename, "tmp", "dst"); err == nil {
		t.Fatal("持续失败应返回错误")
	}
}
