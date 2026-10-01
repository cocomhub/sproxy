# Sonar 质量门禁清理 + CI 覆盖架构（2026-10-01 战役）

> 来源：2026-09~10 的 sproxy Sonar 存量清理战役（**1252 → 2 项，98.5% 清除率**）+ CI 覆盖架构重构（#692-#712）。
> 定位：Sonar 指标解读、NOSONAR 格式、覆盖率口径、CI 覆盖聚合的**踩坑记录**。CLAUDE.md/AGENTS.md 只留摘要指向本文。

---

## 1. Sonar NOSONAR 格式铁律（最重要）

**SonarGo 只识别「行尾」NOSONAR**：`code // NOSONAR: 规则 — 理由` 必须写在**被标记行末尾**。

- ❌ 独立上一行 `// NOSONAR`（前 3 行注释 + 第 4 行 NOSONAR）——**SonarGo 不识别**，白加。
- ✅ `if err := os.Remove(path); err != nil { // NOSONAR: S2083 — ...` —— 行尾，识别。
- 多行理由保留为**前置注释**，NOSONAR 本体必须在被标记行行尾。

**踩坑证据**：#692/#698 曾用「独立上一行裸 NOSONAR」，复扫证明从未生效（S2083×3/S6096/S1313×4 仍 OPEN）；#704 全改为行尾格式后清零。JS 规则同理（S9382 NOSONAR 也需行尾，#710 修复）。

## 2. Sonar 专属规则 vs golangci-lint 盲区

部分规则**只有 Sonar 能查**，golangci-lint 完全测不到——独立审查 agent 若只用 lint 会漏：

- **S8242**（结构体持有 context.Context 字段）：S107 参数收敛把 ctx 收进结构体时**必然触发**，lint 0 issues 也拦不住。修复：单操作作用域结构体加行尾 `// NOSONAR: S8242 — 单次操作作用域共享 ctx`（#701）。
- S8196/S8209/S8205/S8184 等 godre 规则同理。

**流程教训**：等价重构（S107/S3776/JS 现代化）后必须**Sonar 复扫验证**，不能只依赖本地 lint 绿。

## 3. 等价重构会「制造」低覆盖（度量伪象）

S107 把大函数拆成多方法、S3776 拆 helper——**逻辑等价、行号全变**：
- Go 覆盖是**行级（atomic）**，重构后旧覆盖记录不匹配新行 → Sonar 把这些重写行算「新增未覆盖行」。
- 实测 write_ops.go 重构前后未覆盖段**完全相同（60/368）**，证明未覆盖是本来就有的边界，非重构引入。
- 影响：Sonar `new_coverage` 骤降（重构批次期间 26.6%）。**这是度量伪象，不是质量回归**——等价搬移的代码原测试仍覆盖。

**处理**：重构 PR 应配套补关键分支测试；或接受 new_coverage 短期波动（下周期自然恢复）。

## 4. 覆盖率口径：本地 vs Sonar 差 2.4 倍（正常）

| | 本地 cover-check | Sonar |
|---|---|---|
| 指标 | **语句覆盖**（statement） | **行覆盖**（line） |
| 分母 | 44k 可执行语句（仅根 module） | 139k 行（根+15 子 module 全部） |
| 结果 | 81.4% | 33.3% |

**为什么差这么多**：
1. **语句 vs 行**：一条 Go 语句跨 1~3 行（多行签名/字面量），行数 ≈ 语句 × 2。实测根 module 15,099 条跨行语句。
2. **范围**：`go test ./...` 在 go.work 下**只测根 module**；Sonar `sonar.sources=.` 扫全部源码。
3. **Sonar ncloc 虚高**：Go 插件把 `_test.go` 行计入 ncloc（本仓 158k 测试行 vs 94k 源码行），进一步稀释。

**结论**：本地 81.4% 是「根 module 语句覆盖」（真实可靠、Go 社区口径）；Sonar 33.3% 是「全仓行覆盖」（口径不同、含测试文件、有重复计数）。**Sonar 总体覆盖率对 Go 参考价值有限**——其价值在 **issue 规则维度**（S107/S3776/S9383），不在覆盖率数字。

