<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 发布流程（RELEASING）

`CHANGELOG.md` 与版本号由 **release-please** 生成（单一事实源：
`release-please-config.json` + `.github/workflows/release-please.yml`）。**不要手工维护 CHANGELOG。**

## 一次发布的完整步骤

1. **累积可发布提交**：合并到 `master` 的提交里必须至少有一个可发布类型
   （`feat` → Added / `fix` → Fixed / `perf`·`refactor`·`deps` → Changed /
   `remove` → Removed / `deprecate` → Deprecated / `security` → Security）。
   `chore`/`docs`/`ci`/`test`/`build`/`style` **也会**进 CHANGELOG（统一落在 `### Changed`
   段）——`release-please-config.json` 已移除这几类的 `hidden` 标记，Conventional 类型
   全枚举 ⇒ 没有任何提交会从 CHANGELOG 消失。
2. **release-please 开/更新 release PR**（分支 `release-please--branches--master`，标题 `chore: release master`）。
   同一 component/branch **永远只有一个** release PR；后续可发布提交会更新它，不会再开第二个。
3. **审校 release PR**（人工把关点）：
   - 版本号是否符合预期（只有 `fix` ⇒ patch；含 `feat` ⇒ minor；含 `!`/`BREAKING CHANGE:` ⇒ 按 `bump-minor-pre-major` 规则）；
   - 条目是否覆盖全部变更（本仓已开启全类型可见：`chore`/`test`/`docs` 等也会出现在
     `### Changed`——若某条不该出现，应改用更贴切的类型或在 release PR 里删除该条）；
   - 该版本段是否缺 `### Removed`/`### Deprecated` 等只有人工能补的条目（历史遗留的 `chore` 型删除只能手工补）。
4. **等 CI 全绿**：release PR 会触发 CI（它改了 `.release-please-manifest.json`，不在 `paths-ignore` 内）。
5. **合并 release PR** ⇒ release-please 打 tag `vX.Y.Z` 并建 GitHub Release，随后经 `workflow_call` 触发
   `release.yml`，由 GoReleaser 产出二进制/deb/rpm/镜像（`release.mode: keep-existing`，不覆盖 release notes）。
6. **补嵌套模块 tag**（本仓特有，**不可逆，需人工确认**）：

   ```bash
   scripts/tag-release.sh --version <X.Y.Z>          # 干跑，核对将创建的 tag 与目标提交
   scripts/tag-release.sh --version <X.Y.Z> --apply --push
   ```

   Go 官方要求嵌套 module 的 tag 形如 `cmd/sproxy/vX.Y.Z`（`cmd/sclient` 同理）；根 tag 由 release-please 建，
   脚本对已存在的 tag 一律 SKIP、且嵌套 tag 与根 tag **同源**。
7. **验证制品**：GitHub Release 上有各平台归档 + `checksums.txt`；ghcr 镜像已推送。

## 提交信息规范：**scope 必填**（硬规则，2026-09-15 起）

```text
type(scope): 用户可读的能力描述        # 正确
type(scope)!: 破坏性变更说明            # 破坏性加 !
type: 描述                             # ✗ 会被 .githooks/commit-msg 拒绝
```

- **为什么强制 scope**：本仓 squash 合并按**分支提交信息**生成 CHANGELOG 条目；
  缺 scope 会产出无维度条目（如 `* 全量刷新 md ...`，而同段是 `* **docs:** ...`），
  可读性与检索性都受损（PR #275 即因此污染 0.11.1 段，已在 release PR 手工补正）。
- **执行点**：`.githooks/commit-msg`（`make githooks` 通过 `core.hooksPath` 安装）。
  允许的 type：`feat fix perf refactor deps revert security remove deprecate docs chore ci test build style`；
  `Merge*/Revert*/fixup!/squash!` 与 release-please 的 `chore: release master` 自动豁免。
- **squash PR 特别注意**：squash 取的是**分支提交信息**，所以**开 PR 前的分支提交**就必须带 scope——
  事后改 PR 标题**不会**修好它。合并前用 `git log -1 --format=%s origin/<分支>..<分支>` 核对。
