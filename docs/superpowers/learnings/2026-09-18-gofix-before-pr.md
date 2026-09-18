# 2026-09-18 经验：每次 PR 前必须完成 make build（含 go fix）——10 文件残留教训

## 现象
当前 master 跑 `go fix ./...` 一次性触发 10 个文件的 stdlib 现代化改动（都是最近合并的功能 PR 引入的代码）：
- `forvar`：移除冗余 `tc := tc`（Go 1.22 loopvar 语义）
- `minmax`：`min(d.index+count, len(d.ents))` 替代 if/else
- `stringscutprefix`：`strings.CutPrefix` 替代 HasPrefix+TrimPrefix
- `slicescontains`：`slices.Contains` 替代手写循环
- `rangeint`：`for i := range n` 替代 3-clause for
- `waitgroupgo`：`wg.Go(func(){})` 替代 Add/go/Done
- `stringsseq`：`strings.SplitSeq` 替代 Split 循环

## 根因分析（为什么之前没做到）
1. **go fix 触发的是 Go 1.22+ 已存在的 stdlib 现代化**——这些 fix 不是新 Go 版本才加的，而是**代码从未被 go fix 处理过**
2. **CI 不跑 go fix**：Lint job 只跑 golangci-lint（无 go fix 检查）→ CI 拦不住
3. **pre-commit hook 跑 make build（含 go fix）但形同虚设**：
   - hook 06-15 就配置了（core.hooksPath=.githooks），但子代理提交常用 `--no-verify` 绕过
   - hook 只 re-add **STAGED_FILES**（`git diff --cached --name-only`）——go fix 改的**新文件不在 STAGED 里 → re-add 不到 → fix 改动丢失**
4. **控制者接手验证只跑 go test/lint**，没跑 make build → go fix 结果未落盘

## 避免方案（防再发）
### 必须执行（流程纪律）
- **每次 PR 前必须完成 `make build`**（依赖 fmt → gofix + addlicense + gofmt）——子代理、控制者都一样
- 提交前自查：`go fix ./... && git diff --stat`（应无输出或已提交）

### 建议机制（可选，需用户确认）
- **方案 A**：CI Lint job 加一步 `go fix ./... && git diff --exit-code`（dirty 即 fail）——硬门禁
- **方案 C**：hook 增强——go fix 后**重新 add 全部改动文件**（`git add -u` + 新文件），不只 STAGED
- **方案 B**：控制者接手子代理产出时【先跑 make build 再验证】——流程硬规则

## 防再发机制（2026-09-18 用户要求，已落地 #377）
1. **CI Lint job 加硬门禁**：`make check-format`（go fix + addlicense + gofmt 全部 module 后 git diff 必须干净）——绕过 hook 也会在 CI 被拦
2. **pre-commit 支持多 go module**：hook 改跑 `make fmt-all`（go fix + addlicense + gofmt 全部 SUB_MODULE_DIRS）+ 全部子 module 的 vet/gofmt/lint；re-stage 用 `git add -u` + 未跟踪文件（修复旧 hook 只 re-add STAGED_FILES 的漏洞）
3. **PR title CC 格式硬门禁**：`pr-title.yml`（pull_request_target 校验 `type(scope): subject`，与 commit-msg hook 同规则）
4. **绝对禁止 `git commit --no-verify`**（AGENTS.md 硬规则）：pre-commit 全覆盖，绕过会被 CI 拦

## 验证
- 修复提交：`0a48687a`（10 文件，21 insertions / 41 deletions，纯 stdlib 现代化无行为变更）
- 相关包测试全绿（internal/size、pkg/gateway/webdav、pkg/volume/registry、pkg/volume/webdav、pkg/server 相关用例）
- go fix 清理同时覆盖子 module：baidupcs（5 fix）/ kad / s3 / cmd-sproxy（手动应用 go fix 工具 bug 的建议）
