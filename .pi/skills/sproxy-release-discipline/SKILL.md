---
name: sproxy-release-discipline
description: >
  sproxy 提交信息与 release-please 发布纪律。在任何 commit / squash 合并 / release PR 审校 / 版本号校验 /
  破坏性变更（BREAKING CHANGE）标注或补救 / CHANGELOG 生成时使用。覆盖正确格式（type(scope)!: 而非 feat!(scope)）、
  BREAKING CHANGE footer 写法、BEGIN_COMMIT_OVERRIDE 补救、squash 信息源、RELEASING.md 核心约束。
---

# sproxy 提交与发布纪律（release-please）

> 单一事实源：`RELEASING.md`（完整）、`release-please-config.json`、`.githooks/commit-msg`（强制校验）。
> 本 skill 是这些内容的可执行速查，专防「格式错误导致 changelog 失效 / 版本号漏标」类事故。

## ⚠️ 最关键的一条：破坏性变更格式（2026-09-19 实证）

**`feat!(scope): ...` 是错误格式** —— `!` 必须放在 scope 之后、冒号之前：

| 格式 | 结果 |
| --- | --- |
| `feat(api)!: 删除旧端点` | ✅ 正确。release-please 解析出 `breaking-change` 节点 → CHANGELOG `⚠ BREAKING CHANGES` + 版本升 minor（pre-1.0） |
| `feat!(api): 删除旧端点` | ❌ **解析直接抛错**（`unexpected token '('`），commit 无法被识别 → 不进 changelog、不升版本 |
| `feat(api): 重构\n\nBREAKING CHANGE: 说明` | ✅ 正确（body/footer 写法） |

**实验证据**（@conventional-commits/parser 0.4.1，release-please 17.11.2 依赖同款解析器）：
- `feat(api)!:` → AST 含 `breaking-change` 节点，`type=feat, scope=api, breaking=true` ✓
- `feat!(api):` → `unexpected token '(' at 1:6, valid tokens [(, :]`（解析失败）✗
- `feat!:`（无 scope）→ 解析成功且 breaking=true，但**本仓 scope 必填**，仍会被 commit-msg hook 拒绝

本仓 `.githooks/commit-msg` 正则 `^(${ALLOWED})\([a-z0-9_,.-]+\)!?: .+` 只接受 `type(scope)!:`——`feat!(api):` 会被 hook 直接拦截。**hook 是通过的格式就是 release-please 认可的格式。**

## 破坏性变更的两种正确写法

```bash
# A. 类型后缀感叹号（推荐，最简洁；scope 后、冒号前）
git commit -m "feat(api)!: 删除旧端点"

# B. 提交体 BREAKING CHANGE footer（可带分条描述）
git commit -m "feat(api): 重构端点

BREAKING CHANGE: /api/v1/legacy 删除，迁移到 /api/v2"
```

- 两种写法都会：CHANGELOG 生成 `### ⚠ BREAKING CHANGES` 段 + 版本号按破坏性规则涨。
- 破坏性变更**清单**分条写进 `BREAKING CHANGE:` 段落（与设计文档 §8 一致）。
- 一个 squash commit 可含**多个 footer**（feat + fix + breaking 组合）。

## 版本号影响（release-please 机制）

| 时机 | 结果 |
| --- | --- |
| 仅 `fix`/`perf`/`refactor`/`deps`/`docs`/`chore` 等（无 feat、无 !） | **patch**（0.11.0 → 0.11.1） |
| 任一 `feat` | **minor**（0.11.1 → 0.12.0） |
| 任一破坏性变更（`!` 或 `BREAKING CHANGE:`）+ `bump-minor-pre-major: true` | **pre-1.0 只升 minor**；**≥1.0.0 升 major**（1.2.3 → 2.0.0） |

配置：`release-please-config.json`（`release-type: go`、`bump-minor-pre-major: true`、
`include-v-in-tag: true`、`changelog-path: CHANGELOG.md`）。

## ⚠️ 版本未发布前的变更不算破坏性变更（2026-09-20 用户确认，长期有效）

**仓库尚未发布含该功能的版本（线上无人使用）时，接口/协议改动不标注 BREAKING CHANGE**：

- 提交信息类型照常用（`feat`/`fix` 等），**不需要 `!` 或 `BREAKING CHANGE:` footer**
- release-please changelog 正常生成（无破坏性段、版本号不额外升）
- 判定：以「该功能是否已随某个已发布版本上线」为准——未发布 = 改任意接口都算内部演进

**实例（sproxy 2026-09-20）**：mesh 端到端加密 + SmartDial 竞速修正（T1-T7）均未发布（无含此功能的线上版本），
即使改动了协议/接口也照常 `feat(mesh)` 提交、不进 BREAKING CHANGES 段。

## 新增安全开关必须显式 pinning / 显式开关（2026-09-20 用户确认，长期有效）

**安全功能的启用/配置必须有显式开关或显式指纹 pinning，禁止「静默默认启用后悄悄降级」**：

- 安全功能要么明确配置（显式 pinning / 显式开关），要么明确不启用（可观察）
- **禁止**自动启用后因条件不满足悄悄降级成明文/弱模式而用户无感知
- 判定：任何安全开关的生效状态必须**可观测**（日志/告警/metrics），未生效要能发现

