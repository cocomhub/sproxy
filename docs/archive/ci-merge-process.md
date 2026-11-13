# CI 与合并流程（2026-09-13 订正）

适用范围：sproxy 仓库所有 PR。本文记录**已实测验证**的流程事实与规则，避免重复踩坑。

## 1. `master` 有 **active ruleset** 必检 7 项 ⇒ `--auto` **会** gate CI（纯文档 PR 例外）

事实（2026-09-13 复核）：

- `gh api repos/cocomhub/sproxy/branches/master/protection` → **404**：**没有** classic branch protection；
- 但 `gh api repos/cocomhub/sproxy/rulesets` → **有** active ruleset（`master branch`, id `17891054`，
  created `2026-06-19`），规则含 `pull_request` / `required_status_checks` / `deletion` / `non_fast_forward`；
- 必检项（2026-09-15 复核，7 条）：`Test (Go 1.26, ubuntu, +Vault)`、`Test (Go 1.26, windows)`、
  `E2E (real binaries) (ubuntu-latest)`、`E2E (real binaries) (windows-latest)`、
  `Test Sub-Modules (cmd + ext + hub + mesh)`、`UI E2E Tests`、`SonarQube`；`required_approving_review_count = 0`；
  (`strict_required_status_checks_policy = false`)。
  > **2026-09-15 订正**：名单里的 `Benchmark` **已被 `SonarQube` 取代**（ruleset `updated_at` 2026-09-13T21:30Z 之后
  `gh api repos/cocomhub/sproxy/rulesets/17891054 --jq '.rules[] | select(.type=="required_status_checks")'`
  已不含 `Benchmark`）⇒ **Benchmark job 变红/超时不再阻塞合并**，但仓库轮询规则仍要求全绿。
  Benchmark 的 flake 根因与修复见 `2026-09-15-benchmark-ci-timeout-disk-io.md`。
- **当天时间线**：该 ruleset 的必检项是 `2026-09-13T06:49Z` 才配置好的，而 #214 在 `06:26Z` 合并
  （早于配置完成）——**这就是当时观察到「提前合并」的原因**，不代表 `--auto` 现在不 gate。

**规则**：

1. 有代码改动的 PR：轮询 `gh pr checks <PR>` 直到**总数 ≥ 14 且 `pending=0`**，再 `gh pr merge --squash`
   （此时 `--auto` 也等效，因为它会等到 7 项必检全绿；两种写法都可，但**必须**确认没有红的必检项）。
2. **纯文档 PR**（只改 `paths-ignore` 命中的路径，见下）：CI **永不触发** ⇒ 7 项必检**永不报绿** ⇒
   `mergeStateStatus` 恒为 `BLOCKED`。此时**只能**用 `gh pr merge <PR> --squash --admin`（admin bypass），
   这是唯一应当使用 `--admin` 的场景；合并前用 `gh run list --branch <branch>`（应为空）+
   `git diff --name-only origin/master...<branch>`（应全部落在忽略路径）确认「CI 未触发」而非「检查未挂上」。

`paths-ignore`（`.github/workflows/ci.yml`）：`*.md`、`docs/**`、`CHANGELOG.md`、`.gitignore`、
`.editorconfig`、`.notestignore`。

### 1.1 `gh pr checks` 空输出的两分

- **检查尚未挂上**（刚推送 / run 被 cancel）：等下一轮轮询；
- **CI 根本没触发**（纯文档 PR）：按上文用 `--admin` 合并，不必空等（实测空等 25 分钟仍无 check）。

## 2. Benchmark 已改为手动触发（web 页面），不再阻塞 PR

**2026-09-22 变更**：Benchmark job 从 `.github/workflows/ci.yml` 拆出为**手动触发**的独立
workflow（`.github/workflows/benchmark-manual.yml`，web 页面 Actions → Benchmark (manual) → Run workflow）。

- PR 检查**不再包含** Benchmark ⇒ 每个 PR 不再因 Benchmark 的 runner I/O 塌陷 / 卡死而失败阻塞
  （历史上它**不在** master ruleset 必检内，却仍以「每 PR 必跑 + 偶发变红需 rerun」的方式拖慢合并）；
- 需要跑基准（性能回归检查 / 更新基线）时：维护者在 Actions 页手动触发本 workflow
  （默认 master；可在 "Use workflow from" 选择任意分支）。
- 手动 job 的 `timeout-minutes: 15`（手动场景无需人工轮询，超时即结束，状态 cancelled），
  其余步骤（make bench → bench-gate → benchmark-action 图表推送 → artifact 保留）与旧 job 逐字一致。

