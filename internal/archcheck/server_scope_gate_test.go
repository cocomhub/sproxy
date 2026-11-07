// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// server_scope_gate_test.go 是门禁 **R6**：**server 作用域与装配根不得反向依赖**。
//
// 背景（server 治理 S1）：`pkg/server` 是装配根（唯一负责把各切片接起来），
// `pkg/server/<cap>` 是服务端作用域的能力包（config / 未来的 httpapi、volume、cloud…）。
// 两者放在同一子树，但依赖方向必须单向：
//
//	装配根 pkg/server  ──import──▶  pkg/server/<cap>  ──import──▶  pkg/**（Global）
//
// 若 `<cap>` 反过来导入装配根，一旦装配根开始 import 各切片就成环（Go 直接编译失败），
// 更糟的是在成环前会先形成「切片偷偷用装配层的全局状态」——那正是本次治理要消除的耦合。
//
// 同时断言**作用域登记**：`pkg/server/**` 下每个包都必须在 Levels 登记。
// 否则 R1（分层方向）对它隐形——新增切片不登记就能绕过全部层级约束。

import (
	"strings"
	"testing"
)

// serverScopePrefix 是 server 作用域的包路径前缀。
const serverScopePrefix = modulePrefix + "pkg/server/"

// assemblyRootPkg 是装配根（与 assemblyRoot 同值；本门禁用精确匹配语义）。
const assemblyRootPkg = modulePrefix + "pkg/server"

// TestR6_ServerScopeDoesNotImportAssembly 断言 server 作用域子包不导入装配根，且全部登记层级。
func TestR6_ServerScopeDoesNotImportAssembly(t *testing.T) {
	t.Parallel()
	graph := importGraph(t)

	scoped := 0
	for pkg, imports := range graph {
		if !strings.HasPrefix(pkg, serverScopePrefix) {
			continue
		}
		scoped++
		if _, ok := Levels[pkg]; !ok {
			t.Errorf("R6：server 作用域包 %s 未在 Levels 登记——未登记包对 R1（分层方向）隐形，"+
				"新增切片必须显式声明层级", pkg)
		}
		for _, imp := range imports {
			if imp == assemblyRootPkg {
				t.Errorf("R6：server 作用域包 %s 导入了装配根 %s。"+
					"依赖方向必须单向（装配根 → 切片 → Global）；切片需要的能力应下沉到切片自己的包或 Global 包，"+
					"由装配根在构造时注入", pkg, imp)
			}
		}
	}

	// 覆盖探针：扫描面被改窄（前缀写错/图塌缩）时本门禁会静默空转。
	if scoped == 0 {
		t.Fatalf("R6：导入图中没有任何 %s* 包——门禁扫描面失效（前缀或导入图异常）", serverScopePrefix)
	}
}
