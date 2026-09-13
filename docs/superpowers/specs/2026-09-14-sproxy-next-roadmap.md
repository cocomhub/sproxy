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
| E 安全加固 | 分布式限流、密钥轮换编排、写面 TOCTOU 原子化、协议 fuzz 扩展 | 中 |
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

CHANGELOG 中的每个版本 `[X.Y.Z] - YYYY-MM-DD` 都要有对应 tag 才能让 compare 链接有效；
0.1.0–0.11.0 为**回溯建立**（根 tag 已创建并入远端，见 §2.4）；`cmd/*` 嵌套模块 tag 需在对应提交上补打
annotated tag（不可逆，需最终确认后推送）。

---

## 6. 验收标准

- `chore/oss-baseline` 分支：A/B 类死代码零残留；C 类逐项有明确处置记录；测试专用 helper 归位。
- `CHANGELOG.md` 版本号/日期/tag 链接自洽；`git tag` 与 CHANGELOG 版本一一对应。
- `release-please` 配置可产出 release PR；GoReleaser 在 tag 下可 dry-run 通过。
- 全量门禁绿：`gofmt -l` 无输出、`make lint` + `make lint-all` 0 issues、`go test ./pkg/... ./internal/...`、
  `make test-all`、`make check-ci`。
