# 固定等待清理台账（方案 B：确定性时钟/条件等待）

> 目标：消灭测试中的**真实**固定等待（`time.Sleep` 定值等待）。
> 手法：纯逻辑 fixture 用 `testing/synctest` 气泡（虚拟时钟，零真实耗时）；
> 条件可观测的轮询改 `testutil.WaitFor/WaitForBool`（30s 上限）；真实 I/O 的 fixture
> 改 channel 门控。synctest 适用边界见 docs/archive/agent-operating-rules.md
> 或 MEMORY.md「synctest 气泡实证」：只适用纯内存（真实 socket/HTTP 阻塞不算 durably blocked）。

| 测试 | 改造前 | 改造后 | 手法 |
|---|---|---|---|
| TestPersister_ScheduleDebounceRealTimer（hub） | ~0.15s + CI 可 flake | 0.00s | 气泡：debounce 虚拟时钟，`<-(2*debounce)` 推进 + Wait 收敛 |
| TestSignalQueue_WaitTwoConcurrentWaiters_BothWake（hub） | ~0.05s | 0.00s | 气泡：注册间隙 50ms → `synctest.Wait()` |
| TestStream_ReadDataBeforeImmediateClose（mux） | 0.12s | 0.01s | 气泡：20 轮×5ms 竞态窗 → `synctest.Wait()` |
| TestOpenWithMaxStreams_ListenerSide_Reject（mux） | 0.20s | 0.00s | 气泡：200ms 占位 → `synctest.Wait()` 等 readLoop |
| TestMuxDataForUnknownStream（mux） | 0.10s | 0.00s | 气泡：100ms → `synctest.Wait()` |
| TestMuxFramePingPong（mux） | 0.05s | 0.00s | 气泡：50ms → `synctest.Wait()` |
| TestStream_Abort_ConcurrentWithPushData（mux） | 0.01s | 0.01s | 气泡：10ms → Wait（窗口构建更快更确定，计时不变因测试体极短） |
| TestKademliaPersistence_AsyncDebouncedSave（kad） | 0.21s | 0.00s | 气泡：去抖虚拟时钟，`<-time.After(2s)` 推进后单次断言 |
| TestKademliaPersistence_FlushJoinsInFlightDebounce（kad） | 0.20s+CI可flake | 0.00s | 气泡：join 在途回调回归；`<-time.After(debounce+10ms)` 推进使回调触发，FlushPersist 必须 join（R18 登记：synctest 气泡禁止 T.Parallel） |
| TestMeshTargetRefresher_SingleFlight（client） | 0.07s | 0.07s | 5ms 轮询 → WaitFor（消竞争窗口；计时不变，主体为真实 HTTP） |
| TestRateLimiter_RecoversAfterWindow（server） | 0.05s | 0.05s | 5ms 窗口滑动轮询 → WaitForBool（时间由真实限流窗口决定，不变） |
| TestRateLimiter_UpdateConfig_WindowChange（server） | 0.01s | 0.01s | 同上（2ms → WaitForBool） |
| cmd/sclient mesh 轮游两处（dial 重试/错误输出就绪） | 未单测计时 | — | deadline 循环 → WaitForBool（等真实 I/O 语言成立，计时同样由真 I/O 决定） |
| TestE2E_Binary_UploadDownloadDelete（e2e） | 未单测计时 | — | healthz 就绪 100ms 轮询 → WaitFor 30s（同 READY 模式） |
| TestDeadlineConn_SetReadDeadline_ClosesOnExpiry（pkg/sync/httptransport） | 0.08s | 0.00s | 气泡：net.Pipe 阻塞读实测为 durably blocked，deadline 到点走虚拟时钟；15s 真实墙钟窗口 → 30s 虚拟兜底，且「不得早于 deadline 返回」改为无容差断言 |
| TestDeadlineConn_SetDeadline_BothDirections（pkg/sync/httptransport） | 0.06s | 0.00s | 同上（SetDeadline 60ms）；读/写方向**各在独立 net.Pipe 上断言**：写方向对端不读，若 SetDeadline 未作用于写方向则阻塞的 Write 永不返回（窗口兜底变红）——同一连接上并发跑两方向做不到，任一方向到点都 forceClose 整个连接，会顺带唤醒未 arm 的方向 |
| TestDeadlineConn_WriteTimeout_ClosesOnExpiry（pkg/sync/httptransport） | 0.08s | 0.00s | 同上（活跃写超时 80ms；net.Pipe 阻塞写同样是 durably blocked） |
| TestDeadlineConn_ReadTimeout_ClosesOnExpiry（pkg/sync/httptransport） | 0.08s | 0.00s | 同上（活跃读超时 80ms） |

