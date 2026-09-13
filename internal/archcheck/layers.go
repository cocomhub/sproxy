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
	"github.com/cocomhub/sproxy/pkg/syncmgr":          true,
	"github.com/cocomhub/sproxy/pkg/downloader":       true,
}

// Levels 是包 → 层级（数字越小越底层）。L(n) 不得导入 L(>n)。
//
// **全表化**（2026-09，server 域抽取 S1）：原表只登记 5 个 Managed 包 + 4 个基础包，
// 于是 R1 只在它们之间生效——像「pkg/syncexec 反向依赖 pkg/server」这类倒置，
// 只要两端都没登记就对 R1 隐形。现在**全部顶层 pkg/* 包**都登记，分组按当前依赖图的
// **深度**导出（G(n) = 最深依赖链长度），已逐条验证零违规。
//
// 分组的语义（读表时的心智模型）：
//
//	G0 基础库      零 pkg/* 内部依赖（叶子）
//	G1 领域包      只依赖 G0
//	G2 装配层      client / server（可导入 G0/G1 与子包）
//	G3 装配之上的消费者（pkg/sync 依赖 pkg/client）
//	G4 更上层消费者（pkg/syncexec 依赖 pkg/sync）
//
// 表是**顶层包的冻结契约**：新增顶层包必须登记（R3 也会强制 Managed 包这么做）；
// 确有正当理由的跨组新边，改表并在提交说明里写明理由即可。
var Levels = map[string]int{
	// ---- G0 基础库（零 pkg/* 内部依赖，实测）----
	"github.com/cocomhub/sproxy/pkg/accesskey":     0,
	"github.com/cocomhub/sproxy/pkg/certmgr":       0,
	"github.com/cocomhub/sproxy/pkg/checksum":      0,
	"github.com/cocomhub/sproxy/pkg/cli":           0,
	"github.com/cocomhub/sproxy/pkg/cloudfilename": 0,
	"github.com/cocomhub/sproxy/pkg/iostream":      0,
	"github.com/cocomhub/sproxy/pkg/otp":           0,
	"github.com/cocomhub/sproxy/pkg/pathguard":     0,
	"github.com/cocomhub/sproxy/pkg/plugin":        0,
	"github.com/cocomhub/sproxy/pkg/provider":      0,
	"github.com/cocomhub/sproxy/pkg/quota":         0,
	"github.com/cocomhub/sproxy/pkg/sproxysig":     0,
	"github.com/cocomhub/sproxy/pkg/storage":       0,
	"github.com/cocomhub/sproxy/pkg/store":         0,
	"github.com/cocomhub/sproxy/pkg/telemetry":     0,
	"github.com/cocomhub/sproxy/pkg/testutil":      0,
	"github.com/cocomhub/sproxy/pkg/volume":        0,
	// 本工作新增（server 域抽取 S1）：同步任务管理器，从 pkg/server 的子包提升为顶层。
	// 零 pkg/* 内部依赖（实测），故 G0；提升的理由见
	// docs/superpowers/specs/2026-09-13-server-domain-extraction-design.md §3。
	"github.com/cocomhub/sproxy/pkg/syncmgr": 0,

	// ---- G1 领域包 ----
	// 本工作新增（文件服务**领域包**，非子包——故不写 ParentDomain）：只 import 下层
	// **顶层包**（pathguard/checksum/storage/quota/volume）⇒ G1。
	// **不含** volume/registry、storage/capacity 等子包——R2（子包可见性）禁止跨域直连子包，
	// 卷集合与容量核算经领域自定义窄接口（files.VolumeSet / files.StorageManager）由装配层
	// 注入（见 pkg/files/service.go）。此处若被"补回"子包依赖，门禁 R2 会报红。
	"github.com/cocomhub/sproxy/pkg/files":  1,
	"github.com/cocomhub/sproxy/pkg/socks5": 1,
	"github.com/cocomhub/sproxy/pkg/tunnel": 1,

	// ---- G2 装配层 ----
	"github.com/cocomhub/sproxy/pkg/client": 2,
	"github.com/cocomhub/sproxy/pkg/server": 2,

	// ---- 子包（L 取父域之上，由依赖实测确定）----
	// storage 域子包：构造形参接收 checksum.ChecksumStoreIface ⇒ 在 checksum 之上
	"github.com/cocomhub/sproxy/pkg/storage/capacity": 2,
	// volume 域子包：构造形参接收 pkg/volume 域类型、持有 storage/quota 句柄
	"github.com/cocomhub/sproxy/pkg/volume/registry": 2,

	// ---- G3 / G4：装配层之上的消费者 ----
	"github.com/cocomhub/sproxy/pkg/sync":     3,
	"github.com/cocomhub/sproxy/pkg/syncexec": 4,
}

// ParentDomain 声明子包 → 父域包。子包只允许父域子树与装配层导入。
// 装配层是必要例外：路由注册在 pkg/server，它必须引用子包的处理器。
// 随各片 PR 增量登记（当前两项均为 P6 判据下的「真子领域」：可复用的卷集合/容量核算，
// 消费者均为装配层与各自父域）。
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
