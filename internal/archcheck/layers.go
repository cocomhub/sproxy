// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package archcheck 是分层与包可见性的可执行门禁：把「文件服务抽取」设计
// （docs/superpowers/specs/2026-09-12-file-service-extraction-design.md §3.3）
// 中约定的层级关系变成断言，而不是文档里的一句话。
//
// 新增包必须登记进 Levels，否则校验失败——这是刻意的：门禁的价值就在于
// 强迫作者显式声明新包在依赖图中的位置。
package archcheck

// Managed 是本工作新增（抽取）的包。R3 只作用于它们——若把仓库存量包也纳入
// 「必须登记依赖」的范围，登记一个包就会拖出它整条子图（pkg/tunnel →
// xfer / mux / hub / …），门禁根本落不了地。
var Managed = map[string]bool{
	"github.com/cocomhub/sproxy/pkg/pathguard":        true,
	"github.com/cocomhub/sproxy/pkg/checksum":         true,
	"github.com/cocomhub/sproxy/pkg/storage/capacity": true,
	"github.com/cocomhub/sproxy/pkg/volume/registry":  true,
	"github.com/cocomhub/sproxy/pkg/files":            true,
}

// Levels 是包 → 层级（数字越小越底层）。L(n) 不得导入 L(>n)。
// 表内既含 Managed 的新包，也含它们依赖的存量基础包——后者必须显式登记，
// 才能让「新包依赖了哪一层」有据可查。存量基础包统一记 L1：本工作不引入
// 它们之间的方向约束（它们彼此的历史依赖不在本计划范围内）。
var Levels = map[string]int{
	// 本工作新增
	"github.com/cocomhub/sproxy/pkg/pathguard": 0,
	"github.com/cocomhub/sproxy/pkg/checksum":  0,
	// 本工作新增（storage 域子包）：构造形参接收 checksum.ChecksumStoreIface ⇒ 在 L0 之上
	"github.com/cocomhub/sproxy/pkg/storage/capacity": 2,
	// 本工作新增（volume 域子包）：构造形参接收 pkg/volume 域类型、持有 storage/quota 句柄
	// ⇒ 在 L1 之上
	"github.com/cocomhub/sproxy/pkg/volume/registry": 2,
	// 本工作新增（文件服务**领域根**，非子包——故不写 ParentDomain）：其 handler 消费
	// 下层**顶层包**（pathguard/checksum/storage/quota/volume）⇒ 在最上层。**不含**
	// volume/registry 等子包——R2（子包可见性）禁止跨域直连子包，卷集合经领域自定义窄
	// 接口 files.VolumeSet 由装配层注入（见 pkg/files/service.go）。此处若被"补回"
	// volume/registry 依赖，说明窄接口约定被破坏，门禁 R2 会报红。
	"github.com/cocomhub/sproxy/pkg/files": 4,
	// 存量基础包（新包的依赖；pkg/tunnel 由 volumes.go 实测依赖）
	"github.com/cocomhub/sproxy/pkg/storage": 1,
	"github.com/cocomhub/sproxy/pkg/quota":   1,
	"github.com/cocomhub/sproxy/pkg/volume":  1,
	"github.com/cocomhub/sproxy/pkg/tunnel":  1,
}

// ParentDomain 声明子包 → 父域包。子包只允许父域子树与装配层导入。
// 装配层是必要例外：路由注册在 pkg/server，它必须引用子包的处理器。
// 随各片 PR 增量登记，例如 pkg/files/chunked → pkg/files。
var ParentDomain = map[string]string{
	"github.com/cocomhub/sproxy/pkg/storage/capacity": "github.com/cocomhub/sproxy/pkg/storage",
	"github.com/cocomhub/sproxy/pkg/volume/registry":  "github.com/cocomhub/sproxy/pkg/volume",
}

// AssemblyPackages 是允许导入任意子包的装配层（前缀匹配）。
//
// **作用域边界**：导入图由 `go list ./...` 在**根 module** 目录下解析得到，
// 只含根 module 的包——`cmd/sproxy`、`cmd/sclient`、`pkg/tunnel/xfer/ext/*`、
// `pkg/tunnel/hub/ext/kad` 等**子 module 的包不在图中**（实测图中
// `cocomhub/sproxy/cmd/` 的包数为 0）。因此下面的 `".../cmd/"` 条目**当前是
// 空转项、不产生任何约束**；保留它是为了将来子 module 若并入图时自动生效——
// 删掉的话，那一天会得到一个费解的假红（cmd 下的装配代码被判为非法导入者），
// 而保留的成本只是一行注释。
var AssemblyPackages = []string{
	"github.com/cocomhub/sproxy/pkg/server",
	"github.com/cocomhub/sproxy/pkg/client",
	"github.com/cocomhub/sproxy/cmd/",
}