合计：被转换测试体 ~0.83s → ~0.02s（含本次 httptransport 4 例 ~0.30s → 0.00s）；更重要的是确定性
（等待从「猜已就位」变为「精确等到停驻」，deadline 场景从「墙钟窗口够不够」变为「虚拟时钟确定性推进」）。

> 边界修正（httptransport 4 例实测）：`net.Pipe` 是纯内存管道（内部为 channel 收发），其阻塞
> Read/Write **是** durably blocked（`synctest.Wait()` 正常返回，`time.AfterFunc` 到点由虚拟时钟推进）；
> 「真实 socket/HTTP 阻塞不算 durably blocked」的边界只针对真实网络 I/O。
> 另：`deadlineConn` 的 deadline 只在「下一次 Read/Write」arm（见 pkg/sync/httptransport/deadline.go），
> 故转换时把 `SetReadDeadline`/`SetDeadline` 移到读 goroutine 启动**之前**——旧写法依赖调度顺序，
> 读 goroutine 抢在设截止前阻塞就会走到无 timer 的路径上（墙钟窗口耗尽的根因之一）。

| TestE2E_MeshConnect_VirtualIP_UnannouncedPortRejected（e2e） | 6.95s | 6.83s | 两处（虚拟 IP 下发轮询 / 重拨窗） → WaitForBool；连接后的红线判断强迫在同步点之后（消除「眠后 fatal」的假红风险） |
| startHubSPROXY 就绪 helper（e2e_relay） | 未单测计时（helper） | — | healthz + hubNodesOK 双条件轮询 → WaitFor（同 READY 模式） |
| TestE2E_MeshConnect_AnnouncedService 数据面轮询 | 未单测计时 | — | 重试节奏型（连接后逐帧探活），登记为语义前提 |
| tcp_server / signaling_client 的 100ms 对端就绪间隔 | 受影响 ~0.2s | "等对端 Wait*/AcceptTCP 已阻塞"：真实 socket 阻塞不被气泡视为 durably blocked（已实证），无中途可观测点——登记为语义前提 |

## 有意保留（synctest 收益不明确，就地注明理由）

| 位置 | 原始耗时 | 决策 |
|---|---|---|
| TestE2E_CLI_CloudDownloadCancel | 9.86s → 9.33s | 三处轮询（任务状态含中途 Fatal / partial 落盘 / 取消清理）→ WaitFor；计时由真实下载/cancel 语义决定不变 |
| 位置 | 原始耗时 | 决策 |
|---|---|---|
| quic blockingStream 的 3 处（1ms 轮询自转 / 30ms cancel 时序 / 2ms 每轮） | 受影响 ~0.2s | fixture 与 withReadDeadline watcher 置 deadline 的取消路径深度耦合，门控化改造需引入「deadline 变更唤醒」生产级同步机制，成本/风险远超收益（尝试后双向死锁，已回退）——登记为语义前提 |
| pkg/tunnel/mesh 4 处（Lookup×2 / GatewayConnect 重试 / FullMesh peers 轮询） | 未单测计时 | **待深挖**：首次把 Lookup 等待改 WaitFor（叠加 ServicesOf 显式条件）后 TestRunNode_RegistersServicesAndRelays 3/3 复现「中继 echo 未回显""]」；HEAD 原版 ×3 稳定通过 ⇒ 改动确实决定性影响。已回退，待用「同步点/内部 hook」方案单独分析后重试，避免把「等待早了」误判为「flake」 |

注：`go test` 进程级墙钟含工具链固定开销（约 1-2s 起），与单个测试无关。

## 并行化（e2e 包墙钟）

| 阶段 | ./test/ 全包 -race 墙钟 |
|---|---|
| 串行基线 | 174s |
| 第一梯队（mesh_rr/node/vip/federation 家族 9 个测试 t.Parallel，均独立起 hub/节点且不用 t.Setenv） | 151s（−13%） |
| 第二梯队（cli harness 家族 + quota 共 14 个） | **61.7s（−65%）** |

约束：只用不依赖 `t.Setenv`（天然禁并行）与包级共享可变状态的测试；
每对测试独立起 hub/节点/子进程/临时目录。验证：全包 -race 两次全绿。

