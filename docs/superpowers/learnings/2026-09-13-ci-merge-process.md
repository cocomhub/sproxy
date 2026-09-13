# CI 与合并流程（2026-09-13 订正）

适用范围：sproxy 仓库所有 PR。本文记录**已实测验证**的流程事实与规则，避免重复踩坑。

## 1. 本仓 `master` 无分支保护 ⇒ `--auto` 不 gate CI

事实与证据：

- `gh api repos/cocomhub/sproxy/branches/master/protection` → **404**（无保护、无必检项）；
- PR #214 合并时间 `06:26:29Z`，而该 PR 最后一个 CI job 完成于 `06:30:26Z`（**提前约 4 分钟合并**）；
- #208 / #210 / #211 / #212 均带 1 个红色 job 被合并（红的是同一个 `Test (Go 1.26, ubuntu, +Vault)`）。

**规则**：不使用 `gh pr merge --auto`。合并流程固定为：

1. 轮询 `gh pr checks <PR>`，直到**总数 ≥ 14 且 `pending=0`**；
2. 仅当 `fail` 为空时执行 `gh pr merge <PR> --squash`；有红**不合并**（flake 可 `gh run rerun --failed` 重跑）。

注意：`gh pr checks` 输出**为空不等于全绿**，需分两种情况判：

- **检查尚未挂上**（刚推送 / run 被 cancel）：等下一轮轮询即可；
- **CI 根本没触发**（**纯文档 PR**）：`.github/workflows/ci.yml` 的 `paths-ignore` 含
  `*.md`、`docs/**`、`CHANGELOG.md`、`.gitignore`、`.editorconfig`、`.notestignore`
  ⇒ 只改这些路径的 PR **永远不会有 check**。判定方法：`gh run list --branch <branch>` 为空
  且 `git diff --name-only origin/master...<branch>` 全部落在忽略路径内。此时直接看
  `gh pr view <PR> --json mergeStateStatus`（应为 `CLEAN`）后合并即可，不必空等。

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
