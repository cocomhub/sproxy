# REPORT —— 重复文件发现（roadmap 11.5-⑦）

## 状态

**DONE（P1 域纯逻辑，PR #599 已创建，CI 进行中）**

## 交付

roadmap **11.5-⑦ 重复文件发现**：两种模式的重复文件报告入口（设计文档 `docs/designs/2026-09-24-duplicate-finder.md`），输出重复组 + 可回收空间 `DuplicateBytes`。

- **`ReportFromLedger`（台账快照）**：复用 `DedupStore` 台账（dedup.json，checksum → 引用列表）秒级生成报告；refs≥2 才成组；台账 nil（dedup 未启用）返回空报告。
- **`ScanVolume`（全量扫描）**：遍历卷 user 桶（跳过 symlink 与 meta/cloud/archive/chunk/version/trash/`.__` 魔法目录）→ `checksum.Reader` 逐文件 SHA-256 → 按 checksum 分组 → 过滤 refs<2 → `DuplicateBytes=Σ size×(refs-1)`。
- **错误语义**：单文件读失败记 `Errors` 继续（报告不中断）；卷根 nil fail-fast；ctx 取消/超时返回已扫部分 + `Truncated=true`。
- **`DedupStore.exportSnapshot`**：读锁内深拷贝（与 save 同构模式），供台账快照枚举。

## 改动文件

| 文件 | 内容 |
|------|------|
| `pkg/files/dup_report.go`（新，231 行） | DupGroup/DupRef/Report/ScanError 类型 + ReportFromLedger + ScanVolume + 子目录校验（validateDupSubdir）|
| `pkg/files/dup_report_test.go`（新，315 行） | 7 个测试：分组/savings、ledger≡scan、不可读文件继续、symlink 不跟随、ctx 取消 Truncated、subdir 限定、nil Root fail-fast |
| `pkg/files/dedup.go`（+19） | exportSnapshot 快照方法（读锁内深拷贝）|
| `docs/roadmap.md`（11.5-⑦） | 缺 → 部分（P1 落地，P2 路由/sclient/metrics 待做）|

## 验证证据

- **测试**：`go test -count=1 -race ./pkg/files/` 全绿（7.6s；含 `-run 'TestDedupReport'` 精确跑 7 用例，Windows 下不可读文件/符号链接用例按平台 skip 语义跳过）；`go test -count=1 -race ./pkg/server/` 43.6s 绿（回归）；`go test -count=1 -race ./pkg/checksum/` 绿
- **变异验证 3 命中**：
  1. scan 漏过滤 refs<2 → 组数 2→3 红（`重复组数=3 want 2`）
  2. ledger 漏过滤 refs<2 → 唯一文件成组红（`ledger 组数=2 want 1`）
  3. savings 公式 refs-1 改 refs → `DuplicateBytes=80 want 50` 红
  全部还原后复绿（`git diff --stat` 确认仅任务 4 文件）
- **门禁**：`go test ./internal/archcheck/` 绿；`make deadcode-check` PASS；`make notest` OK；`golangci-lint run ./pkg/files/` 0 issues；`make lint` 0 issues；gofmt/goimports 无输出
- **pre-commit 5/5 通过**：go fix + go vet + gofmt + check-loopback + lint（root + 全部子 module）0 issues
- **提交**：`git fetch origin && git rebase origin/master`（快进到 ae1baff5d，零冲突）；只 add 本任务 4 文件；无 Co-authored-by；无 --no-verify

## 残余（P2，设计文档片划分）

- 服务端 `POST /api/dedup/report` 路由（body: `{mode: ledger|scan, subdir?}`）
- sclient `dedup-report [--mode scan] [--subdir]` 命令
- metrics：`sproxy_dedup_scan_total{result}` + `sproxy_dedup_duplicate_bytes`
- E2E：起真实服务，上传 2 个相同文件 → report 命中 1 组

## PR

- PR #599：https://github.com/cocomhub/sproxy/pull/599（CI 14 项检查进行中，全绿后由主 agent squash 合并）