## 单元测试大规模并行（第二批）

- cmd/sclient/cfg/state、cmd/sproxy/cfg、internal/buildmeta/shortid/size/slogutil、
  pkg/accesskey/certmgr/cli/cloud/client/downloader/files/iostream/otp/pathguard/plugin/
  provider/quota/remote/socks5/sproxysig/storage/store/storage/capacity/store/file/
  sync/syncexec/syncmgr/httptransport/cloudfilename 等共 34 包、**+975** 处
  `t.Parallel()`，按"函数体内无 t.Setenv/t.Chdir"自动插桩并逐包验证。
- **回退（并行安全红线）**：pkg/files 的 `TestNewVersionID_*` 三例（文件头注释明示
  "非并行：独占重置包级 lastVersionID"，并行后同纳秒碰撞）与
  pkg/cloud 的 `TestCloudDownloadManager_StorageFullAfterDownload_DeletesAndReleases`
  （共享存储 fixture 竞态）
- 全量：-race ×3 无 FAIL；/test/ e2e 全包 57.9s。

## 串行测试登记（R18「测试并发注册」门禁）

门禁 `internal/archcheck/test_parallel_gate_test.go` **扫描全仓** `*_test.go` 的顶层
`func TestX(… *testing.T)`（参数名不限，`tt`/`_` 均可）：没有 `t.Parallel()`、又不含 `t.Setenv`/`t.Chdir`/`os.Chdir`、
且**函数体内**没有 `// sproxy:serial: <理由>` 标记的用例，计为「串行 Test」。
基线 `internal/archcheck/serial_budgets.tsv` 是**两层棘轮**（逐文件计数 + 全仓总数，只减不增）。

**存量快照（2026-09-15 实测）**：246 个文件 / 1743 个串行用例——门禁此前只扫到
`internal/archcheck` 自身（扫描根写成了 `"."`，而 go test 以包目录为 cwd；**修复前实测仅
9 个文件 / 14 个串行用例**），全仓用例从未被登记；本次把扫描面扩到全仓后一次性冻结为基线，
故这 1743 例按**类别**在此登记，不逐一具名。

**新增用例必须直接满足设计**：默认 `t.Parallel()`；确实不能并发的，在**函数体内**加
`// sproxy:serial: <短理由>`，再在本表登记理由（同时用
`ARCHCHECK_WRITE_BASELINE=1 go test -run TestSerialRatchetHelper ./internal/archcheck/`
重生成基线，基线行数只会因此**上升**，属显式决策）。

| 不可并发类别 | 典型原因 | 门禁如何处理 |
|---|---|---|
| 包级状态独占 | 重置包级变量/单例（`currentDir`、`cfgPtr`、`lastVersionID` 等） | 函数体内标记 + 本节登记 |
| 真实外部资源独占 | 固定端口、共享临时目录、独占文件句柄 | 函数体内标记 + 本节登记 |
| 进程级环境 | `t.Setenv`（Go 禁止与 `t.Parallel` 并用） | 自动豁免（无需标记） |
| 工作目录 | `os.Chdir` / `t.Chdir` | 自动豁免（无需标记） |

扫描口径（已用变异验证钉住，2026-09-15）：

- **识别面**：顶层签名 `func Test*(… *testing.T)`；参数名不限（`tt`/`_` 均可）、`{` 前允许空白。
  刻意不认 `testing.TB`、多参数与带接收者的方法。识别面由 `TestSerialGateRegexRecognizesTestForms`
  钉住——正则写窄会**静默漏检**（不进棘轮、永不报红），与「扫描根退回 `.`」是同一类失效。
- **只认函数体内的标记**（取签名后第一个 `{` 到匹配的 `}`）；写在 doc comment 上的标记不豁免。
- 函数体内出现 `t.Setenv`/`t.Chdir`/`os.Chdir` 的**子串**即豁免——包括仅出现在注释里的情形
  （方向保守：只会更宽松，不会误红）。**已知边界（2026-09-16 实跑发现）**：串行用例若在错误文案或注释里
  提到 `t.Parallel()`（实例：`TestSerialRatchet` 自己的 `t.Fatalf` 文案就写了「新增测试必须默认
  `t.Parallel()`」），会被子串判据当成「已并行」而不计入棘轮；同类子串判据还有 `t.Setenv`。
  修它需先改判据口径（剥离字符串/注释）再重生成基线，另片处理。