**实例（sproxy 2026-09-20）**：mesh 端到端加密默认启用（自动身份，ECDH 防窃听）+ 显式指纹 pinning（防 MITM）——
若接线因条件不满足未启用，必须有日志/告警表明「未启用」，不得静默走明文。

**CHANGELOG 段落映射**（全类型枚举、无 hidden——任何提交都会进 changelog）：

| 类型 | 段落 |
| --- | --- |
| `feat` | ### Added |
| `fix` / `revert` | ### Fixed |
| `remove` | ### Removed |
| `deprecate` | ### Deprecated |
| `security` | ### Security |
| `perf`/`refactor`/`deps`/`docs`/`chore`/`ci`/`test`/`build`/`style` | ### Changed |
| 任一破坏性 | ### ⚠ BREAKING CHANGES |

## squash 合并时的关键坑（本仓实测）

1. **squash 信息源 = 分支最后一个 commit**（不是 PR 标题）——破坏性标记必须写在**分支最后 commit 的 subject 或 body**里；事后改 PR 标题**不会**修好它。
2. 用 `gh api -X PUT pulls/N/merge -f commit_title="..." -f commit_message="..."` 显式传入时，以传入内容为准。
3. **合并前核对**：
   ```bash
   git log -1 --format=%s origin/<分支>..<分支>    # subject 是否含 (scope)!: 或 (scope):
   git log -1 --format=%B origin/<分支>..<分支>    # body 是否有 BREAKING CHANGE: 分条
   git show -s --format=%B HEAD | grep -i co-authored   # 必须为空（禁 Co-authored-by）
   ```
4. squash title 必须保留 `(#PR号)` 后缀（否则 changelog 条目无 PR 链接，#356-#363 批量中招）。

## 遗漏破坏性变更后的补救：BEGIN_COMMIT_OVERRIDE（2026-09-19 PR #381 实测）

**机制**：release-please 修正**已合并 PR** 提交信息的标准手段——不 rewrite 历史、不 force-push，read-time 替换。

**操作**：编辑已合并 PR 的 body，末尾追加：

```
BEGIN_COMMIT_OVERRIDE
feat(api)!: 正确标题 (#381)

BREAKING CHANGE: 破坏性变更清单（分条列出）
END_COMMIT_OVERRIDE
```

release-please 下次运行（lookback 窗口内）用块内内容**替代实际 squash commit 信息** → 版本 bump + changelog 按 override 生成。

**注意点（实测教训）**：
1. **必须带 `(#PR号)` 后缀**——release-please 的 changelog 链接/版本追踪依赖它，遗漏则条目缺 PR 索引。
2. 适用场景：合并时忘了 `!`/`BREAKING CHANGE:`（semver 漏标）、commit 类型/scope 写错、subject 不达意。
3. **仅 squash 合并有效**（plain merge 无法对应到 commit）。
4. **时效性**：必须在 release-please 生成 release PR / 打 tag 之前用——已发布（tag 已打）则来不及改版本号。
5. 属「合并后纠错」通道；**质量仍以「合并前确认分支最后 commit」为主**（先做对，补救是兜底）。

## 提交流程速查

```bash
# 提交前
export PATH="$PATH:$(go env GOPATH)/bin"    # hook 需要 golangci-lint/addlicense
git config user.name && git config user.email   # 必须 suixibing <suixibing@gmail.com>
git add <本任务文件>                        # 禁 -A / 禁 .
git commit -m "feat(scope): 用户可读描述"     # 禁 --no-verify（pre-commit 全量门禁）

# 合并前
gh pr checks <N> --watch                    # total≥14 且 pending=0（7 项必检）
git log -1 --format=%B origin/<分支>..<分支>  # 核最后 commit：scope 正确 + 无 Co-authored-by
gh pr merge <N> --squash --delete-branch     # 或 gh api -X PUT pulls/N/merge（title 带 (#N)）

# 合并后
git pr-clean <N>                             # worktree remove + branch -D + remote prune
git fetch origin && git reset --hard origin/master
```

## 其它 release-please 硬约束

- **`CHANGELOG.md` 不得有 `## [Unreleased]` 段**（门禁 R12 拦；release-please 把它当锚点、不消费）。
- **手改 release PR 只在「准备发布那一刻」做一次**，然后立刻合并——期间的任何可发布提交都会抹掉手改。
- **不要手工维护 CHANGELOG**：单一事实源 = `release-please-config.json` + `.github/workflows/release-please.yml`。
- 删除对外 API 用 `remove(scope): ...` 类型（映射 `### Removed`），避免依赖人工补条目。
- 版本号由提交类型决定（只有 fix ⇒ patch；含 feat ⇒ minor；含 !/BREAKING ⇒ 按 bump-minor-pre-major）。

## 参考文件

- `RELEASING.md`（完整发布流程与版本表）
- `release-please-config.json`（changelog-sections / bump 策略）
- `.githooks/commit-msg`（强制 scope 与格式的正则）
- 记忆锚点：[[squash-merge-discipline]]（全局 MEMORY.md）、[[release-please-behavior]]（项目 MEMORY.md）
