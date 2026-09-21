# REPORT: 同步任务失败单文件重试（sync-retry）

## 交付内容
1. **pkg/syncmgr/retry.go**（新）：
   - `RetryFiles(ctx, id, owner, files)`：从任务 Results 取 error/verify_failed 条目；
     构造重试子任务（方向/remote/src/dst/冲突/校验策略与源任务一致，Include=[失败文件 glob]
     精确限定，Recursive=false）→ SubmitAndStart 复用引擎单文件路径（不重跑整个任务）；
     子任务终态后 `backfillRetryResults` 回写原任务 Results 对应条目；files 空=全部失败，
     指定非失败/不存在=幂等跳过（skipped 明细）
   - `RetryResult{Retried []RetryItem, Skipped []string}` 明细化响应
   - `InjectResultsForTest`：测试专用预置失败结果（供 pkg/server handler 测试）
2. **pkg/server/sync_handler.go + routes.go**：`POST /api/sync/tasks/{id}/retry` handler
   （authMiddleware 包装，localMux + srvMux 双挂；跨 owner 404 防枚举；body MaxBytes 1MiB）
3. **pkg/client/sync.go**：`RetrySyncTaskFiles(ctx, id, files)`（doJSON POST retry 端点）
4. **cmd/sclient/sync.go**：`sync retry <task-id> [--files a,b]` 子命令
   （表格：重试成功/失败/跳过 + 明细；--json 结构化 {retried, skipped}；--files 逗号分隔）
5. **docs/api.md**（retry 端点）+ **docs/cli.md**（sync retry 命令，R15 门禁 flag 登记）

## 验证证据
- **TDD 红灯先行**：RetryFiles 未定义 → 编译红；实现 → 绿
- **变异命中×3**（每条证明测试能抓真 bug）：
  1. 忽略指定 files 重试全部 → TestRetryFiles_InvalidFileSkipped 红（ok.txt/ghost.txt 被错误重试）
  2. srvMux retry 路由缺失 → TestSyncAPI_RetryTask_PartialFiles 404 红
  3. printSyncRetryResult 成功/失败统计算反 → TestSyncCmd_Retry_CallsAPI 红
- `go test -count=1 -race ./pkg/syncmgr/ ./pkg/server/ ./cmd/sclient/` 全绿
  （pkg/server 全量复跑 43.5s ok；首轮 FAIL 为既有 race flake，TestSyncAPI 单独跑绿）
- `make lint` 0 issues；`make deadcode-check` PASS；`go test ./internal/archcheck/` 绿
- gofmt/goimports 干净；无 Co-authored-by

## 残余
- RetryFiles 同步等待子任务终态（60s 有界超时）——超时返回 failed 快照诊断，不阻塞
- 重试子任务本身失败（如源文件仍缺失）→ RetryItem.Action=error 透传，原任务 Results 回写
- 连续同步 watch（roadmap 4.3 P1 主项）仍待做（事件流前置 #433/#437 已就位）
