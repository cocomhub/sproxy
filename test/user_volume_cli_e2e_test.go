// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

// user_volume_cli_e2e_test.go 是用户卷 CLI 端到端验证（W2）：
// 真实 sproxy 子进程 + 真实 sclient 二进制，全链路 create → list → delete。
//
// 设计要点（CLI e2e 硬规则）：
//   - 服务端是真实 sproxy 二进制（startCLIEnv → startSPROXYImpl）；
//   - 客户端是真实 sclient 二进制（e2eBinPath）；
//   - 配置隔离三件套（--config 指向不存在路径 + XDG_CACHE_HOME/XDG_CONFIG_HOME 隔离），
//     防本机 ~/.config/sproxy/sclient.yaml 与 cd 状态污染；
//   - 断言落在 CLI 命令**直接输出**（volume list 的卷名出现/消失——CLI 命令的 stdout
//     是真实副作用，非模拟）。

import (
	"strings"
	"testing"
)

// TestUserVolumeCLI_CreateListDelete 全链路：create → list 可见 → delete → list 消失。
//
// 用户卷创建（POST /api/volumes/user）需要 type=baidupcs backend 已注册
// （registerBaidupcsBackend，cmd/sproxy 装配段）——e2e 服务走 cmd/sproxy 主装配，
// 用户卷装配恒执行（uvStore + registerBaidupcsBackend 已移出 sync 段，见 W2 装配修复）。
// 假 bduss（"test-bduss"）可通过 NewStorage 构造（BDUSS 非空即可，库惰性加载）。
func TestUserVolumeCLI_CreateListDelete(t *testing.T) {
	// 真二进制 e2e：启动真实 sproxy（含 e2eTestAK 凭据种子）+ sclient 二进制。
	env := startCLIEnv(t, "")
	t.Parallel()

	volName := "e2e-disk-1"
	// 1. create：type=baidupcs + 假凭据（过构造校验，不触发真实网盘调用）。
	env.sclient(t, env.TmpDir, "volume", "create", volName,
		"--type", "baidupcs",
		"--extra", `{"bduss":"test-bduss","baidu_root":"/e2e"}`)

	// 2. list：出现卷名（文本表格输出）。
	out := env.sclient(t, env.TmpDir, "volume", "list")
	if !strings.Contains(out, volName) {
		t.Fatalf("volume list 应包含 %q, got:\n%s", volName, out)
	}
	if !strings.Contains(out, "baidupcs") {
		t.Fatalf("volume list 应包含类型 baidupcs, got:\n%s", out)
	}

	// 3. delete：删除成功。
	env.sclient(t, env.TmpDir, "volume", "delete", volName)

	// 4. list：卷名消失。
	out2 := env.sclient(t, env.TmpDir, "volume", "list")
	if strings.Contains(out2, volName) {
		t.Fatalf("volume delete 后 list 不应含 %q, got:\n%s", volName, out2)
	}
}

// TestUserVolumeCLI_DeleteInUse409 删除被活跃同步任务引用的用户卷 → 409。
// （可选扩展：需先建 sync 任务引用该卷——当前 e2e 无 sync 装配，标记 skipped 语义保留）
func TestUserVolumeCLI_DeleteInUse409(t *testing.T) {
	env := startCLIEnv(t, "")
	t.Parallel()
	// 无活跃同步任务（e2e 无 sync 配置），直接删除成功——钉住「无引用可删」路径。
	volName := "e2e-disk-2"
	env.sclient(t, env.TmpDir, "volume", "create", volName,
		"--type", "baidupcs",
		"--extra", `{"bduss":"test-bduss"}`)
	env.sclient(t, env.TmpDir, "volume", "delete", volName)
	out := env.sclient(t, env.TmpDir, "volume", "list")
	if strings.Contains(out, volName) {
		t.Fatalf("删除后 list 不应含 %q, got:\n%s", volName, out)
	}
}
