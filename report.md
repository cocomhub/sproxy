# REPORT.md —— NAT 穿透失败告警（roadmap 11.1-①）

## 状态

**DONE**（PR #595 已创建，CI 进行中）

## 分支 / 提交

- 分支：`feat/alert-nat`（worktree `.worktrees/feat/alert-nat`，已 rebase origin/master）
- 提交：`b5ec1ae21` — `feat(alerts): NAT 穿透失败告警事件源（roadmap 11.1-①）`
- PR：https://github.com/cocomhub/sproxy/pull/595

## 改动文件（9 个）

### pkg/server（AlertEngine source + 事件接口）
- `pkg/server/alerts.go`：新增 `SourceNATFailure = "nat_failure"` 常量（与既有 source 字符串同风格）+ `OnNATFailure(ctx, peer, detail)` / `OnNATRecovered(ctx, peer)` 事件接口——key=`nat_failure\x00<peer>`，per-peer 去抖；同 peer 后续拨号成功自动发恢复通知（recover 内部已判非 firing 即 no-op，每次成功都可安全调用）；`AlertRule.Source` 注释补 nat_failure 取值。
- `pkg/server/alerts_nat_test.go`（新）：FireAndRecover（失败告警 → 同 peer 重复去抖 → 恢复通知）、KeyPerPeer（peer A/B 状态独立）、NoRuleSilent（无 nat_failure 规则零通知）。

### cmd/sproxy（main 装配层，防包环）
- `cmd/sproxy/cloud_exit.go`：新增 `withNATAlert(dial, engine, peer)` 包装——失败 → OnNATFailure、成功 → OnNATRecovered；**返回原 conn/err（告警是旁路副作用，绝不吞错，出口失败仍 fail-closed 向上传播）**；engine==nil 直接返回原 dial（零开销零变化）。
- `cmd/sproxy/root.go`：挂点① `buildCloudExitDial` 产物（云端下载经 mesh 出口，peer=cfg.CloudDownloadExitNode）+ 挂点② `startMeshNodeRoleWithCreds`（mesh node 角色拨号，peer=node id；RunNode 断线退避重连的每次会话失败/成功接入，`h.AlertEngine()` 注入）。
- `cmd/sproxy/cloud_exit_alert_test.go`（新）：FailureFiresAndPropagates（失败发告警 + 错误原样传播）、SuccessRecovers（先失败后成功→恢复通知）、NilEngineNoOp（nil engine 直通零变化）。
- `cmd/sproxy/mesh_node.go` / `mesh_node_test.go`：startMeshNodeRoleWithCreds 签名加 alertEng 参数（既有测试适配传 nil）。

### docs
- `docs/config.md`：补 `alerts.enabled` / `alerts.rules[]` / `alerts.poll_interval` 配置段（nat_failure source 语义：per-peer 去抖 + 恢复）。
- `docs/roadmap.md`：11.1-① 里程碑与源码证据表 → 已落地。

## 测试证据（一行小结）

TDD 红灯先行（测试引用未实现的 SourceNATFailure/OnNATFailure/OnNATRecovered → 编译失败）→ 实现后 6 个新用例全绿（pkg/server 3 + cmd/sproxy 3）；变异验证命中（删 OnNATFailure 内 fire → 红、删 OnNATRecovered → 红，已还原）。

## 变异验证（删关键逻辑 → 测试红 → 还原）

- 删 `OnNATFailure` 内 fire → `TestAlertNATFailure_FireAndRecover` / `KeyPerPeer` 红（3.00s 超时告警未发出）。
- 删 `OnNATRecovered` 内 recover → `TestAlertNATFailure_FireAndRecover` 红（恢复通知未发出）。
- 已还原，全绿。

## 本地验证（全绿）

```
go build ./...                                   OK
go test -count=1 -race ./pkg/server/             OK（42.9s）
go test -count=1 -race ./cmd/sproxy/             OK（4.2s）
go test ./internal/archcheck/                    OK（R18 串行棘轮 / 覆盖探针）
golangci-lint run ./pkg/server/... ./cmd/sproxy/ 0 issues
gofmt -l / goimports -l                          干净
```

## CI 状态

- PR #595：https://github.com/cocomhub/sproxy/pull/595
- 等 CI 全绿后由主 agent squash 合并。
