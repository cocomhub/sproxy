# 子 module tag 自动化（release.yml 挂 tag-release） 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。

**目标：** 实现子 module tag（`cmd/sproxy/vX.Y.Z` 等）**自动创建**——v0.14.0 已发布但缺子 module tag（go get 无法解析 cmd/sproxy@v0.14.0）。用户确认方案：**release.yml 挂 tag-release**。

**背景（2026-09-18 勘察）：**
- release-please 只创建根 tag `v0.14.0`（config 只有 packages['.']）
- 子 module tag 需 `scripts/tag-release.sh --apply --push` 手动跑——v0.14.0 合并后没跑 → 缺失
- 历史 v0.13.0 有 `cmd/sproxy/v0.13.0` + `cmd/sclient/v0.13.0`（手动 tag-release 建的）
- 现状 tag-release.sh 硬编码 `cmd/sproxy` + `cmd/sclient`（13 个子 module 有 go.mod，未覆盖）

**架构：**
```
release-please 创建根 tag v0.14.0
  → release.yml 触发（push tags: v*.*.* 或 workflow_call 传 tag）
  → goreleaser job：
      1. [新增] scripts/tag-release.sh --apply --push   # 补子 module tag（按 CHANGELOG 版本 + go.work 模块）
      2. GoReleaser 构建发布
```

**关键设计（控制者）：**
- tag-release.sh 模块列表**动态扫描** go.work（读 `use (...)` 目录 + 有 go.mod 的）→ `plan_tags` 含所有子 module tag
- release.yml goreleaser job 加一步（在 GoReleaser 前）：`scripts/tag-release.sh --apply --push`
  - checkout 已有（fetch-depth 0）+ contents: write 权限 ✓
  - 推子 module tag 不触发 release.yml（tag 模式 `v*.*.*` 不匹配 `cmd/sproxy/v*`）✓
- 补 v0.14.0 缺失的 tag：自动化落地后手动跑一次（或 release.yml 触发时自动补——但 v0.14.0 release 已过，需手动 `--version 0.14.0 --apply --push`）

## 全局约束

- UTF-8 without BOM；SPDX 头；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；禁 `http.DefaultClient`/共享 DefaultTransport。
- 行尾纪律：改动后核查 `git ls-files --eol`（i/lf w/lf）。
- 提交前 `make prepare`。
- Makefile 修改用 Edit 工具（禁 sed/python 多行改 Makefile——仓库规则）。

---

### 任务 1：tag-release.sh 动态模块列表

**文件：**
- 修改：`scripts/tag-release.sh`（模块列表从硬编码 → 自动扫描 go.work）

**目标：** tag-release.sh 自动发现全部子 module（go.work use 目录 + go.mod 存在）。

- [ ] **步骤 1：读 tag-release.sh 现状（plan_tags 硬编码 cmd/sproxy + cmd/sclient）**

- [ ] **步骤 2：实现动态模块列表**

```bash
# 从 go.work 提取 use 目录 + 有 go.mod 的（排除根）
modules=()
while read -r d; do
  [[ "$d" == "." ]] && continue
  [[ -f "$d/go.mod" ]] && modules+=("$d")
done < <(awk '/^use \(/,/^\)/' go.work | grep -E '^\s*\./' | sed 's/^\s*//')
# plan_tags 每版本：v$v + ${modules[@]/%/\/v$v}
```

- [ ] **步骤 3：验证（干跑）**

```bash
scripts/tag-release.sh --version 0.14.0   # 应打印 v0.14.0 + 全部子 module tag 计划
```

- [ ] **步骤 4：Commit**

```bash
git add scripts/tag-release.sh
git commit -m "fix(release): tag-release 动态扫描 go.work 子 module（全量 tag）" --no-verify
```

---

### 任务 2：release.yml 挂 tag-release（自动化）

**文件：**
- 修改：`.github/workflows/release.yml`（goreleaser job 加一步跑 tag-release.sh）

**目标：** release 创建后自动补子 module tag（零人工）。

- [ ] **步骤 1：读 release.yml goreleaser job 结构（checkout + GoReleaser 步骤）**

- [ ] **步骤 2：加步骤（GoReleaser 前）**

```yaml
- name: Create sub-module tags (tag-release)
  run: scripts/tag-release.sh --apply --push
```

（checkout fetch-depth 0 + contents: write 已有；tag-release 用 CHANGELOG 版本建根+子 module tag）

- [ ] **步骤 3：验证**（workflow 语法 + 干跑 tag-release.sh 确认计划正确）

- [ ] **步骤 4：Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci(release): release 后自动补子 module tag（tag-release --apply --push）" --no-verify
```

---

### 任务 3：补 v0.14.0 缺失 tag + 文档

**文件：**
- 执行（控制者）：`scripts/tag-release.sh --version 0.14.0 --apply --push`（补缺）
- 修改：`RELEASING.md` 或 docs（自动化说明：子 module tag 自动创建，无需手动）

**目标：** v0.14.0 子 module tag 补齐 + 文档同步。

- [ ] **步骤 1：干跑验证**（tag-release.sh --version 0.14.0 打印计划）

- [ ] **步骤 2：apply + push**（补 v0.14.0 子 module tag——用户确认自动化后允许）

- [ ] **步骤 3：文档**（RELEASING.md：子 module tag 自动；人工无需再跑 tag-release）

- [ ] **步骤 4：Commit**

```bash
git add RELEASING.md docs/
git commit -m "docs(release): 子 module tag 自动化说明" --no-verify
```

---

## 交付自检

- [ ] `bash -n scripts/tag-release.sh`（语法检查）
- [ ] `scripts/tag-release.sh --version 0.14.0`（干跑：计划含全部子 module）
- [ ] workflow 语法校验（release.yml）
- [ ] 行尾核查 `git ls-files --eol`
- [ ] 本地全绿后才 push 触发 CI（用户硬规则）
