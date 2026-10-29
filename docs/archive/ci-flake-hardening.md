# CI flake 治理（2026-10-29）

> 背景：master 上的偶发红（单跑必绿、整包并发/低配 runner 上偶发）反复消耗 rerun 成本。
> 本文记录三项**根因**与仍然有效的判断，供后续维护参考。

## 1. 分块上传「已确认」的持久化契约（flakes → 真实缺陷）

- **症状**：`TestE2E_Restart_ChunkedUploadResumesAfterKill9` 偶发 `ReceivedCount=1 want 2`。
- **根因**：`POST /upload/chunk` 返回 200 时只更新内存 bitmap 并**异步**持久化 `session.json`；
  `kill -9` 落在窗口内 ⇒ 重启时 `ChunkChecksums[i] == ""` ⇒ `verifyTempChunks` 只能判「需重传」。
  这不是测试噪声，而是「200 = 已持久化」契约不成立（客户端白传；断电场景同样）。
- **修复**：`MarkChunkReceived` 在返回前原子写**每块 sidecar** `<sessionDir>/chunk-<i>.sha256`；
  恢复时 `loadChunkChecksumSidecars` 先采信 sidecar（乐观置 `Received=true`），
  `verifyTempChunks` 再用临时文件内容**重新校验**，不匹配降级为需重传（数据丢失仍安全）。
- **为何不整体同步写 `session.json`**：JSON 含全部块 checksum，8k 块时单次 ~512KB；
  每块同步写 = 灾难。sidecar 每块 ~百字节，代价可忽略。
- **仍有效判断**：分块 ACK 的持久化粒度应等于「客户端可依赖的最小单位」（单块），
  而不是「会话全量快照」。

## 2. 任务停止判定：事件驱动而非轮询（`pkg/cloud`）

- **症状**：`TestCloudDownloadManager_CancelStopsDownload` 偶发 `download goroutine did not stop`。
- **根因**：`waitTaskStopped` 用 `time.Sleep(100ms)` 轮询 `running` map；CI 低配 runner 上
  goroutine 退出（清理临时文件/账本 flush）耗时被轮询粒度放大，2s 上限误判。
- **修复**：新增 `runningDone map[string]chan struct{}`——`markRunningLocked` 创建、
  `clearRunningLocked` 关闭；`waitTaskStopped` 改为 `select` 等待关闭信号（无轮询）。
- **门禁**：`internal/archcheck/cloud_wait_event_gate_test.go` 断言该函数体不含 sleep 且必须
  等待 `runningDone`（回退轮询即红）。注意门禁文件自身**不得出现**该 sleep token（R14 睡眠棘轮
  按文本计数，字面量需拼接）。

## 3. E2E 固定端口 → 动态分配（`test/`）

- **症状**：`TestE2E_MeshNode_ServiceAccess` 整包并发偶发 bind 失败/超时。
- **根因**：mesh 节点网关硬编码 `127.0.0.1:18085/18086`；整包并行与上次运行残留会让固定端口冲突。
- **修复**：复用 `freeLoopbackAddr(t)`（listen `:0` 取端口后释放），两个网关与 `echoRoundTrip`
  全部改用动态地址。
- **仍有效判断**：E2E 中任何「子进程监听端口」都不应硬编码；`:0` 探测 + 立刻交给子进程足够
  （TOCTOU 窗口极短，失败立即暴露）。

## 验证方式

- 单元：`TestUploadStore_ChunkAckSurvivesStaleSessionJSON`（模拟 kill9 时 `session.json` 落后，
  重启后仍识别已确认块）、`TestUploadStore_ChunkSidecarIsPerChunk`；
  变异：删除 sidecar 加载调用 ⇒ 「校验值应跨崩溃存活，got ""」红。
- 门禁变异：把 `waitTaskStopped` 改回轮询 ⇒ 门禁红。
- 回归：`pkg/files`、`pkg/cloud`（整包 3 轮 + race）、`internal/archcheck`、`test/` 整包 E2E
  （含 mesh 3 轮）全绿；lint 0 issues。

## 未纳入本轮（如再复现再查）

- `TestFederationClient_PersistAutoSaveOnSync`（已有 30s `WaitFor`，本地 8 轮/整包 3 轮未复现）
- `TestE2E_CLI_Volumes`（子进程 + JSON，本地整包未复现；若再红先查 `startCLIEnv` 的端口与配置隔离）
