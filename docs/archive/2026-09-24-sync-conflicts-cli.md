<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sclient sync conflicts：同步冲突列表与解决 CLI — 归档

> **归档说明**：原文 `docs/designs/2026-09-24-sync-conflicts-cli.md` 已于 2026-11-14 归档。
> CLI 用法见 `docs/cli.md` §sync；服务端 API 见 `docs/api.md` §同步冲突（sync conflicts）。
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景 / 目标
服务端已有三方合并冲突索引（`ConflictIndex`）与三个 handler（list / get / resolve），但 sclient 无对应
入口（`cmd/sclient/sync.go` 的 `NewCmdSync` 只有 push/pull/both/retry/watch/schedule）。
目标：`sclient sync conflicts list` + `resolve <id> --strategy ours|theirs|manual [--content <text>]`，
**服务端零改动**（原样使用既有 handler 与 ConflictIndex）。

## 定案与取舍（仍有效）
1. **本地 fail-fast 优先于半途网络往返**：strategy 归一与 manual 缺 content 校验放在 `NewClient` **之前**
   （错误不出网），把服务端 400 语义前移。
2. **`success:false` 的 200 响应显式校验**（对齐 `ListSyncTasks` 审查 M-3 模式）：不静默返回空。
3. **列表只展示服务端语义（未解决）**：**不本地过滤 resolved**（防与服务端语义漂移）。
4. **列表不含冲突内容行**（Ours/Theirs）——防刷屏；详情（含内容行）留待后续 `get` 命令。
5. **manual 解决依赖 `--content` 显式提供**（文本冲突场景够用）；二进制内容的 manual 解决（`--file`/stdin）
   留后续片，文档标注限制。
6. **`ours`/`theirs` 时 `--content` 被忽略**（不报错，文档标注），避免无谓失败。
7. **不动 `OutputFormatter`（13 方法接口）**：命令内 `--json` 分支直接输出，对齐 `printSyncTaskResult` 先例。

## 外部契约 / 踩坑
- `SyncConflictItem` 字段/`json` tag 必须与 `syncmgr.ConflictItem` **逐一对齐**（漏字段/错 tag = 静默丢数据）。
- resolve 端点 `/api/sync/conflicts/{id}/resolve`；body `{choice, content}`；响应 `{success, path}`。
- 错误透传：404（冲突不存在）/ 400（非法 choice）/ 500（写回失败）均非零退出。
- 服务端 `writeConflictFile` 固定按空 owner（anonymous 租户）解析落盘路径——CLI 不感知，属服务端既有语义。

## 落地指针
- `pkg/client/sync.go`：`SyncConflictItem` / `ListSyncConflicts` / `ResolveSyncConflict`。
- `cmd/sclient/sync_conflicts.go`：`newCmdSyncConflicts`（list / resolve）；`cmd/sclient/sync.go` 注册一行。
- 测试：`cmd/sclient/sync_conflicts_test.go`、`pkg/client/client_test.go`。