- **目录排除口径与其它全仓遍历门禁同源**（`repoScanSkipDir`，见 `internal/archcheck/repo_walk_test.go`）：
  隐藏目录 + `node_modules`/`build`/`vendor`/`dist`。口径**本身**由 `TestRepoScanSkipDir_PinnedSets` 铉住：
  本仓今天没有 `dist`/`node_modules`/`vendor`，删掉清单任一项四道门禁仍会全绿，只能显式断言。
- 覆盖探针（`TestSerialGateScanCoverage`）两类判据：**全局**用串行统计（串行文件数 `< 200` 或
  串行总数 `< 1000`，对应 246/1743 快照）；**逐子树**用**扫到的 `*_test.go` 文件总数**
  （2026-09-16 实测 `pkg/` 334、`cmd/` 58、`internal/` 24、`test/` 17、`web/` 10，下限取约 60%）。
  子树为何不用「仍有串行用例的文件数」：那个量会随棘轮收敛（整文件并行化后即从键集消失）**单向下滑**，
  与门禁期望方向相反 ⇒ 合法收敛会被误诊为「扫描面漏掉子树」。两类判据都只设下限（阈值单向下调）；
  若确因大规模删除/合并测试导致**合法收缩**，应同步下调阈值并在本节登记理由（失败信息里也带了这句提示）。
- 扫描面快照可自助核对：`go test -count=1 -v -run TestSerialGateScanCoverage ./internal/archcheck/`
  会打印「串行文件数 / 串行例数 / 逐子树测试文件数（含下限与快照）」。


## 串行测试登记（sclient context 重构 T3a，2026-09-19）

- `TestRootContext_FirstRunMigratesLegacy`：读写真实 XDG 配置目录并做迁移落盘，
  与其它读 xdg.ConfigHome 的测试并发会互相覆盖；函数体含 `// sproxy:serial:` 标记。
- `TestRootContext_NoConfig_NoLegacy_NoError`：依赖「XDG 配置目录为空」前提，
  并发测试可能在目录里创建文件破坏前提；函数体含 `// sproxy:serial:` 标记。

## 串行测试登记（优雅重启 L1，2026-09-25）

- `TestGracefulRestart_StartChildAndFileListener`：启动 helper 子进程 + 绑定真实端口，
  子进程调度与既有 runServer 用例互斥；函数体含 `// sproxy:serial:` 标记。
- `TestGracefulRestart_HandleRestartReadyThenDrain` / `TestGracefulRestart_HandleRestartTimeoutNoDrain`
  / `TestGracefulRestart_NoListenerNoSpawn`：操作包级 `restartListener`
  （`storeRestartListener`/`getRestartListener`），与其它优雅重启用例互斥；函数体含
  `// sproxy:serial:` 标记。

## 2026-09-27 协议盐测试串行登记

- `pkg/tunnel/ecdh_test.go`（TestSetSaltPrefix_Custom/DefaultReset、TestSetProtocolSalts_Apply、
  TestDeriveProtocolSalts_Deterministic）：**共享全局盐变量**（SetProtocolSalts/SetSaltPrefix 全局副作用），
  并行会相互污染派生结果——登记不可并发。
- `cmd/sclient/root_test.go`（TestPersistentPreRun_ProtocolSaltKey_*）：同样因全局盐副作用，不可并行。

## 2026-10-02 PikPak 账号子命令串行登记

- `cmd/sproxy/pikpak_account_cmd_test.go`（TestNewCmdPikpakAccount_Manage /
  TestNewCmdPikpakAccount_AddDuplicateCrossProcess）：覆盖包级全局 `pikpakSecretsDirOverride`
  （指向不同临时 secrets 目录），两条用例与本身/其它用包级变量互斥——登记不可并发，
  函数体含 `// sproxy:serial:` 标记。

## 2026-10-04 PikPak hybrid 测试串行登记

- `pkg/volume/ext/pikpak/hybrid_test.go`：`TestHybridDownload_ParallelPools`（**绝对时序断言**
  <600ms）并行下受 `-race`/并发负载影响会 flake——登记串行（预算 1；time.Sleep 2 处经
  sleep 棘轮登记）。
