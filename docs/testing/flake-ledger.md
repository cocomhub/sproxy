# Flake 台账（可追踪、有据可依）

> 目的：把「CI/本地偶发失败」从**口头记忆 + 一律 rerun** 变成**可追踪、可取证、有纪律**的清单。
> 维护规则见文末。

## 处置纪律（先取证，不盲改，不劣化）

1. **先分清三类**：
   - **A 类 真实竞态/缺陷** ⇒ 必须**改代码**（`race detected` 属此类，会掩盖真实缺陷）；
   - **B 类 时序/夹具不稳** ⇒ **先取证根因**，再改判据/夹具；**禁止**用加 `sleep`/放宽断言糊过去；
   - **C 类 基础设施/环境** ⇒ 改 workflow/配置，但**必须先证明它真在失败**。
2. **取证到「失败步骤」**，而不是"日志里有没有这条消息"：
   ```bash
   gh api repos/{owner}/{repo}/actions/runs/<run-id>/jobs \
     --jq '.jobs[]|select(.conclusion=="failure")|.name, (.steps[]|select(.conclusion=="failure")|.name)'
   ```
   （反例见 §已更正误判：`setup-go` 的 404 只是**正常 cache miss**，步骤结论始终 success。）
3. **不得为了"换稳定"而禁用/降级一个工作正常的能力**（用户明示 2026-11-13）。
4. **不接受的 flake 不得只靠 rerun 过关**：要么修、要么补可诊断信息、要么明确记档为"待复现"。
5. **每修一项都要有判据**（回归测试 / 变异命中 / 结构性论证 + 强制复现）。
6. **B 类「负载型超时」的标准修法 = `testutil.ScaleTimeout`**（2026-11-27）：
   固定等待窗口（`5s/10s/30s`）在 `-race` + 全包 `t.Parallel` 下会把"只是慢"误报成
   "泄漏/超时"（实测同一用例 4.1s → 28–39s）。正确做法是把窗口包成
   `testutil.ScaleTimeout(10 * time.Second)` —— 它按 `//go:build race` 决定的 `RaceEnabled`
   放大 4×，**非 race 构建原样返回**（保持对真泄漏/真超时的严格检出）。
   **禁止**为了"换稳定"直接把常量调大（那会让非 race job 一起变宽松），也**禁止**只靠 rerun。
   已接入：`pkg/cloud`（7 文件）、`pkg/volume/secretdata`（6 文件）。新增等待窗口时优先用它。

## 清单

