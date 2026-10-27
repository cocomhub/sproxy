# CI 耗时优化：等待注入 + 覆盖率单遍（2026-10-27）

> 背景知识归档：本次为「CI 耗时分析 → 实施优化」的决策与踩坑记录，供后续维护者沿用方法论与避免重复踩坑。

## 实测数据（GitHub Actions API，近 100 run / 逐 job / 逐 step）

| 指标 | 数值 |
|---|---|
| 单次 CI 总墙钟 | 中位 ~18min（1086s），p90 ~30min（唯一 >6h outlier 是隔夜队列饥饿，非逻辑慢） |
| 关键路径（最慢 job） | 中位 926s ≈ 15.5min |
| 平均 job 分钟 | ~69 job-min / run |
| 队列等待 | 除 Sonar（needs: [test, test-submodules] 串行，中位排队 898s）外 ~4s |

job 中位耗时排序：Test(ubuntu,+Vault) 891s > E2E(windows) 611s > Test(windows) 602s > E2E(ubuntu) 325s > Chaos 312s（fuzz 6×30s=250s）> SubModules 275s > Lint 224s（check-format 106s）> UI E2E 208s > Sonar 204s > Build×6 ~100s。

## 根因（本地 -json 复现，4 核）

1. **等待超时 30s 被循环放大**：2026-10-18 用户裁定「配额不足排队重试（默认 30s）」后，测试 harness 没设 `quotaWaitTimeout` ⇒ 每个「配额故意打满断言 507」用例白等 30s。pkg/server 有 8 个串行 30s 测试 + 若干 30s 并行，`TestVolumesMirror_QuotaFullSkips` 等单独跑 35s。
2. **两个 260s+ 长尾**（t.Parallel 并行测试被同包慢测试挤占）：
   - `TestBackupQuotaFS_DurableCapacityLedger`：容量层 waitq 等待 30s 触发后每步 ~5s×6。
   - `TestBandwidthThrottles*`：`testHTTPClient` 无超时 + 1KB/s 限速 10KB ≈10s，被挤占时无限等（2 核 runner 上放大到 250s+）。
3. **覆盖率第二遍全量**：CI `cover-check` → `test-cover` 把全量 -race 测试再跑一遍 ≈ +7min/run（`-count=1` 缓存当时因命令行 `-parallel 8` 与 test 不一致而无法复用）。

## 方案：测试可注入等待上限（非假时钟）

- **waitq 已有 Clock 接口可注入假时钟**，但 waitAcquire 的超时用 `time.NewTimer(timeout)`（真实墙钟）——Clock 只服务 `Now/Since`（老化排序/等待时长统计），推假时钟不会让 timer 提前触发。因此「配额打满断言 507」的用例**不能用假时钟加速**，只能注入短超时（50ms）。
- 注入点全部走**结构体私有字段 + 构造时注入**（遵守 AGENTS.md 7b：禁止全局可变 seam）：
  - `pkg/files`：`WithQuotaWait Option` + `runtime.waitTimeout()` 单一入口（7 处 `WaitReserve` 全走它；生产恒 nil = 30s 语义不变）。
  - `pkg/server`：`RegisterRoutesOpts.QuotaWaitTimeout` 测试瞬态 opt（先例 TotpRateLimit）；`buildVolSetHandlers`/`newAssemblyTestHandlers` 设 50ms。
  - `backupQuotaFS.waitTimeout`、`reserveCopyQuota(timeout)` 参数化；`filesStorageManager` 透传。
  - 注意 `newMinimalService`（pkg/files 测试）独立构造也要注入。
- **带宽测试**：给下载 client 加 60s Timeout（断言语义不变，只防无限等）。
- **覆盖率单遍**：`make test` 去 `-parallel 8`（与 cover 命令行对齐 ⇒ 缓存可复用）；新增 `cover-test`（复用 make test 缓存产 build/cover.out）；`cover-check COVER_REUSE=1` 只算阈值不重跑测试。archcheck 的 `TestDeadcodeGate_WiredIntoCI` 会钉住 CI 里必须有 `make cover-check` 字样（保留该调用，加 COVER_REUSE=1 即可）。

## 效果

- pkg/server -race 全量：本地 234-300s → **66-75s（-74%）**；CI 应同步 -3~4min/test job。
- 覆盖率 step 不再第二遍全量（-7min/run 关键路径）。
- `make check-ci` 全绿（含 archcheck 门禁、lint 0）。

## 踩坑 / 残余

1. **pkg/cloud 的 `TestCloudDownloadManager_CancelStopsDownload` / `TestDownloadIntegrity_ResumeResetsState` 在本机 -race 偶发红 = 既有 flake**：master 基线（独立 worktree）3/11 失败率复现，与本改动无关。特征：`download goroutine did not stop after cancel` 的 WaitFor 5s 超时——排队任务在等待器触发前 cancel 完毕会留下 brief window；`RetryDelay: time.Millisecond` 在 -race 下偶发超时。处置：flaky 属测试时序问题，另行登记待办，不在本 PR 修。
2. **waitq 推假时钟不加速 timer**：`waitAcquire` 的 `time.NewTimer(timeout)` 是真实墙钟；Clock 只服务老化/统计。想全虚拟化需给 waitq 加可注入 timer 工厂（未来可选，本次不做）。
3. `pkg/cloud` 的 `manager_test.go:664` 用 `WaitForBool(30s,…)` 轮询带宽协调器 refill（2s window）——若未来想提速需把 `window` 缩短，但 2s 是跨平台（Windows 文件 IO）余量，不宜动。

## 未做（后续候选）

- chaos fuzz 6×30s → 可合并同包目标/减半（250s job）。
- check-format 106s（gofix-all 全模块）→ 只对改动文件跑 gofix。
- Playwright 浏览器二进制 actions/cache（装一次 40s）。
- Sonar 排队 898s：拆 needs 或独立扫描 job（墙钟无贡献，仅拖尾）。
- Build×6 matrix 合并 setup-go 次数（省 job-min，墙钟不变）。