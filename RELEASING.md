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
   `chore`/`docs`/`ci`/`test`/`build`/`style` **不产生 release PR**。
2. **release-please 开/更新 release PR**（分支 `release-please--branches--master`，标题 `chore: release master`）。
   同一 component/branch **永远只有一个** release PR；后续可发布提交会更新它，不会再开第二个。
3. **审校 release PR**（人工把关点）：
   - 版本号是否符合预期（只有 `fix` ⇒ patch；含 `feat` ⇒ minor；含 `!`/`BREAKING CHANGE:` ⇒ 按 `bump-minor-pre-major` 规则）；
   - 条目是否覆盖全部面向用户的变更（`chore` 类改动**不会**出现——内容重要就应改用 `feat`/`fix`/`remove`）；
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

## 注意事项

- **`CHANGELOG.md` 不得有 `## [Unreleased]` 段**（门禁 R12 会拦）。原因：release-please 用
  `DEFAULT_VERSION_HEADER_REGEX = '\n###? v?[0-9[]'` 找**第一个版本标题**作插入锚点，而 `## [Unreleased]`
  因 `[` 恰好命中 ⇒ 新版本段会被插到它**上面**，且它**从不被消费/清理**（写进去的内容永远不会进入任何版本）。
- **不要手工改 release PR 里的 `CHANGELOG.md` 后放着不管**：若有新的可发布提交落地，release-please 会
  **重建该 PR 分支并覆盖**手改内容；发布前必须核对版本段条目仍完整。
- 删除对外 API 请用 `remove(<scope>): ...` 提交类型（已映射到 `### Removed`），避免依赖人工补条目。
- 发布 PR 的标题/结构若需调整（如 `pull-request-title-pattern`），**必须在一个 release PR 合并之后**再改——
  标题不匹配会诱使 release-please 再开一个重复 PR。
- `scripts/tag-release.sh` 只支持 `X.Y.Z`；预发布版本（`-rc.N`）不会自动生成 tag（脚本会提示）。
