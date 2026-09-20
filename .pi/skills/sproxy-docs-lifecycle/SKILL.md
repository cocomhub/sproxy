---
name: sproxy-docs-lifecycle
description: >
  sproxy 文档生命周期纪律：设计文档（specs/plans/designs）→ 权威文档收敛 → 归档精华。
  在以下场景使用：任何包含文档产出（设计/计划/规格）的功能开发、PR 合并前文档完整性审核、
  文档清理/整理任务、docs/ 结构变动。覆盖「设计文档何时写、何时并入权威文档、何时归档/删除、
  squash 合并前如何审核文档」的全流程约束。
---

# sproxy 文档生命周期纪律

> 单一事实源：`docs/README.md`（文档索引）、`docs/` 根级权威文档（api / architecture / cli / config /
> deploy / tunnel / mesh-testing / glossary）、`docs/archive/`（经验归档）。
> 本 skill 是把「文档管理与 PR 流程」绑定的可执行约束，防「设计文档与实现脱节、过期内容误导」类问题。

## 一、文档分层（哪些是现行事实源，哪些是过程产物）

| 层 | 路径 | 状态 | 何时更新 |
|----|------|------|----------|
| **权威文档** | `docs/api.md` `docs/architecture.md` `docs/cli.md` `docs/config.md` `docs/deploy.md` `docs/tunnel.md` `docs/mesh-testing.md` `docs/glossary.md` `docs/README.md` | 现行事实源 | **功能实现合并时同步更新**（门禁 R9/R15 防漂移） |
| **活跃规则** | `docs/archive/agent-operating-rules.md` `docs/archive/ci-merge-process.md` `docs/archive/benchmark-ci-timeout-disk-io.md` `docs/archive/gofix-before-pr.md`（2026-09-20 自 learnings 移入并归档，被 AGENTS.md/Makefile/ci.yml/门禁硬引用）| 现行事实源 | 规则变更时 |
| **过程产物** | `docs/superpowers/{specs,plans,designs}`（历史设计/计划/规格） | **已废弃**（2026-09-19 #389 全部删除，git 历史可回溯）| 不再产生新文件 |
| **经验归档** | `docs/archive/`（code-review / mesh-evolution / architecture-decisions / benchmark-ci）| 精华沉淀，非事实源 | 实现落地后把跨时间有效的教训补入 |
| **测试台账** | `docs/testing/virtual-time-conversions.md` | 门禁 R14 引用 | 串行测试登记时 |

## 二、设计文档生命周期（核心流程）

```
功能立项 → 设计文档（如有）→ 实现 → 功能合并（PR squash）→ 权威文档收敛 → 精华归档
              │                        │                    │
              │  （可跨 PR 的多阶段任务）│                    │
              └──→ 每片合并前：把已完成部分并入权威文档 ──→ 最终收敛/归档
```

### 1. 设计文档可以写，但必须有「收敛计划」
- 新功能设计/计划/规格**可以**写（git 跟踪的 `docs/superpowers/{specs,plans,designs}/` 已删除——**新设计写在哪**）：
  - 简单功能：**不写设计文档**，直接实现 + 在 commit message / PR body 说明设计决策；
  - 复杂功能：设计文档放**分支内**（如 `docs/designs/2026-XX-XX-<feature>.md` 或 PR 的临时目录），
    **合并前必须并入权威文档或归档精华**，不留过程产物在 master。

### 2. 每次 PR 合并前：设计文档 → 权威文档收敛（硬性）
- **功能实现合入前**，把该功能涉及的接口/配置/行为变更写入对应权威文档：
  - 新 HTTP 端点 → `docs/api.md`
  - 新 CLI 子命令/flag → `docs/cli.md`
  - 新配置项/语义 → `docs/config.md`
  - 架构/协议改动 → `docs/architecture.md` / `docs/tunnel.md`
  - 部署方式 → `docs/deploy.md`
- **收敛动作与代码同 PR**（不是合并后才补）——避免「代码合了文档没跟上」的窗口。

### 3. squash 合并前：文档完整性/正确性审核（硬性，合并前哨兵）
合并 PR 前，除 CI 全绿 + 分支最后 commit 审核外，**必须过一遍文档审核**：

