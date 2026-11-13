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

## 清单

| # | 位置 | 类别 | 状态 | 备注 |
| --- | --- | --- | --- | --- |
| A | `cmd/sclient` `NewRootCmd` 包级 pflag | **A 真实数据竞争** | ✅ **已修**（#1130） | CI 复发 ≥2 次；根因＝包级变量被并行写；**强制并发**测试可确定性复现（master 报 `DATA RACE`、修复后无） |
| B | `pkg/telemetry` `TestTracerDoubleEndProducesWarning` | — | ✅ **非问题** | 该测试需接管全局 slog ⇒ 已登记 `// sproxy:serial:`，不并行即无竞争 |
| H | `setup-go` 模块缓存 `BlobNotFound` 404 | — | ✗ **已更正误判** | 见下节：它**从不导致失败**，**不要动缓存** |
| C | `pkg/tunnel` 3 例同时失败（#1115 的 `Test (windows)`；#1138 的 `Test (ubuntu, +Vault)` **复现**） | **A 真实数据竞争** | ✅ **已修** | 根因：`TestGetBuf_PutBuf` 在 `putBuf` **归还池之后**仍读 `buf[0]`（断言"清零"）⇒ 池可**立即**把该 buffer 交给并行使用者（CI 实测 `mockStream.Read`）⇒ race。修法：**不再读归还后的内存**，改为锁定 `getBuf` 的契约（长度/可再次取用）。**对照**：master 整包 `-race` 复现 1/3 次、修复后 `-count=20` 通过 |
| D | `pkg/server/s3` `TestListObjectsV2_NamedBucket` / `TestBucketRouting` | B? | 🔍 **待复现** | 曾本地复现一次、其后多次不复现 |
| E | `TestStageDL_TamperedFallsBackToRemote` | B? | 🔍 **待复现** | 在一次全量 `./pkg/...` 中失败、重跑即过 |
| F | `pkg/cloud` `TestCloudDownloadManager_SubmitAndStart_Async` | B? | 🔍 **待复现** | 文案 `timeout waiting for async download to complete`；疑**等待语义**（非纯超时） |
| G | `pkg/volume/secretdata` `TestWriteFileStream_MemoryDeliveryUploadFail_NoChunkOrphan` | B? | 🔍 **待复现** | 失败文案「夹具未命中内存交付分块上传失败分支」⇒ 疑**块级并发下错误汇总文案**不确定；本地 `-count=30` 与全包 ×3 均不复现 |

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
