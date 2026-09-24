# REPORT — sclient upgrade 自更新

## 状态

**DONE**（已提交、已 push、已开 PR，等待 CI 全绿）

- commit: `89df91492`（rebase 到 origin/master `563364567` 后；原始提交 `de6eb7a33`）
- 分支: `feat/sclient-upgrade`（worktree `.worktrees/feat/sclient-upgrade`）
- PR: https://github.com/cocomhub/sproxy/pull/577

## 改动文件（10 files, +1949/-3）

| 文件 | 说明 |
|---|---|
| `pkg/selfupdate/selfupdate.go` | 新包：Latest/ByTag（GitHub API 单次）、AssetName/NormalizeVersion/CompareVersions/FindAsset、Checksums 解析、DownloadAndVerify（SHA-256 fail-closed）、ExtractBinary（防 zip-slip）、SwapBinary（原子替换 + Windows 两段式 + lock） |
| `pkg/selfupdate/selfupdate_test.go` | 纯函数/网络/解包/替换全量单测（t.Parallel，httptest 127.0.0.1） |
| `cmd/sclient/upgrade.go` | `upgrade [--check] [--to <ver>] [--force]` 命令（--api-base/--release-base 隐藏 flag） |
| `cmd/sclient/upgrade_test.go` | CLI 全链路 httptest（含 --check --json 脚本解析、--to 完整升级替换） |
| `cmd/sclient/root.go` | 一行注册 NewCmdUpgrade |
| `test/e2e_cli_upgrade_test.go` | e2e（真实二进制 + 假 API/CDN）：--check --json 解析 + 完整升级替换断言 |
| `internal/archcheck/layers.go` | 登记新包 `pkg/selfupdate`（L0，零仓内依赖） |
| `internal/archcheck/dead_symbols_test.go` | 墓碑 `extractTarGz` 改名 `extractTarGzLegacy`（原 pkg/server 符号；新版同语义实现在新包，避免误报） |
| `docs/cli.md` | 子命令一览 + upgrade 章节（R15 门禁） |
| `docs/roadmap.md` | 11.6-① / A5 状态 -> 已落地 |

## 测试证据

- TDD 红灯先行（先写测试后实现）
- **变异验证命中（3 次，均还原）**：
  1. 删 SHA-256 校验（`if false`）-> `TestDownloadAndVerify_TamperedBytesFails` 红
  2. up-to-date 判定条件反转 -> `TestUpgradeCmd_UpToDateNoForce` 红
  3. --check JSON update_available 判定反转 -> 红
- 全绿：
  - `go test -count=1 -race ./pkg/selfupdate/ ./cmd/sclient/ ./internal/archcheck/`（rebase 后复跑）
  - `go test -count=1 -race -tags=e2e -run "TestE2E_CLI_Upgrade" ./test/`
  - `make test-all` / `make test` / `make notest` / `make deadcode-check` / `make check-loopback`
  - golangci-lint 0 issues（root + cmd/sclient + e2e tag + make lint-all 全子模块）
  - gofmt / goimports 干净；make vet 通过
- e2e 断言真实副作用：--check --json 输出可解析 JSON（current/latest/update_available）；--to 完整升级后目标二进制内容被替换

## 已知残余 / 风险

- Windows `.upgrade.bat` 两段式兜底已实现并被单测覆盖（`TestSwapBinary_FallbackBat`，注入全失败 rename 链），但**真实 Windows exe 占用场景**未在 CI 上以真实二进制复现（e2e 完整升级在 Windows 上以临时副本为目标成功通过——目标非运行中进程，故不会触发占用分支）。建议后续在真实 Windows 上手工验证一次「运行中自替换」路径。
- GitHub API 未认证限流 60/h：每调用仅 1 次 API（下载走 CDN 直链），--check 高频调用仍可能 403（已 fail-closed 报错）。

## CI 状态

PR #577 已创建，CI 检查运行中（`gh pr checks 577`：Lint/Test/Build/E2E 等 pending，Conventional Commits 已 pass）。等全绿后按流程合并。
