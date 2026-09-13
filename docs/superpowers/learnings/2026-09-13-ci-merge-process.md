# CI 与合并流程（2026-09-13 订正）

适用范围：sproxy 仓库所有 PR。本文记录**已实测验证**的流程事实与规则，避免重复踩坑。

## 1. `master` 有 **active ruleset** 必检 7 项 ⇒ `--auto` **会** gate CI（纯文档 PR 例外）

事实（2026-09-13 复核）：

- `gh api repos/cocomhub/sproxy/branches/master/protection` → **404**：**没有** classic branch protection；
- 但 `gh api repos/cocomhub/sproxy/rulesets` → **有** active ruleset（`master branch`, id `17891054`，
  created `2026-06-19`），规则含 `pull_request` / `required_status_checks` / `deletion` / `non_fast_forward`；
- 必检项（7 条）：`Test (Go 1.26, ubuntu, +Vault)`、`Test (Go 1.26, windows)`、
  `E2E (real binaries) (ubuntu-latest)`、`E2E (real binaries) (windows-latest)`、
  `Test Sub-Modules (cmd + ext + hub + mesh)`、`UI E2E Tests`、`Benchmark`；`required_approving_review_count = 0`；
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

## 2. Benchmark job 超时即取消重试（10 分钟规则）

现象：`Benchmark` job（`make bench`）偶发长时间卡在 `in_progress`（实测 30~40 分钟），而本地同命令全绿
（`go test -bench=. -benchmem -count=5 -run=^$ ./...`）⇒ 判定为 runner 争用，非代码缺陷。

**规则**：单个 `Benchmark` job 超过 **10 分钟**仍未完成 →

```bash
gh api -X POST repos/cocomhub/sproxy/actions/runs/<run-id>/cancel
# 等待该 job 变为 completed/cancelled
gh api -X POST repos/cocomhub/sproxy/actions/runs/<run-id>/rerun
```

要点：

- rerun 会生成**新的 job id**（`run_attempt + 1`）⇒ 每轮必须从 `gh pr checks` **动态取 job id**，不可缓存旧 id；
- `gh run cancel <run-id>` 曾返回 `HTTP 500`；改用 REST cancel（`gh api -X POST .../cancel`）更可靠；
- 实测：重试一次后 Benchmark 约 5 分钟完成。

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

## 5. 其他已实测的踩坑

- **不要用 `git stash`**：仓库中存在其他分支遗留的 stash，`git stash pop` 会弹出**别人的** WIP
  （已发生两次，造成 `UU` 冲突）⇒ 改用临时副本，避免 stash。
- 提交前必须 `export PATH="$PATH:$(go env GOPATH)/bin"`（pre-commit 需要 `golangci-lint` / `addlicense`）。
- 新建分支前先 `git fetch && git checkout master && git reset --hard origin/master`
  （曾因本地基座落后导致 add/add 冲突、且 **CI 未触发**）。
- Windows 下 `python` 看到的 `/tmp` 与 git-bash 的 `/tmp` **不是同一目录**；跨工具传递文件用仓库内相对路径。
