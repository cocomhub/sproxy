# 设计：sclient sync conflicts —— 同步冲突列表与解决 CLI

> 日期：2026-09-24 ｜ 范围：sclient 新增 `sync conflicts` 子命令族（服务端零改动）

## 1. 背景 / 目标

- 现状（已核实）：
  - 服务端 `GET /api/sync/conflicts`（`pkg/server/sync_handler.go` `syncListConflicts`）
    返回 `{success, conflicts: []*syncmgr.ConflictItem}`（未解决、时间升序）；
  - `GET /api/sync/conflicts/{id}`（`syncGetConflict`）返回单条（含已 resolved 历史）；
  - `POST /api/sync/conflicts/{id}/resolve`（`syncResolveConflict`）body
    `{"choice":"ours|theirs|manual","content":"..."}` → `{success, path}`；
  - 冲突索引 `ConflictIndex`（`pkg/syncmgr/conflict_index.go`）条目字段：
    `ID/Path/HunkCount/BaseSHA/OursSHA/TheirsSHA/Ours[]/Theirs[]/Timestamp/Resolved/ResolveAt`；
  - sclient 侧：`pkg/client/sync.go` 无冲突 API；`cmd/sclient/sync.go` 的
    `NewCmdSync` 只有 push/pull/both/retry/watch/schedule。
- 目标：`sclient sync conflicts list` 列出未解决冲突；
  `sclient sync conflicts resolve <id> --strategy ours|theirs|manual [--content <text>]`
  提交解决策略并写回文件。

## 2. 组件与接口

### 2.1 pkg/client（`pkg/client/sync.go` 追加，对齐 SyncTask 风格）
```go
// SyncConflictItem 对齐 syncmgr.ConflictItem JSON（列表/详情共用）。
type SyncConflictItem struct {
    ID        string   `json:"id"`
    Path      string   `json:"path"`
    HunkCount int      `json:"hunk_count"`
    BaseSHA   string   `json:"base_sha"`
    OursSHA   string   `json:"ours_sha"`
    TheirsSHA string   `json:"theirs_sha"`
    Timestamp int64    `json:"ts"`
    Resolved  bool     `json:"resolved"`
    ResolveAt int64    `json:"resolve_at,omitempty"`
    // Ours/Theirs 内容行（list 不展示内容，避免刷屏；详情可后续加 get 命令时使用）
    Ours   []string `json:"ours,omitempty"`
    Theirs []string `json:"theirs,omitempty"`
}

func (c *FileClient) ListSyncConflicts(ctx context.Context) ([]SyncConflictItem, error)
  // GET /api/sync/conflicts；显式校验 success（对齐 ListSyncTasks 审查 M-3 模式）
func (c *FileClient) ResolveSyncConflict(ctx context.Context, id, choice, content string) error
  // POST /api/sync/conflicts/{id}/resolve；body {choice, content}
```

### 2.2 cmd/sclient（新文件 `cmd/sclient/sync_conflicts.go`）
- `newCmdSyncConflicts(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command`：
  父命令 `conflicts`（RunE 帮助），挂 `list` 与 `resolve` 两个子命令；
  `NewCmdSync`（`cmd/sclient/sync.go`）末尾加一行 `cmd.AddCommand(newCmdSyncConflicts(factory, ios))`。
- `list`：`cobra.NoArgs`；`svc.ListSyncConflicts` → 表格输出
  （ID / PATH / HUNKS / OURS_SHA[:8] / THEIRS_SHA[:8] / TS）；空列表输出
  `无未解决冲突`。`--json` 输出 `{conflicts: [...]}`（根 flag，同 sync 既有惯例）。
- `resolve`：`cobra.ExactArgs(1)`（id）；
  `--strategy`（ours|theirs|manual，必填）+ `--content`（manual 必填）。
  **纯参数校验 fail fast 在 `NewClient` 之前**（对齐 `newCmdSyncDirection` 审查 M-2 模式）：
  - strategy 归一：`ours|theirs|manual` 直接映射 choice；非法值报错并列出可选值；
  - strategy=manual 且 `--content` 空 → 报错（服务端 400 语义前移到 CLI，防半途网络往返）；
  - strategy=ours|theirs 时 `--content` 被忽略（不报错，文档标注）。
  成功输出 `已解决冲突 <id>: <path>`；`--json` 输出 `{success:true, path}`。
  `ResolveSyncConflict` 返回错误（404 冲突不存在 / 400 非法 choice）时
  `fmt.Errorf("解决同步冲突失败: %w", err)` 非零退出。

