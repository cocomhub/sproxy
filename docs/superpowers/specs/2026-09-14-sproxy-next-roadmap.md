> **历史归档（非权威）**：本文档记录*当时*的计划/结论，可能与当前实现不一致；
> 现行事实源：`AGENTS.md`、`README.md`、`docs/*.md` 与代码本身。

# sproxy 后续发展规划（2026-09-14）

> 本文档固化 2026-09-14 的现状盘点、遗留问题审计结论、发展方向与用户已拍板的发布决策。
> 实施计划见 `../plans/2026-09-14-oss-baseline-standardization.md`。

**状态：** 已确认（用户 2026-09-14 决策）
**分支：** `chore/oss-baseline`

---

## 1. 现状坐标

仓库规模：422 commits，近 30 天 133 commits；`master` 与 `origin/master` 同步；0 open issue；
12 个 Go module（根 + `cmd/sproxy` + `cmd/sclient` + 9 个子模块）；416 个测试文件。

### 1.1 已完成主线

| 主线 | 依据 | 状态 |
|------|------|------|
| 完全组网阶段 1–5 | `2026-08-29-sproxy-fullmesh-roadmap.md`、`2026-08-31-sproxy-fullmesh-stage5-design.md` | ✅ |
| 生产就绪（阶段 6） | `../plans/2026-09-04-stage6-production-readiness.md` | ✅ 基本完成 |
| 文件服务域化重构 | `2026-09-12-file-service-extraction-design.md` | ✅ |
| 远程访问面（Y 读+写） | `2026-09-13-remote-access-architecture-design.md` / `../plans/2026-09-13-remote-access-architecture.md` | ✅ |
| mesh 载体与可观测（S/W 系列） | PR #234–#248 | ✅ |

**结论：既定路线图已收口**，当前处于「上一轮大计划全部交付、下一轮尚未立项」的节点。

---

## 2. 遗留问题审计

### 2.1 死代码审计（取证方法与结论）

取证手段：
- `golang.org/x/tools/cmd/deadcode`（RTA，从 `cmd/sproxy` / `cmd/sclient` 反向可达）；
- 自写导出符号扫描（962 个导出 func/method/type/const/var，区分 prod / test 引用）；
- `git log -S/-G` 追调用点消失历史。

**分类结论：**

| 类别 | 定义 | 代表 | 处置（2026-09-14 已执行） |
|------|------|------|------|
| A 替代遗留 | 生产调用已被删除，函数与测试仍在 | `cmd/sclient/archive.go:writeArchiveResponse`、`batch.go:runBatchOperation`、`cloud_download.go:extractTarGz`、`cmd/sproxy/mesh_node.go:startMeshNodeRole` | **已删除**（任务 2；对应测试同步删/改，逐条披露） |
| B 陈旧死类型 | 特性已废除但注释仍在描述 | `pkg/server/handlers.go:TunnelUpdater` + `TunnelHandler()`、`pkg/tunnel/xfer/ext/grpc/grpc.go:XferServer` | **已删除**（任务 3）；失实注释同步订正 |
| C 便捷包装 | 已被更精确变体取代，但属公开 API | `hub.NewFederationClient`、`hub.ParseRegisterAck`、`tunnel.AccessKeyMesh`、`tunnel.NewHandler`、`client.CloudCreateGroup/CloudListGroups/DownloadItemsSequential` | `tunnel.NewHandler` **已删**（统一到 `NewLocalHandler`）；其余**保留**（薄委托/SDK 入口） |
| D 零引用访问器 | 只读小工具，零调用 | `AllowIP`、`AuthToken`、`Disabled`、`FileCount`、`MaxHops`、`HasService`、`ServiceHosts` | **保留**（公开 API） |
| E 反射/接口驱动 | 误报 | `MarshalJSON`/`UnmarshalJSON`、`MuxStreamAddr.Network()` | **禁止删** |
| F 测试基建 | 有意保留 | `pkg/testutil/**`、`mockxfer`、`mockdht`、`vaultmock`、`xfertest` | 保留；可重用者已归位 |