- `cmd/sproxy/keyframe_register_test.go`（#733 并入，2026-10-05 补登记）：`TestRegisterKeyframeBackend_Unregister`
  与 `TestRegisterKeyframeBackend_DualRegistration` 写**共享 blocklet provider 注册表**（包级全局）——
  并行会交叉污染注册状态，登记串行（预算 2）。

## 2026-10-05 全局能力可测试化策略落地（#734 CI 修复）

背景：#734 首次合入时把 30+ 新 pikpak 测试一次性登记串行，棘轮基线被顶到 1810；
CI 的 `pull_request` merge 树（分支并入 master 后）又带进 master 侧未显式登记的串行
（keyframe 2 例），slack 被吃光 → `TestSerialRatchet` 红灯（1812 > 1810）。

处置（落实用户 2026-10-05 指示「新测试必须思考是否真的要串行」）：
- 逐用例分析并行安全性：hybrid_test.go 21 例（自建 httptest + t.TempDir，无全局依赖）、
  hybrid_register_test.go 2 例、hybrid_resume/scratch_concurrent/probe_boundary/resolver 各例、
  pikpak_test.go `TestAPI_DeletePermanent`——全部加 `t.Parallel()`；
- 全局依赖用例改用**本地实例注入**（全局能力可测试化三件套）：`TestHybridDownloader_RegisterPriority`
  从写全局 `downloader.DefaultRegistry` 改为 `downloader.NewRegistry()` 本地实例；
  新增 `pikpak.RegisterHybridDownloader(reg, cfg)` 注册入口（生产传全局、测试传本地）；
- 保留串行仅 2 处：ParallelPools（绝对时序）+ keyframe 2 例（共享 provider 注册表）；
- 基线归位 1781，与 master 同步（rebase），CI merge 树自洽。


<!-- 2026-10-12 封装卷 wrapper 生产装配用例（#772） -->

## 2026-10-12：封装卷 wrapper 生产装配用例（`cmd/sproxy/secret_register_test.go`，串行 +1）

`TestEnclosureWrapper_Secretdata_ProductionAssembly` 钉住「wrapper 类型（`type: secretdata`）+
`extra.source` 的生产装配路径可用」（wrapper 注册 / 声明解析 / 补装 / 写读往返 / 真值报告）。
> 历史注：该用例早期夹具写作 `type: enclosure` + `wrapper: secretdata`；`type: enclosure` 已
> **纯内部化**（不是可写卷类型），夹具随之改为 wrapper 类型声明。
**不可并行的理由（与既有 `TestRegisterSecretVolumeBackends_Unconditional` 同类）**：
它必须走**生产注册路径**，因此触及两个进程级单例——① `enclosure` wrapper 注册表（全局，
`RegisterWrapper` 重复注册 panic，只能进程内注册一次）；② `secretDataSet`（`atomic.Pointer`，
`setupSecretBackends` 会把**本用例的卷集**存入、收尾再清空）。与同包用例并行时，两者的
`Store/Load` 会互踩（读到对方的卷集 → 密钥解析到不存在的 secrets 卷），故显式串行。
注册表已同步登记 `internal/archcheck/serial_budgets.tsv`（总数 1784 → 1785，无新增 slack）。

## 2026-10-13 卷感知工作目录用例（`cmd/sclient/cd_test.go` 5 → 8）

新增 3 个**串行**用例（`TestCd_VolumeURI_SwitchesVolumeAndDir`、`TestCd_LeadingSlashReturnsToLocalVolume`、
`TestCd_Pwd`）。**不可并行的理由**：`cd` 的工作目录（含所在卷）落在 **XDG 缓存**里，用例必须用
`setCacheDirForTest`（内部 `t.Setenv("XDG_CACHE_HOME", tmp)` + `xdg.Reload()`）把缓存隔离到临时目录，
否则会读写**真实用户缓存**；`t.Setenv` 与 `t.Parallel` 互斥（Go 测试框架限制），故显式串行。
注册表已同步登记 `internal/archcheck/serial_budgets.tsv`（`cmd/sclient/cd_test.go 5 → 8`）。

## 2026-10-20 审计面板筛选 e2e（`web/e2e/audit_export_notify_test_e2e_test.go` 2 → 3）

