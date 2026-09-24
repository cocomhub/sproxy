# REPORT —— 客户端能力线（L2）

## 状态：DONE

- **commit**：`ade35f909`（分支 `feat/client-cap`，已 rebase origin/master f46730efc）
- **PR**：https://github.com/cocomhub/sproxy/pull/587
- **测试小结**：`go test -count=1 -race ./pkg/server/ ./pkg/client/ ./cmd/sclient/` 全绿 + `./internal/archcheck/` 全绿 + `golangci-lint` 0 issues + `gofmt -l`/`goimports -l` 无输出
- **CI 状态**：PR 已创建，等待 CI 全绿（主 agent 合并）

## 改动文件

| 文件 | 说明 |
|---|---|
| pkg/server/du.go（新） | `GET /api/du`：目录递归统计（dirs/files/size），ACL 经 locateForRead 收口 + 404 防探测，遍历 IO fail-closed |
| pkg/server/du_test.go（新） | fixture 树精确统计、功能桶/魔法目录跳过（变异验证）、400/404 边界、默认卷回落 |
| pkg/server/routes.go | du 路由三处同步：主 mux fileRouteRead + localMux 裸注册 + isFileGroupedRoute/isReadOnlyFileRoute 成员 |
| pkg/server/handlers_localmux_test.go | localMuxPatterns 补 `GET /api/du` |
| pkg/client/du.go（新）+ du_test.go（新） | `FileClient.Du()` + mock 契约测试（禁共享 client） |
| pkg/client/volume.go + volume_ops_test.go（新） | `CopyVolume/MoveVolume/RebalanceVolume` + mock 契约测试 |
| cmd/sclient/du.go + du_test.go（新） | `sclient du [path]` / `df`（复用 /api/stats 卷/磁盘/配额水位）；随 cd/--vol |
| cmd/sclient/batch_run.go + batch_run_test.go（新） | `runBatchConcurrent`（worker 池 + 信号量 + 保序 + SIGINT Skipped + ProgressSink），6 例单测 |
| cmd/sclient/volume_ops.go + volume_ops_test.go（新） | `volume copy/move/rebalance` 子命令 + 集成测试 |
| cmd/sclient/root.go、volume.go、volume_test.go | du/df 注册、volume 命令挂 copy/move/rebalance |
| docs/cli.md | du/df/volume copy-move-rebalance 登记（R15） |
| docs/roadmap.md | 11.7-①/11.7-④/11.5-⑨/A1/A4 置「已落地」 |

## 验证证据

- 服务端 du：TestDUDirRecursive（dirs=1/files=2/size=33 精确）、TestDUBucketExcluded、
  TestDUInvalidPath（穿越/绝对路径 400）、TestDUMissingPath404、TestDUBucketSkipMutation（变异钉住桶跳过）；
- pkg/client：TestFileClient_Du{,_Root,_ServerError,_SuccessFalse}、TestFileClient_{Copy,Move,Rebalance}Volume；
- cmd/sclient：TestNewCmdDu/TestNewCmdDF 集成、TestVolumeOps_{Copy,Move,Rebalance}Cmd；
- 并发批量：TestRunBatchConcurrent_{OrderPreserved,ConcurrencyPeak,FailureIsolated,ProgressMatches,SerialDegradation,CancelMarksSkipped}（-race 全绿）；
- 门禁：`go test ./internal/archcheck/`（含 R15 docs-cli、R18 串行棘轮）全绿。

## 变异命中

- du 桶跳过逻辑（功能桶/魔法目录计入 → 计数超预期红）由 TestDUBucketSkipMutation/TestDUBucketExcluded 钉住；
- 并发批量信号量删除 → ConcurrencyPeak 红；保序破坏 → OrderPreserved 红；SIGINT 继续执行 → CancelMarksSkipped 红。

## 残余风险

- du 默认卷回落时对「全视图未命中且默认卷授权」的场景先 stat 探测再返回 200/404，
  与 download 回落语义一致；多卷 ACL 排除面的 404 防探测已由显式 volume 分支覆盖。
- 并发批量进度条（TTY 渲染）按设计仅提供 ProgressSink 接口与 no-op 实现，未做真实 TTY 进度条
  （flag 接线留给后续；接口与并发语义已完整）。

## 下一步

- 等 PR #587 CI 全绿 → squash 合并 → 删分支；
- backup/export CLI（11.7-⑤）另片（服务端卷导出端点未存在，先补服务端）。