**实际删除清单（本轮）：** `writeArchiveResponse`、`runBatchOperation`、`extractTarGz`、`startMeshNodeRole`、
`TunnelUpdater`、`(*Handlers).TunnelHandler`、`Handler.UpdateKey`（空实现）、`tunnel.NewHandler`、`XferServer`。

**保留清单（本轮）：** `tunnel.AccessKeyMesh`（薄委托 `accesskey.ParseMesh`）、`hub.NewFederationClient`、
`hub.ParseRegisterAck`、`client.CloudCreateGroup`/`CloudListGroups`/`DownloadItemsSequential`/`WithStructCodec`/`WithOffset`/`ResetRunners`、
D 类零引用访问器、E 类反射/接口方法、F 类测试基建。

**测试工具归位（任务 5）：** `clientfactory` 的 mock 拆到 `cmd/sclient/internal/clientfactory/mock.go`；
`pkg/files` 跨包测试 helper 集中到 `pkg/files/testing_helpers.go`；门禁 R11 已登记进 learnings §5。

**证据（关键）：** commit `a1dc9aa4`（#90「清理死代码」）的 diff 明确删除了 `runBatchOperation` 的生产调用行
（`-results := runBatchOperation(args, ...)`）却保留了函数与测试；`extractTarGz` 的生产调用在 #106 消失；
`startMeshNodeRole` 自 S5(#240) 引入起从未接过线（`root.go` 直接用 `...WithCreds`）。

### 2.2 测试工具散落

生产文件中存在仅被测试引用的导出符号：
- `cmd/sclient/internal/clientfactory/mock.go` 的 `mockFactory`/`NewMock`（26 个测试文件在用；已从 `factory.go` 拆出）；
- `pkg/files/chunked_store.go` 的 `MustNewUploadStore`（`pkg/files` 与 `pkg/server` 测试共用）；
- `pkg/accesskey` 的 `NewRingFromKeyPairs`/`WithID`/`DeriveMasterKey`；
- 生产代码中的测试接缝 `SetHostOnly`/`SetMDNSLoopbackOnly`/`SetCandidatesForTest`/`SetClock`/`SetTTL`。

### 2.3 仓库卫生

- 本地残留分支 24 个、远端未合并分支 10 个（squash 合并后未删）；
- 4 个他人遗留 stash（硬规则禁用 `git stash pop`）；
- 2 个 dependabot PR 挂置（#148、#152）。

### 2.4 发布机制缺口

| # | 缺口 | 证据 |
|---|------|------|
| 1 | `go install .../cmd/sproxy@<tag>` 实际失效 | 根 tag 已回溯建到 `v0.11.0`，但缺 `cmd/sproxy/vX.Y.Z` 嵌套 tag；且 `go.mod` 用 `replace ../../` + `require v0.0.0` |
| 2 | CHANGELOG 双源漂移 | `CHANGELOG.md` 手工维护 vs GoReleaser 从 commit 生成 |
| 3 | 无版本/发布自动化 | 版本靠手打 tag |
| 4 | `draft: true` 需人工发布 | `.goreleaser.yaml:102` |
| 5 | `before.hooks` 会改源码 | `go mod tidy` + `go fmt ./...` |
| 6 | 嵌套模块 tag 缺失 | 根 tag 已回溯建到 `v0.11.0`（与 CHANGELOG 0.1.0–0.11.0 一致）；缺 `cmd/sproxy/vX.Y.Z` 与 `cmd/sclient/vX.Y.Z` |

---

## 3. 后续发展方向（候选池）

| 主线 | 内容 | 优先级 |
|------|------|--------|
| A 收尾与发布 | 死代码规范化、CHANGELOG 单源、tag 补齐、release 自动化 | **本轮** |
| B 生产运维闭环 | readiness/liveness 分离、指标告警与看板、配置热更新范围收敛、备份/恢复与升级迁移、审计导出 | 高 |
| C mesh 网络补全 | 透明网关（`remote://` 本地代理）、目录服务、用户级联邦、多跳 chained relay、规模化验证 | 中高 |
| D 存储数据面 | 卷迁移/再平衡、版本 GC、对象存储后端、分块会话幂等持久化 | 中 |
| E 安全加固 | 分布式限流、密钥轮换编排、写面 TOCTOU 原子化、协议 fuzz 扩展（2026-09-17：mux/tcp/tunnel 帧解析 fuzz 已落地，见 PR） | 中 |
| F 开发者体验 | 部署工件（Compose/Helm）、SDK 文档、Web UI gap 收口 | 中 |
| G 性能规模化 | benchmark 基线、100+ 节点、goroutine/内存收敛 | 中 |

---

## 4. 本轮已确认决策（用户 2026-09-14）

1. **分支**：新建 `chore/oss-baseline`，作为「标准化开源库基线」载板。
2. **死代码**：完全无效代码可以删除（A 类 + B 类中确认已废者）。
3. **重复实现**：存在不同实现的，评估统一为一份或适当重构（C 类逐个决策）。
4. **测试工具**：纯测试且可重用的，迁到 `pkg/testutil` 或合适的独立位置/文件。
5. **发展规划**：本文档保存，随代码 PR 一并提交（遵守「不单独开纯文档 PR」）。
6. **嵌套模块发布**：参考 CHANGELOG 版本生成对应 tag（含 `cmd/sproxy/vX.Y.Z`、`cmd/sclient/vX.Y.Z`）。
7. **发布自动化**：选**方案 B —— GoReleaser + release-please**。
8. **供应链安全**：暂不需要（不做 SBOM/签名/SLSA）。
9. **分发渠道**：暂不考虑（不做 Homebrew/Scoop/apt 仓库）。

---

## 5. 发布机制选型（方案 B 展开）

### 5.1 方案对比（决策依据，保留存档）

| 维度 | A 保守加固 | **B GoReleaser+release-please** | C +git-cliff | D 全栈供应链 |
|------|-----------|-------------------------------|--------------|-------------|
| 落地成本 | 0.5–1d | 1–2d | 1d | 3–5d |
| 解决 CHANGELOG 漂移 | ✗ | ✓ | ✓ | ✓ |
| 自动版本 PR（可审查） | ✗ | ✓ | ✗ | ✓ |
| 供应链安全 | ✗ | ✗ | ✗ | ✓ |
| 新增依赖 | 无 | release-please | git-cliff | syft/cosign/SLSA |

**选型：B。** 理由：本仓已强制 Conventional Commits（硬规则 9）与 squash 合并，release-please 落地摩擦最低；
由它独占 `CHANGELOG.md` 与版本号（单源），标签触发现有 GoReleaser 只负责构建与产物。

### 5.2 嵌套模块 tag 规则（Go 官方要求）

- 根 module `github.com/cocomhub/sproxy` → tag `vX.Y.Z`；
- 嵌套 module `github.com/cocomhub/sproxy/cmd/sproxy` → tag `cmd/sproxy/vX.Y.Z`；
- 嵌套 module `github.com/cocomhub/sproxy/cmd/sclient` → tag `cmd/sclient/vX.Y.Z`；
- 其余 `pkg/**` 子模块是否打 tag 由「是否对外独立发布」决定（本轮先只覆盖两个 main module）。

> Go 官方明确：`github.com/user/repo/moda` 的 tag 必须为 `moda/v1.2.3`，否则 `go get` 无法解析。

### 5.3 CHANGELOG 与 tag 的关系

CHANGELOG 中的每个版本 `[X.Y.Z] - YYYY-MM-DD` 都要有对应 tag 才能让 compare 链接有效。

**现状（2026-09-14）：** 根 tag `v0.1.0`–`v0.11.0`（annotated）已建立并已推送，CHANGELOG 各版本日期
与对应 tag 提交日期逐条一致；**嵌套 module tag（`cmd/sproxy/vX.Y.Z`、`cmd/sclient/vX.Y.Z`）尚不存在**，
由 `scripts/tag-release.sh` 生成：默认干跑；`--apply` 本地创建；`--apply --push` 显式推送（发布类不可逆，
需人工确认）。脚本优先取已存在根 tag 指向的提交，保证嵌套 tag 与根 tag 同源。
回归测试 `scripts/tag-release_test.sh`（`make test-tag-release`，已接入 CI Lint job）。

> 注：即使补了嵌套 tag，`go install github.com/cocomhub/sproxy/cmd/sproxy@<tag>` 仍**不可用**
> —— 嵌套 module 的 `go.mod` 带相对 `replace` + `require v0.0.0`，代理解析时会忽略 replace。
> 本轮只保证 tag 与 CHANGELOG 自洽，不承诺 `go install`（发布说明已相应订正）。

---

## 6. 验收标准

- `chore/oss-baseline` 分支：A/B 类死代码零残留；C 类逐项有明确处置记录；测试专用 helper 归位。
- `CHANGELOG.md` 版本号/日期/tag 链接自洽；`git tag` 与 CHANGELOG 版本一一对应。
- `release-please` 配置可产出 release PR；GoReleaser 在 tag 下可 dry-run 通过。
- 全量门禁绿：`gofmt -l` 无输出、`make lint` + `make lint-all` 0 issues、`go test ./pkg/... ./internal/...`、
  `make test-all`、`make check-ci`。

---

## 7. 执行结果与状态（2026-09-14 收尾）

### 7.1 标准化清单：全部收口

| 组 | 项 | 状态 |
|---|---|---|
| A 收尾与发布 | release-please 接入、GoReleaser v2 修复、嵌套 tag 脚本、`RELEASING.md` | ✅（#249 #250 #255 #256）；**发布动作**待用户决定 |
| B 门禁 | B1 死代码失败门禁、B2 覆盖率门禁 fail-closed、B4 固定等待条件化 + 棘轮、B5 CLI 文档、B6 R11 去 git 依赖 | ✅（#262 #263 + 本片；B4 余量 161 → 136） |
| C 卫生 | C1 分支/stash、C2 术语与零引用导出、C3 dependabot、C4 | C1 ✅（分支 26→2、stash 4→0）、C2 ✅（#264）、C3 **暂缓（用户明示）** |
| D 结构 | D1 cmd 薄层、D2 TODO/正确性、D3 超大文件 | D1 ✅（#257 #258 #261 #265；D1-e 判定不做）、D2 ✅（#259 #260）、D3 ✅（#266–#269） |
| E 仓库卫生 | CONTRIBUTING / SECURITY / README | ✅（#264，R16 守） |

### 7.2 顺带修掉的真实缺陷（均为用户可见）

1. `relay stats` 恒显示 0（解析 `node_count`，服务端发 `nodes_connected`）——#258
2. rename 的 TOCTOU 可绕过「目标已存在」409 门禁并**静默覆盖**目标——#259
3. `storage.AtomicRename` 慢路径**无条件删除目标**（Windows 实测数据丢失）——#259
4. `--hub wss://…` 派生出明文 `http://` ⇒ relay 管理命令全部失败——#261
5. `make cover-check` 在 Windows 上**恒 PASS**（依赖 `bc`）——#262

### 7.3 新增/强化的门禁

R13（门禁自身守卫）、R14（睡眠棘轮）、R15（CLI 文档漂移）、R16（仓库卫生）、B1 `make deadcode-check`（挂 CI Lint job）、B2 cover-check fail-closed、R11 改纯 Go 扫描（不再依赖 `.git`，且覆盖未跟踪文件）。**每条都做过变异验证**（注入退化 → 门禁必须报红）。

### 7.4 仍待办

- **发布**：一次性审校 release PR #252（补 `### Removed`、删 `fix(lint)` 噪声、核对版本）→ 合并 → `scripts/tag-release.sh --version X.Y.Z --apply --push` 补嵌套 tag。
- **依赖**：dependabot #148/#152（用户明示暂缓）。
- **远端分支**：`feature/goedel-go-optimize`（未合并，待裁决）。
- **超大文件**：未列入 D3 的 >1000 行生产文件（`syncmgr/manager.go` 1213、`client/chunked.go` 1209、`webrtc/webrtc.go` 1171、`files/chunked_upload.go` 1096、`files/chunked_store.go` 1089）——用户明示暂不拆。