新增 1 个串行用例 `TestAudit_FilterRows`（S4 审计面板筛选的真浏览器验证：筛选栏渲染 / 输错动作
收敛为 0 条 / 清除恢复）。**不可并行的理由**：与同文件既有 2 例同因 —— ① Playwright 真浏览器夹具
（`pageFixture`）每次都要启 chromium；② 用例依赖「**先播种审计事件、后读面板**」的时序，而审计服务是
进程内共享状态（播种的 `POST /api/notify/test` 与面板读取都落在同一个 `audit.Service`）⇒ 并发会互相
干扰（读到对方/读到空表）。注册表已同步登记 `internal/archcheck/serial_budgets.tsv`（该文件 2 → 3，
总数 +1，无其它文件 slack 变化）。

## 串行 Test 登记理由（R14 新增）

| 文件 | 串行数 | 理由 |
| --- | --- | --- |
| `pkg/cloud/recovery_queued_test.go` | 4 | 三个用例都直接操作**同一个** `CloudDownloadManager` 的 `m.tasks`/磁盘状态（恢复路径会启动下载 goroutine 并读写共享 map）⇒ 并行会互相污染状态；且恢复语义本身依赖「单进程内唯一一次 RecoverAll」。已用**唯一 URL + 独立 tempdir** 降低耦合，但不满足 `t.Parallel()` 的隔离前提。第 4 个（`TestSubmitAndStart_PersistsSubmitted`）验证「提交时是否把 submitted 落盘」，同样操作共享 manager 状态。 |
| `test/e2e_restart_cloud_test.go` | 2 | 每个用例都要**构建并启动真实 sproxy 子进程**、占用固定端口与独立存储根，并做 kill -9 / 重启；端口与进程生命周期是全局资源，不能并行。 |
| `test/e2e_restart_quota_test.go` | 3 | 同上（真实进程 + 固定端口 + 存储根）；其中配额用例还需控制「等待/释放」时序。 |
| `test/e2e_restart_matrix_test.go` | 2 | 状态矩阵用例：每个子用例都构建/启动真实 sproxy 子进程并独占存储根与端口（全局资源，不可并行）。 |
| `test/e2e_restart_upload_test.go` | 1 | 分块上传中断用例：真实进程 kill -9 + 同根重启，端口/进程生命周期独占。 |
| `pkg/audit/prod_hardening_test.go` | 3 | 审计读面/冷段/续号：需构造多段大文件与真实 FileJournal（段文件 O(size) 写盘），无固定等待、收益为零。 |
| `pkg/cloud/prod_hardening_test.go` | 2 | 孤儿组恢复 / 启动恢复取消：依赖 CloudDownloadManager 内部状态机（含幂等守卫）与共享持久化目录，不可并行。 |
| `pkg/cloud/prod_hardening_transfer_test.go` | 1 | 转存残骸自清：注入式 FS 状态机（armed/重试）与用例一一对应，不可并行共享。 |
| `pkg/server/prod_hardening_test.go` | 2 | 归档 checksum / 组归档删除配额：需真实 storage.Root、临时根与 owner 配额池（共享 `h.globalPool`），用例级独占。 |
| `pkg/volume/secretdata/prod_hardening_test.go` | 4 | 卷锁/在途写并发/目录 meta 仲裁/Eof 校验：均在**同一时间轴**上构造真实卷状态（死锁用例带 10s 超时断言、仲裁用例需真实 mtime 次序），并行会互相干扰计时与全局 slog 输出。 |
| `cmd/sproxy/root_extra_test.go` | 5 | 进程级装配/启动用例（注册全局后端类型、信号停机等），共享包级全局状态，不可并行。 |
| `cmd/sclient/share_test.go` | 3 | CLI 集成（httptest 捕获 POST body）：需独占测试服务器与 cobra 命令状态，不可并行。 |
| `cmd/sclient/trash_test.go` | 6 | CLI 集成（httptest 捕获请求 URL）：同上，且断言「请求路径参数」需串行采集。 |

## 2026-10-27 云下载 meta_extra e2e（`web/e2e/cloud_audit_e2e_test.go` 4 → 5）

新增 1 个串行用例 `TestCloudDownload_MetaExtra`（云端下载预览填自定义元数据 JSON → 确认提交 →
断言请求体 meta_extra 与输入一致）。**不可并行的理由**：与同文件既有 4 例同因 —— Playwright 真浏览器
夹具（`pageFixture`）每次都要启 chromium 且依赖「先播种 → 后读面板/任务」的进程内共享时序（cloud
任务服务为进程内共享状态），并发会互相干扰。注册表已同步登记 `internal/archcheck/serial_budgets.tsv`
（该文件 4 → 5，总数 +1）。