卡死自愈兜底仍生效：`make bench` 经 tools/benchwatch（进程外看门狗）运行，停滞会在 ~2 分钟内
**以非零码失败**（而非被 job 级 timeout-minutes 静默取消）⇒ 诊断与 artifact 得以保留。

**规则（手动触发场景）**：单个 Benchmark run 超过 **15 分钟**仍未完成 → 取消该 run，**只重跑失败的 job**：

```bash
github action run 页面 → 对 benchmark-manual workflow 的失败 run 点 Re-run failed jobs
```

保留 REST cancel 路径（runner 排队异常/其它 job 也卡住时主动干预）：

```bash
gh api -X POST repos/cocomhub/sproxy/actions/runs/<run-id>/cancel
```

要点（**2026-09-13 按用户要求修正，手动触发场景同用**）：

- GitHub **没有 job 级 cancel API**（只有 `POST .../jobs/<id>/rerun`）⇒ 想停一个卡死的 job 只能
  取消整个 run；取消会把**正在跑**的其它 job 也标为 cancelled，而 rerun failed 正好只重跑这些
  真正失败的 job，**已成功的不动**；
- rerun 会生成**新的 job id**（`run_attempt + 1`）⇒ 每轮必须动态取 job id，不可缓存旧 id；
- `gh run cancel <run-id>` 曾返回 `HTTP 500`；改用 REST cancel（`gh api -X POST .../cancel`）更可靠。

现象（历史，PR 自动触发时代的根因）：`Benchmark` job（`make bench`）偶发长时间卡在 `in_progress`
（实测 30~40 分钟），而本地同命令全绿（`go test -bench=. -benchmem -count=5 -run=^$ ./...`）⇒ 当时判定为 runner 争用。

> **2026-09-15 订正（根因已查明）**：不是单纯的 runner 争用，而是 **benchmark 夹具把 runner 磁盘写带宽
> 放进了计时路径**（`pkg/client` 每趟写 ~2.5 GiB → 触发脏页回写节流后单次 op 从 5 ms 变成 6.5 s）。
> 取证、机制与修复见 `2026-09-15-benchmark-ci-timeout-disk-io.md`；判断「是不是同类卡死」直接复用该文 §1 的判据。

## 3. 合并后删除分支

> 注：纯文档 PR（见第 1 节）无需等 CI，但**仍需**删分支。

本仓**不会**自动删除已合并 PR 的 head 分支。`gh pr merge <PR> --squash` 之后立即清理：

```bash
git push https://github.com/cocomhub/sproxy.git --delete <branch>   # 远端
git branch -D <branch>                                              # 本地
```

## 4. 推送一律走 https

SSH 在本机不可用（`git@github.com: Permission denied (publickey)`）：

```bash
git push https://github.com/cocomhub/sproxy.git HEAD:refs/heads/<branch>
```

> 注：本机 SSH 已验证可用（2026-09-21：`ssh -T git@github.com` 成功），https/SSH 均可。

## 6. GoReleaser 发布：嵌套 module tag 必须全忽略（#413 事故，2026-09-21）

**事故**：v0.17.0 发布失败——GoReleaser 报 `failed to parse tag 'web/e2e/v0.17.0' as semver`，
Release 页只有 release-please 正文、无制品。

**根因**（两层叠加）：

1. `scripts/tag-release.sh`（#404 引入）发布时自动补建全部子 module tag（按 `go.work use`
   动态扫描）；v0.16.0 成功是因为当时子 module tag 尚不存在（#404 是发布后手动补建的），
   v0.17.0 是第一个「子 module tag 已存在」的发布；
2. `.goreleaser.yaml` 的 `git.ignore_tags` 只排除 `cmd/*`——GoReleaser 用
   `git describe --tags --abbrev=0 --exclude=...`（v2.18 源码确认 getTag/gitTagsPointingAt
   三候选），`web/e2e/*`、`pkg/*` 等前缀未被忽略 → 命中最近嵌套 tag → semver 解析失败。

**修复**（#413）：

- `.goreleaser.yaml` `git.ignore_tags` 扩为 `cmd/*`、`pkg/*`、`web/*`（覆盖 `go.work use`
  全部子 module 前缀，与 tag-release.sh 动态扫描同源；**新增子 module 须同步补**）；
- 门禁 `internal/archcheck/build_flags_alignment_test.go` 补断言三前缀必须在 ignore_tags
  （缺失即红），防未来漏配；
- 重跑 Release workflow 前**先把 v0.17.0 tag 重指向 master**（见下）。

