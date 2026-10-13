// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// e2e_cli_volume_cwd_test.go 覆盖**卷感知工作目录**（用户口径 2026-10-13）：
// `cd <卷名>:/<路径>` 把工作目录切到该卷；其后所有相对路径操作（list/upload/meta/download/
// mkdir/delete…）默认只作用于**当前工作目录所在卷**；`cd /<路径>` 回到**本地卷**根。
//
// 本用例**不能并行**：cd 的工作目录（含卷名）落在 XDG 缓存里，必须隔离到临时目录，避免污染
// 真实用户缓存（用 t.Setenv，故不能 t.Parallel）。
func TestE2E_CLI_VolumeCwd_DefaultsToCurrentVolume(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("LOCALAPPDATA", cacheRoot) // Windows 上 adrg/xdg 走 LOCALAPPDATA
	env, _ := newSecretdataPlaybackEnv(t)

	// 1) cd 到加密卷卷根 → pwd 显示 `<卷名>:/`。
	env.sclient(t, env.TmpDir, "cd", "vault:/")
	if got := strings.TrimSpace(env.sclient(t, env.TmpDir, "pwd")); got != "vault:/" {
		t.Fatalf("pwd=%q want vault:/", got)
	}

	// 2) 相对路径上传（**不带 --volume**）：目标卷来自工作目录 ⇒ 落到加密卷。
	content := []byte("volume cwd payload")
	if err := os.WriteFile(filepath.Join(env.TmpDir, "cwdfile.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, env.TmpDir, "upload", "cwdfile.bin")

	// 3) list（不带 --volume）默认列当前卷 ⇒ 必须看到该文件（此前未指定卷只列本地卷 ⇒ 看不到）。
	if out := env.sclient(t, env.TmpDir, "list"); !strings.Contains(out, "cwdfile.bin") {
		t.Fatalf("工作目录卷下 list 应含 cwdfile.bin, got:\n%s", out)
	}

	// 4) 相对路径 mkdir + cd 进入子目录（URI 绝对路径）→ pwd 显示新位置。
	env.sclient(t, env.TmpDir, "mkdir", "sub")
	env.sclient(t, env.TmpDir, "cd", "vault:/sub")
	if got := strings.TrimSpace(env.sclient(t, env.TmpDir, "pwd")); got != "vault:/sub" {
		t.Fatalf("pwd=%q want vault:/sub", got)
	}
	// 该子目录确实建在加密卷里（卷内 list 可见 sub/）。
	if out := env.sclient(t, env.TmpDir, "list", "vault:/"); !strings.Contains(out, "sub") {
		t.Fatalf("vault 卷根应含 sub 目录, got:\n%s", out)
	}

	// 4.1) 加密卷**目录删除**尚未接线 ⇒ 必须 fail-closed 且提示清晰（绝不能静默删本地同名目录）。
	if _, stderr, rerr := env.sclientRun(t, env.TmpDir, "rmdir", "vault:/sub", "--force"); rerr == nil {
		t.Fatal("外部/加密卷 rmdir 应 fail-closed（当前未接线）")
	} else if !strings.Contains(stderr, "暂不支持删除目录") {
		t.Fatalf("rmdir 失败信息应说明原因, got: %s", stderr)
	}

	// 5) 相对路径 download（从工作目录卷根起算）。
	env.sclient(t, env.TmpDir, "cd", "vault:/")
	dl := filepath.Join(env.TmpDir, "dl")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	env.sclient(t, dl, "download", "cwdfile.bin")
	got, err := os.ReadFile(filepath.Join(dl, "cwdfile.bin"))
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("下载内容不一致: %q", got)
	}

	// 6) `cd /` 回到**本地卷**根：此后 list 不再含加密卷内容（视图切换生效）。
	env.sclient(t, env.TmpDir, "cd", "/")
	if got := strings.TrimSpace(env.sclient(t, env.TmpDir, "pwd")); got != "/" {
		t.Fatalf("pwd=%q want /", got)
	}
	if out := env.sclient(t, env.TmpDir, "list"); strings.Contains(out, "cwdfile.bin") {
		t.Fatalf("回到本地卷后 list 不应含加密卷文件, got:\n%s", out)
	}
}
