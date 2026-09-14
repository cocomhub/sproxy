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

合计：被转换测试体 ~0.53s → ~0.02s；更重要的是确定性（等待从「猜已就位」变为「精确等到停驻」）。

注：`go test` 进程级墙钟含工具链固定开销（约 1-2s 起），与单个测试无关。