- 补救：若 release PR 里已出现无 scope 条目，可在该 release PR 中把它补成 `* **scope:** ...`
  （只此一次、改完立刻合并——见下节「手改只在发布时刻做一次」）。

## 版本号怎么涨（release-please 机制）

配置：`release-please-config.json`（`release-type: go`、`bump-minor-pre-major: true`、
`include-v-in-tag: true`、`changelog-path: CHANGELOG.md`、`component: sproxy`）。

| 时机 | 结果 |
| --- | --- |
| 任何可发布提交合入 `master` | release-please 开/更新**唯一** release PR（分支 `release-please--branches--master`） |
| 又有新提交合入 | 同一 PR 被**强制更新**（分支 rebase/重建）——**手改内容会被覆盖** |
| 合并 release PR | 打 tag `vX.Y.Z`、建 GitHub Release，并 `workflow_call` 触发发布工作流 |

`bump-minor-pre-major: true` 表示 **1.0.0 之前**（当前阶段）的版本涨法：

| 本段含有的提交类型 | 版本变化 | 例 |
| --- | --- | --- |
| 仅 `fix` / `perf` / `refactor` / `deps` / `docs` / `chore` / `ci` / `test` / `build` / `style` / `remove` / `deprecate` / `security` | **patch** | `0.11.0` → `0.11.1` |
| 任一 `feat`（新增能力） | **minor** | `0.11.1` → `0.12.0` |
| 任一破坏性变更（`type(scope)!:` 或正文 `BREAKING CHANGE:`） | **pre-1.0 时也只升 minor**（`bump-minor-pre-major` 的效果）；**≥1.0.0 后**才升 major | `0.11.1` → `0.12.0`；`1.2.3` → `2.0.0` |

> **何时会出现 `v0.11.xx → v0.12.0`**：只要从 `v0.11.x` 那个 tag 之后到 release PR 合并前，
> `master` 上落过**任意一个 `feat(...)` 提交**（或带 `!` 的破坏性提交），就会是 minor 提升；
> 只有「全是 fix/refactor/docs/chore 一类」时才是 patch（如本次 0.11.1）。

**CHANGELOG 段落映射**（`changelog-sections`，全部类型均已枚举、**无 hidden**——即任何提交都会出现在
CHANGELOG 里，不会静默消失）：

| 提交类型 | 段落 |
| --- | --- |
| `feat` | ### Added |
| `fix` / `revert` | ### Fixed |
| `remove` | ### Removed |
| `deprecate` | ### Deprecated |
| `security` | ### Security |
| `perf` / `refactor` / `deps` / `docs` / `chore` / `ci` / `test` / `build` / `style` | ### Changed |

**还需要人工补条目的情形**：只有当某次改动**无法用上述类型表达**时才需要（在**发布时刻**一次性补进
release PR 的版本段并立刻合并）。经 0.11.1 逐条核验：若所有删除/移除都用了 `fix`/`refactor` 等
**已枚举类型**，或删除发生在**内部实现**（非对外 API），则**无需**手补 `### Removed`——门禁
`TestReleasePRChangelogEntriesHaveScope` 与「全类型可见」配置共同保证「有改动必然出现在 CHANGELOG」。

## 制品与 GoReleaser

二进制 / deb / rpm / 容器镜像由 GoReleaser 产出（`.goreleaser.yaml`），入口是 `Release` workflow
（`.github/workflows/release.yml`，触发：`push: tags` / `workflow_call`（release-please 打 tag 后复用）/ `workflow_dispatch`）。

**编译前置（硬要求）**：`internal/buildmeta` 用 `//go:embed build/dirty_info.txt` 内嵌构建元信息，而该文件被
`.gitignore` 忽略、由 `make prepare` 生成。因此**任何编译入口都必须先产出它**：

