// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// dead_symbols_test.go 是「**已确认删除的死代码不得复活**」的墓碑门禁（R11）。
//
// 触发背景：2026-09-14 的死代码审计发现，仓库里积累了一批「生产调用已被删除、函数与测试
// 仍在养着它」的遗留符号——最典型的是 #90「清理死代码」那次提交删掉了 runBatchOperation 的
// 调用行却保留了函数体与测试，此后无人再碰。这类代码不会自己消失，只会误导阅读者。
//
// 判据（结构性，不做语义猜测）：以下符号不得以**词边界**形式出现在任何**非测试**源码中。
// 用 `git grep -w`（词边界而非固定子串）是必须的：`startMeshNodeRole` 是
// `startMeshNodeRoleWithCreds` 的前缀，固定子串匹配会在删掉包装后依然命中合法函数。每个条目都对应一次
// 有 git 取证的清理，证据见 docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md §2.1。
// 允许出现在 `_test.go` 中（例如把旧调用点改写为规范入口的对照断言）。
//
// 为什么不直接用 deadcode 工具当门禁：deadcode 不带 -test 时会把「仅被测试引用」的 helper
// （NewMock/DiscardLogger/SetHostOnly 等）一并报为不可达，输出永不为空，无法作为失败条件；
// 带 -test 时又会把上面这些真正该删的符号也算作可达。所以用一份显式的墓碑清单来守。

import (
	"os/exec"
	"strings"
	"testing"
)

// deadSymbols 是 2026-09-14 审计确认删除的历史遗留符号（逐条附删除依据）。
var deadSymbols = []string{
	// 生产调用被 client.FileClient.Archive 取代，仅剩测试引用。
	"writeArchiveResponse",
	// 生产调用被 batch_delete.go / batch_rename.go 的内联循环取代，仅剩测试引用。
	"runBatchOperation",
	// 生产调用被 download-archive（下载原始归档，不在本地解压）取代，仅剩测试引用。
	"extractTarGz",
	// 自 S5 引入起 root.go 就直接调用 startMeshNodeRoleWithCreds，该包装从未接过线。
	"startMeshNodeRole",
	// tunnel_key 已废除、handleSighup 不再热替换密钥，UpdateKey 全仓零调用。
	"TunnelUpdater",
	// 同次清理：Handlers.TunnelHandler() 访问器与 h.tunnelHandler 字段同义，删除后仅由字段担 POST /tunnel 路由；
	// 该名字不通用（仅 root.go 一条注释曾提及），故可入墓碑。
	"TunnelHandler",
	// 无任何实现断言或消费方的空接口（protoc 生成后才会出现的 Xfer_StreamServer；当前为手写骨架接口）。
	"XferServer",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestNoResurrectedDeadSymbols(t *testing.T) {
	t.Parallel()
	// 必须显式以仓库根为工作目录：`git grep` 默认只搜**当前目录**，而 go test 的 cwd 是
	// internal/archcheck ⇒ 不指定就只搜本包，门禁会假绿（实测踩到，首版即如此）。
	root := repoRoot(t)
	for _, sym := range deadSymbols {
		cmd := exec.Command("git", "grep", "-nw", sym, "--", "*.go", ":(exclude)*_test.go")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("git grep %s: %v", sym, err)
			}
			continue // ExitCode 1 = 无匹配 = 期望结果
		}
		t.Errorf("已删除符号 %q 复活：\n%s", sym, strings.TrimSpace(string(out)))
	}
}