**关键认知**：

- **workflow_dispatch 触发 Release 时 checkout 的是 tag 自身 commit**——tag 若不指向最新
  master，release.yml 里的 tag-release.sh 与 goreleaser 读到的都是旧 commit（第一次重跑仍
  失败即因此）；tag 重指向（旧 commit 是 master 祖先，安全；release-please 不会重建已存在
  tag）后成功，制品 11 个全上传。
- `gh pr merge --squash` 遇「Head branch is out of date」且分支基于 tag commit 时，实际是
  分支落后 base——重 push 分支 → reopen → 重新 CI → 合并。
- tag-release.sh `--push` 在子 module tag 已存在时推送失败为**警告级不阻塞**（远端已存在
  无需推）。

**下次发布验证**：单触发源（`on.push tags`）+ ignore_tags 生效（`previous=web/e2e/v0.16.0`
仍会显示，但 `current=v0.17.0` 正确）。

- **不要用 `git stash`**：仓库中存在其他分支遗留的 stash，`git stash pop` 会弹出**别人的** WIP
  （已发生两次，造成 `UU` 冲突）⇒ 改用临时副本，避免 stash。
- 提交前必须 `export PATH="$PATH:$(go env GOPATH)/bin"`（pre-commit 需要 `golangci-lint` / `addlicense`）。
- 新建分支前先 `git fetch && git checkout master && git reset --hard origin/master`
  （曾因本地基座落后导致 add/add 冲突、且 **CI 未触发**）。
- Windows 下 `python` 看到的 `/tmp` 与 git-bash 的 `/tmp` **不是同一目录**；跨工具传递文件用仓库内相对路径。

## CI 完全不触发：先查「合并关系 / 三方 diff」

> 实测沉淀（2026-11-12，sproxy #1119/#1120）：**PR 存在但 CI 一个 run 都不出现**时，
> 根因往往不是 workflow 配置，而是 **head 与 base 的合并关系异常**。

### 机制
`ci.yml` 的 `pull_request.paths-ignore` 判定依赖**三方合并后的变更文件集合**（需 merge-base）。
分支落后 / 存在冲突 / 该计算退化时，GitHub 拿不到可靠的变更集合 ⇒ **整个 workflow 被跳过**
⇒ 表现为「PR 在，CI 却完全不存在」。

### 判别（关键信号）
**PR 的 `changed files` 混入「base 已包含」的文件** → 高度可疑。
（本次实测：PR 实际只含 4 个文件，GitHub 却列出 11 个，多出的正是 base 已合并的文件。）

### 无效 vs 有效手段（逐项实测）
| 手段 | 结果 |
| --- | --- |
| `gh pr close` → `reopen` | ✗ 无效 |
| 空提交（`--allow-empty`）push | ✗ 无效 |
| 换新分支 + 新 PR | ✗ 无效（diff 同样异常）|
| **`git fetch && git rebase origin/master` + `git push --force-with-lease`** | ✅ **立即触发** |

⇒ 前三者都**没有改变 head 相对 base 的合并关系**；**只有重算合并 diff 才有效**。
force push 后 `changed files` 也会一并刷新为正确值（本次从 11 → 4）。

### 硬规则：**每次 push 前先 `git rebase origin/master`**
```bash
git fetch origin
git rebase origin/master          # 有冲突则解决（分析 master 差异，勿破坏本分支功能目的）
git push --force-with-lease       # 重写 head SHA ⇒ 触发 pull_request: synchronize
```
同一根因还解释另外两类失败：**必检项莫名失败**（落后导致缺 master 侧修复）、
**PR diff 失真**（混入上游文件）。⇒ 先 rebase 再判断是否真 flake。

排查顺序：① 看 `changed files` 是否混入 base 文件 → ② rebase 最新 base → ③ force push → ④ 复查 `gh run list -b <branch>`。

### 勘误与补充（2026-11-13）：根因是 **base 分支**，不是 `paths-ignore` 语义

上节把「PR 的 `changed files` 混入 base 已有文件」列为**信号**是对的，但**机制归因**曾出错，特此修正：

1. **`paths-ignore` 的语义是「全部改动文件都匹配才跳过」**（与 GitHub 文档措辞 "all" 一致），
   **不是**「任一匹配即跳过」。证据（`gh run list -b <branch>`）：
   - #1070 / #1041（**纯文档**，各 1 个 `docs/*.md`）⇒ **无 `CI` run** ⇒ 跳过 ✓
   - #1077（`docs/*.md` **+** `pkg/.../*_test.go`，**混合**）⇒ **有 `CI success`** ⇒ **不跳过** ✓
   ⇒ 所以"文档搭在代码 PR 里"**不会**让 CI 漏跑 ⇒ `*.md` / `docs/**` **可以**安全放进 `paths-ignore`。