**配置缓解**（#713）：
- `sonar.test.exclusions=**/*_test.go`：测试文件不计 ncloc/分母。
- `.pi/**` 加入 exclusions。
- 移除 `fileclient.sh`（零引用遗留脚本）与 grpc/webrtc 排除（#57 历史遗留，现已有 79-81% 真实覆盖）。

## 5. go.work 下的覆盖聚合（#712 核心）

**`go test ./...` 在 go.work 模式下只测当前 module**（实证：82 包、16 子 module 零命中）→ `make test-cover` 只覆盖根 → Sonar 把子 module 源码按 0% 计。

**聚合方案**（Makefile `test-cover-all`）：
- 每个子 module **cd 进入 + GOWORK=off + coverprofile 绝对路径** → 覆盖记录为完整 import path（`github.com/cocomhub/sproxy/pkg/...`），与根覆盖同构，Sonar 剥离前缀匹配 `sonar.sources=.`。
- 产出 `build/coverage/<slug>.out`（`pkg_tunnel_mesh` 等），`sonar.go.coverage.reportPaths=build/coverage/*.out,build/cover.out`（通配符官方支持）。
- `SKIP_ROOT_COVER=true`：CI 的 test job（vault 服务版）产 root.out，test-submodules 跳根避免重复。

**关键路径陷阱**：从仓库根 `GOWORK=off go test ./pkg/volume/ext/s3/...` **会失败**（不跨 module）——必须 cd 进子 module，coverprofile 写绝对路径（Windows 需 `D:/` 格式）。

## 6. CI 架构优化（#712）

- **Sonar 纯消费**：`needs: lint` → `needs: [test, test-submodules]`，下载两个覆盖 artifact 后扫描。**不再依赖 lint**（此前串行等最慢 job）、**不再自跑 cover-check**（此前重复全量根测试）。`sonarqube-scan-action` 不需要 setup-go（纯上传）。
- **fuzz 并入 chaos**：6 个 fuzz 目标（补 FuzzParseKey/FuzzValidateFilePath/FuzzCalcChunkSize）移入 chaos job，常规 test 不被拖长。fuzz 必须 `-fuzztime=30s` 限时（Benchmark job 曾超时教训）。
- **MinIO 迁移**：s3 集成测试在 16 号子 module，原挂在 test job 是**死配置**（根 `./...` 不跨 module，从未实跑却每次起容器）；迁到 test-submodules + `S3_ENDPOINT` 后**首次实跑**。
- **download-artifact SHA**：用官方 v7 tag 的真实 SHA（`37930b1c2a...`），不能用编造的（CI 解析失败）。

## 7. 测试基建踩坑

- **注册表测试清空全局竞态**（#601 同型）：`TestCipher_RegisterAndLookup` 曾 `cipherRegistryClear()` 清空共享注册表，并行的 `TestCipher_StreamRoundtrip`（查 aes-256-gcm）在清空窗口内 miss → 偶发「未知算法」。**修复：用唯一注册键（时间戳）测首次/重复注册语义**，不碰共享注册表（#701）。
- **集成测试 TOCTOU**：MinIO 首次实跑后，多个 `t.Parallel` 用例并发 `MakeBucket` 同桶 → `BucketAlreadyOwnedByYou`。**修复：`minio.ToErrorResponse(err).Code == BucketAlreadyOwnedByYou` 容忍**（视为并发抢先创建）。
- **stall 型测试超时**：`DownloadTimeout: 300ms` 在 CI 负载下建连慢于超时 → 偶发 `context deadline exceeded`。**修复：按 stall 时长校准**（stall=5s → 超时 2s；stall=1~2s → 保持 300ms，避免超时==stall 竞态）。
- **pre-commit 误扫描**：`gofmt -l .` 会扫到 `.worktrees/`（其他并行 worktree 的未完成文件）误拦提交 → 排除 `.claude/worktrees/` 与 `.worktrees/`。

## 8. 协作流程教训（多会话）

- **共享 checkout 会被其他会话切换分支**（曾切到非我创建的 `fix/s3-bucket-sign-race`）——独立任务用 `git worktree add /tmp/wt-xxx -b branch origin/master`。
- **Edit 工具落点**：`file_path` 必须指向 worktree 的绝对路径（`C:\Users\leon.li\AppData\Local\Temp\wt-xxx\...`），否则改到共享主 checkout。
- 独立审查 agent 用 lint 测不到 Sonar 专属规则 → 重构后必须 Sonar 复扫。