```bash
# 1. 代码中新增的导出符号/路由/配置，权威文档是否覆盖？
git diff origin/master..HEAD --stat | grep -E "\.go$" | awk '{print $2}'   # 改动文件
grep -rn "HandleFunc\|/api/" pkg/server/routes.go | head                   # 新路由 → 查 api.md
grep -rn "Flags().Bool\|Flags().String" cmd/sclient/*.go | head            # 新 flag → 查 cli.md
grep -rn "cfg\." pkg/server/config.go | head                               # 新配置 → 查 config.md

# 2. 本次 PR 是否引用了已删除的文档路径？（断链检查）
git grep -n "docs/superpowers/plans\|docs/superpowers/specs" HEAD -- ':!docs/archive' || echo "无断链"

# 3. 权威文档内是否有「已删除/已移除」内容的残留引用？
grep -n "已废除\|已移除\|deprecated" docs/cli.md docs/config.md | head
```

**审核清单（合并前逐项过）**：
- [ ] 新路由/新 flag/新配置项 → 已在对应权威文档出现（R15 只守 root persistent flags，子命令局部 flag 需人工核对）
- [ ] 无对已删除文档路径的引用（`docs/superpowers/plans|specs|designs`）
- [ ] 权威文档中无「描述已移除功能」的残留（如已废弃端点、已删除配置键）
- [ ] 文档引用的代码符号/命令/端点都存在（文档漂移反向检查）
- [ ] 若本次功能含破坏性变更 → cli.md/config.md 同步标注（与 release-please 的 BREAKING 一致）

### 4. 多阶段任务（跨 PR）：允许，但最终必须收敛
- 多阶段功能允许**跨 PR 分片实现**，但**每片合并前**必须把该片已完成部分并入权威文档
  （不能攒到最后一片才补文档——中间片合并后文档即滞后）；
- 最终一片合并时：设计文档的**精华**（方法论/安全边界/取舍标准/用户决策结论）→ 补入 `docs/archive/` 对应主题；
  **过程细节**（逐任务 checkbox、审查统计、交接简报）→ 不保留（git 历史可回溯）。

### 5. 文档清理/整理任务的判据（什么可以删）
- **权威文档未覆盖 + 无跨时间价值** → 删除（git 历史可恢复）；
- **权威文档已覆盖**（功能/配置面）→ 删除，不重复保留；
- **有跨时间价值的方法论/教训** → 提炼进 `docs/archive/` 后删除；
- **被 AGENTS.md/Makefile/ci.yml 硬引用** → 保留原路径（移动会断链）；
- 删除前用 `git grep` 全仓查引用，被引用的先改指向或确认可删。

## 三、文档维护硬性要求（写代码时同步做）

1. **改动 API/配置/CLI 行为必须同步更新对应权威文档**（门禁 R9「权威文档不得引用已移除产物」+ R15「root persistent flag 必须在 cli.md」兜底；子命令局部 flag 靠人工核对）
2. **新 JS 文件登记进 Makefile `web-test`**（R10 门禁）；Web UI 改动必须带 node 单测 + Playwright e2e
3. **删除对外 API 用 `remove(scope): ...` 提交类型**（release-please 映射 `### Removed`）
4. **不写「历史归档（非权威）」头**：被删的过程文档统一以 git 历史为准，不再保留过时副本

## 四、权威文档内容准则

- `docs/README.md` 是索引，不重复正文内容；新增功能文档先想是否应并入现有主题而非单开文件
- `docs/archive/` 主题文件（code-review / mesh-evolution / architecture-decisions / benchmark-ci）：
  补内容时**按功能维度**追加，不按日期堆砌；标注「历史判断可能已演进」（如过度设计结论被 mesh 推翻）
- 架构决策记录在代码注释 + commit message（`docs/archive/architecture-decisions.md` 只留方法论与已执行收敛）
- 文档与代码一致性用 grep 核对（flag/端点/配置键），不靠「我记得写了」

## 五、检查时机速查

| 时机 | 动作 |
|------|------|
| 功能开发前 | 判断是否需要设计文档（简单功能不写；复杂功能分支内写 + 收敛计划） |
| 每片合并前 | 该片已完成功能 → 并入权威文档（与代码同 PR） |
| squash 合并前 | 文档审核清单逐项过（§3 审核清单）+ CI 全绿 + 分支最后 commit 审核 |
| 功能最终落地 | 精华 → `docs/archive/`；过程产物不留 master |
| 文档清理任务 | 按 §2.5 判据逐文件核对（权威覆盖/硬引用/精华提炼） |

## 参考文件

- `docs/README.md`（文档索引与维护约定）
- `docs/archive/`（经验归档：code-review / mesh-evolution / architecture-decisions / benchmark-ci）
- `internal/archcheck/docs_rules_test.go`（R9 门禁：权威文档不得引用已移除产物）
- `internal/archcheck/docs_cli_flags_test.go`（R15 门禁：root persistent flags 必须文档化）
- 记忆锚点：[[docs-lifecycle]]（PR #389 docs 清理 / #390 cli 补文档 的落地教训）
