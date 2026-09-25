---
name: parallel-batch-implementation
description: >
  Use when executing a multi-PR feature roadmap in parallel batches — dispatching worker subagents across independent worktrees,
  coordinating merge order, handling subagent timeout/401/retry, enforcing rebase-before-PR and squash-merge discipline,
  triaging CI flake, and keeping design docs as the authority. Triggers: any "批次/并行实施/多条 PR 同时推进" task,
  roadmap S1-S5 分批, worker 子代理分派, 多 PR 并行开发, subagent 超时/401 重试, flake 记录与单 PR 修复。
---

# 并行批次实施（Parallel Batch Implementation）

> 在 roadmap 分批并行实施多条功能时使用。核心：**worktree 隔离 + worker 子代理分派 + 主 agent 只调配不亲做 + 单任务粒度 + PR/rebase/flake 纪律**。

## 概述

把 roadmap 拆成**独立任务**，每个任务一个 worktree + 一个 worker 子代理，并行推进；主 agent 只负责分派/调配/审查/合并，**绝不亲自执行真实任务**（否则失去并行性）。批量完成比串行快 3-5 倍，但每个环节都有已实证的坑。

## 核心模式

### 1. 任务分派模式（并发安全）

**已实证教训（批次 36-37）：**
- **并行 7 个子代理 → 6 个 401**（API 并发超配额：`{"error":{"code":"","message":"Invalid token (api_error)"}}`）——每批**只存活最后启动的 1 个**
- **对策**：① 一次只启 2-3 个；② 401 失败**立即 resume**（`subagent({action:"resume", id})` 保留上下文续跑，比重分派高效）；③ resume 交错进行（一次 1 个）
- **30min 超时是常态**：worker 子代理（deepseek-flash）在 30min 时限内通常无法完成「调研+实现+测试+提交」完整流程

```bash
# 建 worktree（必须基于最新 origin/master）
git fetch origin && git worktree add .worktrees/feat/<name> -b feat/<name> origin/master
# 写 TASK.md（需求唯一来源：背景/规格/实现要求/提交流程硬规则/报告契约）
# 分派 worker（cwd 指向 worktree；async:true；task 含 TASK.md 路径 + 仓库规则要点 + 回传契约）
```

**任务粒度铁律：**

| 粒度 | 结果 | 对策 |
|------|------|------|
| 单任务（1 项功能） | ✅ 超时前可完成（webhook-sign 实证） | 默认 |
| 2-4 项小功能合并 | ⚠️ 可能超时 | 拆开 |
| 4+ 项大任务（L3 原样） | ❌ 11h 无产出 | **必须拆分**后 interrupt + 重建 worktree |

**拆分模式**：原任务超时无产出 → `interrupt` → 检查 worktree 是否有未提交成果 → 按设计文档拆成 N 个独立 worktree + 单任务子代理。

### 2. Resume 续跑（保留成果）

子代理超时/401 后 worktree 里的未提交改动**不会丢**：

```bash
# 1. 检查成果
git -C .worktrees/feat/<name> status --porcelain | head -10
git -C .worktrees/feat/<name> log --oneline origin/master..HEAD
# 2. resume（保留上下文续跑；指示「直接产出代码不再探索」——超时常因过度调研）
subagent({action:"resume", id:"<run-id>", message:"继续实现…先 git status 确认改动在 → 直接产出 → 验证 → 提交 → push → gh pr create"})
```

**已实证**：resume 后指示「直接产出代码」→ oidc-ldap/file-tags/vol-retention 从 0 文件推进到完成（2 文件→15 文件→11 文件）。

### 3. 主 agent 收尾接管（子代理超时后的兜底）

子代理完成大部分但超时被杀（如 oidc-ldap 5 文件未提交）→ **主 agent 接管完成剩余**：
- 检查编译（`go build ./...` 报错即补：未限定类型加包前缀、go mod tidy、go.work 登记）
- 验证全绿（build + test -race + archcheck + golangci-lint + goimports）
- 更新 roadmap 标注 → 提交 → push → gh pr create

**主 agent 接管常见修复**：
- `routes.go: ExternalAuthHandler` 未限定 → 加 `authn.` 前缀（import 未用编译错）
- 新子 module go.mod 依赖全 indirect → `GOWORK=off go mod tidy` 升 direct
- **go.work 登记新子 module**（Makefile gofix-all 遍历 SUB_MODULE_DIRS 自动发现 go.mod，但 go.work 必须含它——注意改 worktree 内 go.work 而非主仓库）
- roadmap 对应项状态「缺」→「已落地」

### 4. PR 纪律（每次必做）