- Makefile：`prepare` 无条件生成该 embed 副本（含 `SKIP_VERSION=true`）；所有会编译/类型检查本仓模块的目标
  都显式依赖 `prepare`（`build` / `build-ci` / `test` / `test-all` / `build-all` / `vet` / `lint*` /
  `test-packages` / `archcheck` / `deadcode(-check)` / `bench*` / `build-%`）。
- GoReleaser**不走 Makefile**，故 `.goreleaser.yaml` 里以 `before.hooks: [make prepare]` 兜底。
- 门禁：`internal/archcheck/makefile_bench_deps_test.go`（同时校验上面两条）。

> 事故记录（同源两次）：CI `bench` 缺 `prepare` ⇒ Benchmark 表现为「超时被 cancel」；v0.11.1 首发时
> `Release` run 34958665107 报 `internal/buildmeta/buildmeta.go:14:12: pattern build/dirty_info.txt:
> no matching files found` ⇒ 发布失败、制品缺失（tag 与 Release 已存在但无 Asset）。

**本地预演**（发布前建议跑一次，7 秒左右）：

```bash
goreleaser build --snapshot --clean --single-target   # 会先跑 before hook（make prepare）
```

**失败补跑**：若某 tag 的 `Release` run 失败，**不要用 `gh run rerun`**——rerun 复用的是那次 run
自带的旧 workflow 定义；应先把修复合入 master，再在 master 上手动 dispatch（workflow 定义取 master，
checkout/构建仍取输入的 tag）：

```bash
gh workflow run release.yml -f tag=v0.11.1
```

## 注意事项

- **`CHANGELOG.md` 不得有 `## [Unreleased]` 段**（门禁 R12 会拦）。原因：release-please 用
  `DEFAULT_VERSION_HEADER_REGEX = '\n###? v?[0-9[]'` 找**第一个版本标题**作插入锚点，而 `## [Unreleased]`
  因 `[` 恰好命中 ⇒ 新版本段会被插到它**上面**，且它**从不被消费/清理**（写进去的内容永远不会进入任何版本）。
- **不要手工改 release PR 里的 `CHANGELOG.md` 后放着不管**：若有新的可发布提交落地，release-please 会
  **重建该 PR 分支并覆盖**手改内容；发布前必须核对版本段条目仍完整。
- **手改 release PR 只在「准备发布的那一刻」做一次**（补 `### Removed` 之类的人工条目、删除噪声条目），
  然后立刻合并；不要提前多天手改——期间的任何可发布提交都会把它抹掉（已实测）。
- **squash 合并用的是「分支 commit 信息」，不是 PR 标题**（本仓实测）：分支上最后一个 commit 的 subject
  就是 master 上的提交信息，并会被 release-please 当成 changelog 条目。合并前务必确认它是想要的
  Conventional Commit subject（本次把 `fix(lint): …` 写进去，就给 release notes 混入了内部门禁修复的噪声条目）。
- 删除对外 API 请用 `remove(<scope>): ...` 提交类型（已映射到 `### Removed`），避免依赖人工补条目。
- 发布 PR 的标题/结构若需调整（如 `pull-request-title-pattern`），**必须在一个 release PR 合并之后**再改——
  标题不匹配会诱使 release-please 再开一个重复 PR。
- `scripts/tag-release.sh` 只支持 `X.Y.Z`；预发布版本（`-rc.N`）不会自动生成 tag（脚本会提示）。
- **`pull-request-title-pattern` 必须含 `${version}`**（现为 `chore(release): ${component} v${version}`，门禁
  `TestReleasePleasePRTitlePatternCarriesVersion` 拦）：缺版本号的标题（旧值渲染成 `chore: release master`）
  会让 release-please 在下次运行时无法把「已合并的 release PR」与版本关联，日志报
  `pullRequestTitlePattern miss the part of '${version}'` + `There are untagged, merged release PRs
  outstanding - aborting` ⇒ **既不建 tag 也不建 Release**（v0.11.1 实测）；此时需手动补 tag + Release，
  并把该 PR 的 `autorelease: pending` 标签改为 `autorelease: tagged`。