| # | 位置 | 类别 | 状态 | 备注 |
| --- | --- | --- | --- | --- |
| A | `cmd/sclient` `NewRootCmd` 包级 pflag | **A 真实数据竞争** | ✅ **已修**（#1130） | CI 复发 ≥2 次；根因＝包级变量被并行写；**强制并发**测试可确定性复现（master 报 `DATA RACE`、修复后无） |
| B | `pkg/telemetry` `TestTracerDoubleEndProducesWarning` | — | ✅ **非问题** | 该测试需接管全局 slog ⇒ 已登记 `// sproxy:serial:`，不并行即无竞争 |
| H | `setup-go` 模块缓存 `BlobNotFound` 404 | — | ✗ **已更正误判** | 见下节：它**从不导致失败**，**不要动缓存** |
| C | `pkg/tunnel` 3 例同时失败（#1115 的 `Test (windows)`；#1138 的 `Test (ubuntu, +Vault)` **复现**） | **A 真实数据竞争** | ✅ **已修** | 根因：`TestGetBuf_PutBuf` 在 `putBuf` **归还池之后**仍读 `buf[0]`（断言"清零"）⇒ 池可**立即**把该 buffer 交给并行使用者（CI 实测 `mockStream.Read`）⇒ race。修法：**不再读归还后的内存**，改为锁定 `getBuf` 的契约（长度/可再次取用）。**对照**：master 整包 `-race` 复现 1/3 次、修复后 `-count=20` 通过 |
| D | `pkg/server/s3` `TestListObjectsV2_NamedBucket` / `TestBucketRouting` | B? | 🟡 **待复现 + 已加稳定性探针** | 曾本地复现一次、其后多次不复现（本机 `-race -count=3` 全绿）。这两个用例都是纯 HTTP 断言，共同的非确定来源只有"服务端未及时响应" ⇒ 新增 `TestS3_SlowAndOversizedRequestsDoNotHang` 钉住「中断/超大 PUT 必须有界结束」，避免该前提回归后再去追那两个用例。若仍复现，按 F 条方式取证。|
| E | `TestStageDL_TamperedFallsBackToRemote` | B? | 🔍 **待复现** | 在一次全量 `./pkg/...` 中失败、重跑即过 |
| F | `pkg/cloud` 全包等待类用例（`SubmitAndStart_Async` / `QueuedTaskCancellable` / `StorageAccountingNoLeak` / `StorageFullAfterDownload_*` / `RecoverRestartsDownloading` …） | **B 负载型（已定位并修）** | ✅ **已修 2026-11-27** | 原文案 `timeout waiting for async download to complete` / `Close() hung: download goroutine leaked`。**根因（本地确定性复现）**：`-race` + 全包 `t.Parallel` 的 CPU 抢占 —— 单跑 4.1s 的用例在全包 `-race` 下 28–39s 才完成，而窗口是固定 5s/10s/30s ⇒ 把"慢"误报成"泄漏/超时"。**取证**：单独跑失败用例全绿（4.1s）、`-parallel 4` 全包全绿（94s）、`git diff --name-only origin/master \| grep cloud` 为空（该分支未改 pkg/cloud）。**修法（不是加大常量）**：新增 `testutil.ScaleTimeout`，按 `-race` 构建放大 4×（`RaceEnabled` 由 `//go:build race` 决定）⇒ **非 race 构建保持原严格窗口**，不放宽真实缺陷检出；cloud 包 7 个测试文件的等待窗口统一接入。**验证**：全包 `-race -count=1` 连续 3 次全绿（38s/36s/35s，此前 67–94s 且多次失败）。 |
| G | `pkg/volume/secretdata` 等待类用例（含 `TestWriteFileStream_*` / `TestStageDL_*` / `meta_*`） | **B 负载型（窗口已加固）** | 🟡 **窗口已加固，仍待复现** | 原失败文案「夹具未命中内存交付分块上传失败分支」。本机 `-race -count=2` 未复现（202s 全绿），但该包 6 个测试文件的固定窗口（2s/10s/30s）已统一接入 `testutil.ScaleTimeout` —— 与 F 条同一机理（race + 全包并行抢 CPU）。**若再出现且带完整失败行**，按 F 条方式取证（单跑/`-parallel`/diff）。|

### 待复现条目的取证要求（下次出现时补齐）
- 取**失败 step 名**与**完整失败行**（含实际错误文案）；
- 记录 run id / job id / 是否 `-race`；
- 用 `-count=50 -race` 或全包多次尝试本地复现；复现成功才动代码。

## 已更正误判：`setup-go` 缓存 404（原 #1131，已关闭）

- **曾列为 flake H 并提了「禁用缓存」的 PR** ⇒ **用户质疑**「根因清楚吗？不要无脑劣化功能」。
- **取证结论**：失败 run 的失败步骤是 `Run UI E2E tests` / `go test (race+coverage)`；
  `Run actions/setup-go` 步骤结论**始终 success**；最近 100 个 run 中该步骤**失败 0 次**。
- ⇒ `BlobNotFound` 404 是缓存的**正常 miss 路径**（未命中 ⇒ 继续下载），**不影响步骤结论**。
- ⇒ **PR #1131 已关闭撤回**；缓存**保持启用**（本仓该缓存工作正常）。

## 维护规则

1. 新增条目必须写**类别 + 状态 + 取证信息**（缺则视为未登记）；
2. 状态流转：`🔍 待复现` → `🛠 修复中` → `✅ 已修（附判据）` / `✗ 已更正（非问题）`；
3. **A 类（真实竞态）永远优先**，且必须给"可确定性复现"的回归用例；
4. 修完把该条状态改为 `✅ 已修` 并附 **PR 号 + 判据**；
5. 每季度回顾：超过 2 个版本仍未复现的 `🔍` 条目，可标记 `❄ 长期未复现`（保留在册，不删除）。