**创建 PR 前必须 rebase**（用户硬规则）：
```bash
git fetch origin && git rebase origin/master   # 冲突直接处理
# 冲突：git checkout --ours/theirs → git add → GIT_EDITOR=true git rebase --continue → git push --force
git push -u origin feat/<name>
gh pr create --base master --title "type(scope): 功能描述" --body "交付/改动要点/验证证据（功能维度）"
```

**squash 合并 body 按功能维度**（非简单罗列 commit msg）：
```bash
gh pr merge <N> --squash --delete-branch --subject "feat(scope): 能力描述 (#N)" \
  --body "roadmap X.Y 落地：<功能>。<实现要点>。TDD + 变异命中。验证：<证据> + CI <N>/<N>。"
```

**push 失败特征**：输出 "and the repository exists" = 未成功 → `git push --force` 重试 → `git ls-remote` 验证本地=远端。

**基建改动合入后**（如 ci.yml 镜像替换）：所有旧分支 PR 必须 rebase，否则旧 CI 配置继续失败。

### 5. CI 轮询与 flake 治理

**轮询纪律**（用户硬规则）：
- **小间隔频繁检查**（60-90s），不长时间 sleep 硬等
- **已终止状态立即 break**（success/failure/cancelled），不等到循环超时
- **达标条件**：`fail=0 && pending=0`（不依赖固定数量）

**flake 处理流程**（用户硬规则）：
1. **先判断是否 flake**：未改动文件的既有测试 / 基建（Docker pull unauthorized、Azure blob BlobNotFound 404）/ 平台时序（windows 差异）/ 阈值边缘
2. **记录待办**（scratchpad add：测试名 + PR 号 + 特征）
3. **rerun job**：`gh api -X POST repos/{o}/{r}/actions/jobs/<id>/rerun`
4. **高概率/积累多个 → 单 PR 集中修复**（不单独开 PR）

### 6. 文档即接口（设计文档权威）

- **设计文档是权威**：TASK.md 明确「实现必须完全遵循设计文档」；子代理对任务范围与设计文档冲突时**以设计文档为准**（TASK 中陈旧提法忽略并在 report 说明）
- **roadmap 状态同步**：每个 PR 完成必须更新 docs/roadmap.md 对应项「缺/待设计」→「已落地（#PR）」
- **62 份设计文档全在 master docs/designs/**（批次 37 前），子代理直接读

## 常见缺陷模式（已实证）

| 缺陷 | 症状 | 对策 |
|------|------|------|
| 子代理自合并 PR | 子代理超出 TASK 范围自行 squash | TASK 明确「主 agent 会合并」；发现后确认结果正确性 |
| 空 Handlers nil channel | `close(h.uploadingStop)` 对空结构 panic | 判 nil 守卫（`if h.uploadingStop != nil`） |
| goroutine 回落误报 | TestRunServer_SignalShutdown 30s 超时 | 测试改**自身基线**（before + limit）而非全局 limit |
| ETXTBSY | sclient upgrade 原子替换后立即 fork | 替换后加短暂重试/延迟 |
| 401 超配额 | 并行 7 子代理 6 个 Invalid token | 交错分派（一次 2-3）+ resume |
| interactive rebase 卡住 | 子代理 rebase 中途超时被杀 | 主 agent 接管：`git rebase --continue` 续跑 |
| go.work 改错位置 | 主仓库 go.work 找不到 worktree 内子 module | 改 **worktree 内** go.work |

## 红线

- 并行 7+ 子代理一次全启（401 超配额）
- 4+ 项大任务不拆分就分派
- 创建 PR 不 rebase origin/master
- squash body 只罗列 commit msg
- flake 不记录待办就直接 rerun 了事
- 主 agent 亲自执行真实任务（失去并行性）

## 快速参考

```bash
# 分派
git fetch origin && git worktree add .worktrees/feat/<n> -b feat/<n> origin/master
subagent({agent:"worker", async:true, cwd:"<worktree>", task:"读 TASK.md → 实现 → 验证 → rebase → push → PR → REPORT.md"})

# 超时处理
git -C .worktrees/feat/<n> status --porcelain   # 查成果
subagent({action:"resume", id:"<id>", message:"直接产出代码不再探索 → 完成"})

# 合并（自查关键点后）
gh pr merge <N> --squash --delete-branch --subject "feat(s): 能力 (#N)" --body "<功能维度>"

# flake
gh api -X POST repos/cocomhub/sproxy/actions/jobs/<id>/rerun
scratchpad add "flake：<test>（<PR> 偶现，未改该文件）——积累后单 PR"

# 收尾
git worktree remove .worktrees/feat/<n> --force && git branch -D feat/<n> && git remote prune origin
```
