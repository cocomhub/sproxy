# 联邦卷强一致性（11.3-③）

## 背景 / 目标
- 现状：`pkg/volume/federated/federated.go` 是联邦卷适配层——只读 `Reader` 注入 + 可选 `Writer`（`WithWriter`，注入 `sync.FS` 写面）。写面直接转发 `WriteFile/Rename/Delete/MakeDir` 到远端，**无冲突策略 = LWW 覆盖语义**（远端以最后写入者胜）。
- 目标：可选 `extra.conflict_mode=version`（版本检查：远端版本号不匹配 → 409 拒绝）/ `conflict`（冲突文件：写冲突时生成 `.conflict.<ts>` 文件保留双方）——**复用 sync 冲突策略**（`pkg/sync` 已有冲突处理：`ConflictIndex` 在 server 端装配，syncmgr 管理）。

## 组件与接口
- `pkg/volume/federated/federated.go` 扩展：
  - `ConflictMode` 枚举：`lww`（默认，现状零回归）/ `version` / `conflict`。
  - `FS` 增 `WithConflictMode(mode ConflictMode) *FS`（装配层按 `extra.conflict_mode` 配置调用；nil/未调用 = lww）。
  - `Writer` 接口扩展为**版本感知**（可选接口，type-assert）：
    ```go
    type VersionedWriter interface {
        syncpkg.FS
        // CheckVersion 返回远端路径当前版本号（-1=不存在）；nil = 不支持版本检查。
        CheckVersion(ctx, path) (int64, error)
        // WriteFileVersioned 带版本前置检查写入（expected<0 = 期望不存在）。
        WriteFileVersioned(ctx, path, r io.Reader, size, mtime int64, expected int64) error
    }
    ```
    `FS` 在 `WriteFile` 内：lww → 直转；version → 先 `CheckVersion` 再 `WriteFileVersioned`（CAS 语义：远端侧原子比较，防 TOCTOU）；conflict → 写入前查版本，不匹配则远端把旧文件改名 `.conflict.<ts>` 后写入新内容（或本地生成冲突文件，取决于远端实现——本期定义远端实现接口，装配层落地）。
- 装配层（cmd/sproxy）：联邦卷的 `extra.conflict_mode` 解析 → `WithConflictMode`；version/conflict 模式要求 Writer 实现 `VersionedWriter`，不满足 → 启动报错（fail-closed，不静默降级回 lww）。
- 冲突文件命名与 sync 既有语义对齐（复用 `ConflictIndex` 展示，冲突文件进用户卷视图 + `/api/sync/conflicts` 可查）。

## 数据流
1. LWW（默认）：写面直转（现状零回归）。
2. version 模式：`WriteFile` → `CheckVersion(path)` 取当前版本 → `WriteFileVersioned(..., expected)` → 远端 CAS：当前版本 == expected 才写（写后版本+1），否则 409 → 客户端收 `409 Conflict`（带当前版本号提示重取）。
3. conflict 模式：`CheckVersion` 发现不匹配 → 远端把旧文件原子改名为 `<name>.conflict.<unix-ts>`（保留旧内容）→ 新内容以原路径写入 → 客户端 200 + 响应体带 `conflict_created: true` + 冲突文件路径。冲突文件登记 `ConflictIndex`（若已装配）。

## 错误处理
- version 模式 CAS 失败（并发写入者先到）→ 409 + 当前版本号（客户端重取重试，幂等）。
- conflict 模式 rename 旧文件失败（权限/配额）→ 500，不写新内容（保持一致性，防丢旧内容）。
- Writer 不支持 `VersionedWriter` 却配置 version/conflict → 装配期 fail-fast 报错（禁静默降级 lww——安全开关可观测红线）。
- 版本号溢出/异常 → 视为 CAS 失败（fail-closed 409）。
- conflict 文件配额超限 → 该次写入失败并报告（不绕过配额）。

## 测试 + 变异点
- `TestFederated_LWW_ZeroRegression`：默认（无 ConflictMode）→ 直转 Writer（变异：默认改成 version → 红）。
- `TestFederated_VersionMatch_Writes`：版本匹配 → 写入成功 + 版本 +1（变异：不检查直接写 → 红）。
- `TestFederated_VersionMismatch_409`：版本不匹配 → 409 + 当前版本号（变异：返回 200/静默覆盖 → 红）。
- `TestFederated_Conflict_CreatesFile`：conflict 模式 → 旧文件改名 `.conflict.<ts>` + 新内容写入（变异：不保留旧文件/不生成冲突文件 → 红）。
- `TestFederated_UnsupportedWriter_FailFast`：Writer 非 VersionedWriter + version 模式 → 装配报错（变异：静默降级 lww → 红）。
- 变异验证核心：CAS 检查缺失、冲突文件生成缺失、静默降级三处各命中。

## 片划分
- P1：`ConflictMode` + `VersionedWriter` 接口 + `FS` 三分支转发逻辑 + 单测。
- P2：远端侧 CAS 实现（装配层落地 `WriteFileVersioned` 语义）+ conflict 文件生成 + `ConflictIndex` 登记。
- P3：sclient/CLI 侧 409/conflict 提示 + 文档 + 跨节点 e2e。

## 风险与零回归
- 默认 lww 不变（`WithConflictMode` 未调用即现状直转），配置显式开启才改变语义。
- 版本/冲突语义集中在联邦卷写面，不动既有本卷写路径。
- fail-fast 防静默降级：模式显式配置但能力缺失 → 启动报错，不悄悄回 lww。
- 冲突文件命名与 sync 既有惯例一致，可被既有冲突索引/API 消费。