## 3. 数据流

```
sclient sync conflicts list
  → svc.ListSyncConflicts → GET /api/sync/conflicts（authMiddleware；conflictIndex nil → 400）
  → 表格/JSON 输出（仅未解决，服务端已过滤）

sclient sync conflicts resolve <id> --strategy ours
  → 本地校验 strategy → POST /api/sync/conflicts/{id}/resolve {choice:"ours"}
  → 服务端 ConflictIndex.Resolve(id,"ours") 取快照 → writeConflictFile 写回
  → {success, path} → CLI 输出已解决
```

## 4. 错误处理

- strategy 非法 / manual 缺 content：本地报错，非零退出（不出网）。
- 服务端 400（未配置 sync conflicts / invalid choice / manual 缺 content）与
  404（冲突不存在）：包装透传，非零退出。
- 500（写回失败）：透传服务端错误信息，非零退出。
- 服务端返回 `success:false` 的 200 响应（未来语义变化）：`ListSyncConflicts`
  显式校验并报错（M-3 模式），不静默返回空。

## 5. 测试 + 变异点

- **pkg/client 单测**（`pkg/client/client_test.go` newMockServer 扩展）：
  - ListSyncConflicts：mock `GET /api/sync/conflicts` 返回 2 条 → 断言解析
    ID/Path/HunkCount/SHA/ts（变异：漏字段 / json tag 错 → 红）；
  - ListSyncConflicts success=false → 报错（变异：删 M-3 校验 → 红）；
  - ResolveSyncConflict：断言请求 path 含 `/resolve`、body choice 正确、
    成功返回 nil（变异：choice 映射错 / 端点拼错 → 红）。
- **cmd/sclient CLI 测试**（httptest mock + CaptureStdout）：
  - `list` 表格含冲突 ID 与 path（变异：行格式漏 path → 红）；
  - `list` 空列表输出"无未解决冲突"（变异：删空分支 → 红）；
  - `resolve <id> --strategy theirs` 输出"已解决"/path（变异：strategy→choice
    映射错（如 ours/theirs 互换）→ 红）；
  - `resolve` 缺 id → cobra.ExactArgs 报错（变异：放宽 Args → 红）；
  - `resolve --strategy manual` 无 `--content` → 本地报错（变异：删校验 → 红）；
  - `resolve --strategy bogus` → 本地报错列出可选值（变异：删 fail-fast → 红）。

## 6. 片划分

- **片 1**：`pkg/client/sync.go` 追加 `SyncConflictItem` +
  `ListSyncConflicts`/`ResolveSyncConflict` + client 单测（含 mock 扩展）。
- **片 2**：`cmd/sclient/sync_conflicts.go` 新文件 + `NewCmdSync` 一行注册 +
  CLI 测试 + `docs/cli.md` 登记（R15 门禁）。

## 7. 风险与零回归保证

- 服务端**零改动**（三个 conflicts handler 与 ConflictIndex 原样使用）。
- 既有 `sync push/pull/both/retry/watch/schedule` 命令零改动（仅父命令加一个子命令）。
- `NewCmdSync` 只加一行 `AddCommand`；新逻辑全部在独立新文件，无 import 面变化。
- 不动 `OutputFormatter`（13 方法接口）——命令内 `--json` 分支直接输出，
  对齐 `printSyncTaskResult` 先例。
- 列表只展示未解决冲突（服务端语义）；不本地过滤 resolved（防与服务端漂移）。
- 文本冲突内容展示：`list` 不含 Ours/Theirs 行（防刷屏）；manual 解决依赖
  `--content` 显式提供（文本冲突场景够用；二进制内容的 manual 解决留待
  `--file`/stdin 扩展，文档标注限制）。

## 8. 遗留说明

- `sync conflicts get <id>`（详情含冲突内容行）不在本设计范围，可作为后续片。
- 服务端 `writeConflictFile` 当前固定按空 owner（anonymous 租户）解析落盘路径；
  CLI 侧不感知，属服务端既有语义，不改动。