2. **`branches: [master]` 是真凶**：PR 的 **base 指向功能分支**时，
   `pull_request.branches: [master]` **不匹配** ⇒ 整个 workflow **不派发**（CI 一个 run 都没有）。
   #1092（含 1 个 docs 的 33 文件 PR）当时"CI 完全不触发"即属此类，与 `paths-ignore` 无关。
3. **处置**：
   - 创建 PR 时 **`--base master` 必须显式指定**（`gh pr create --base master` 或建 PR 后核对 `baseRefName`）；
   - 已恢复 `*.md` / `docs/**` 到 `ci.yml` 的 **`pull_request.paths-ignore`**（纯文档 PR 重新可秒合并）；
     但 **`push:`（master）段故意不含它们** —— 混合 PR 合并后的那次 push 必须跑全量（最后一道防线）。

### 验证记录（2026-11-13）
恢复 `*.md`/`docs/**` 后（#1124 合并，master `24c43a295`），本 PR **只改 docs/** ⇒ 用于实测：
应只有 `CI (docs-only)` + `PR title check`（**无 `CI` run**）⇒ 即「纯文档 PR 秒合并」恢复。

## 并行 PR 的三条硬教训（2026-11-13 实测沉淀）

> 背景：一次流水线并行推进 8 个 PR（用户要求「不等 CI 连续执行」）时连续踩到三个坑。
> 它们**都可预防**，以下为口径。

### ① **相关功能不要拆过细**（用户明示原则，最根本的一条）
**「不要拆分过细，相关功能一起 PR 合并即可，减少冲突。」**

实测代价：`#1135`（并发闸）/ `#1136`（L1a 配额）/ `#1137`（L1b 可配置）**三改同一文件**
（`pkg/volume/secretdata/secretdata.go` 的字段区与初始化区）⇒ `#1135` 一合并，另两个**立即
`CONFLICTING`**、`gh pr merge` 失败 ⇒ **每个都要单独 rebase 解冲突**。

⇒ 口径：
- **同族改动（同文件 / 同模块 / 同一能力链）⇒ 一个 PR 一次做完**（代码 + 测试 + 装配接线 + 文档）；
  冲突在**提交时**就解决了，不会等到合并才爆；
- **只有真正无关**（不同文件 / 不同能力、无共享冲突面）才并行多 PR；
- 这与既有规则一致：**不拆小 PR**。

### ② **合并前也要 rebase**（不只 push 前）
即便 push 前 rebase 过，**等 CI 的这段时间里上游可能已合并** ⇒ 轮到自己合并时又落后
（本轮 `#1137`/`#1138` 各自二次冲突）。⇒ 口径：
```bash
gh pr view <n> --json mergeable --jq .mergeable   # 必为 MERGEABLE 才 merge
# 若 CONFLICTING：git fetch origin && git rebase origin/master && git push --force-with-lease
```

### ③ **删分支必须在 `state == MERGED` 之后**
本轮脚本在 `gh pr merge` **失败后仍执行**了 `git push --delete <branch>` ⇒ 分支消失 ⇒
GitHub **自动关闭** PR，且**无法 reopen**（它同时缓存了旧的 `CONFLICTING`）⇒ 只能**重新 push 分支 + 开新 PR**
（本轮 `#1138` 即 `#1136` 的重开）。⇒ 口径：
```bash
gh pr merge <n> --squash -t "..." -b "$(cat body.md)"
ST=$(gh pr view <n> --json state --jq .state)
[ "$ST" = "MERGED" ] && git push origin --delete <branch>   # 只在确认合并后删
```

## Flake 治理纪律

与 CI 相关的偶发失败**不要一律 rerun 过关**：修 / 补可诊断信息 / 明确记档为待复现。
清单、分类（A 真实竞态 / B 时序夹具 / C 基础设施）与**取证要求**（必须核对**失败 step** 而非
"日志里有没有该消息"）见 **`docs/testing/flake-ledger.md`**。

（典型反例：把 `setup-go` 缓存的 `BlobNotFound` 404 当 flake 并提了「禁用缓存」的 PR ⇒
取证发现该步骤**始终 success**、近 100 run 零失败 ⇒ **404 是正常 cache miss**，属**误判**，
PR 已撤回。**不要为换稳定而禁用工作正常的能力。**）
