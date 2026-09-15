# 固定等待清理台账（方案 B：确定性时钟/条件等待）

> 目标：消灭测试中的**真实**固定等待（`time.Sleep` 定值等待）。
> 手法：纯逻辑 fixture 用 `testing/synctest` 气泡（虚拟时钟，零真实耗时）；
> 条件可观测的轮询改 `testutil.WaitFor/WaitForBool`（30s 上限）；真实 I/O 的 fixture
> 改 channel 门控。synctest 适用边界见 docs/superpowers/learnings 或 MEMORY.md
> 「synctest 气泡实证」：只适用纯内存（真实 socket/HTTP 阻塞不算 durably blocked）。

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
