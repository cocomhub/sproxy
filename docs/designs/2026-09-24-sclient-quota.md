# 设计：sclient quota —— 本 owner 配额水位查看

> 日期：2026-09-24 ｜ 范围：sclient CLI 新增 `quota` 命令（服务端零改动）

## 1. 背景 / 目标

- 现状（已核实）：服务端 `GET /api/stats` 已返回 quota 段
  （`pkg/server/stats.go` `StatsResponse.Quota` = `QuotaStatus{Usage, MaxBytes, Watermark}`，
  `quotaStatusOf` 计算：`MaxBytes<=0`（不限）→ watermark=0；admin 空 owner = 全局聚合）。
  但 `pkg/client/stats.go` 的 `StatsResponse` **未定义 Quota 字段**（JSON 反序列化直接丢弃），
  `sclient stats`（`cmd/sclient/stats.go`）也不展示配额。
- 目标：新增 `sclient quota [--json]`，以认证用户身份展示本 owner 水位
  （usage / max_bytes / watermark），脚本可用 `--json` 消费。

## 2. 组件与接口

### 2.1 pkg/client（`pkg/client/stats.go`，唯一服务端契约扩展点）
```go
// StatsResponse 追加字段（对齐服务端 QuotaStatus JSON）：
Quota QuotaStatus `json:"quota"`

type QuotaStatus struct {
    Usage     int64 `json:"usage"`
    MaxBytes  int64 `json:"max_bytes"`
    Watermark int   `json:"watermark"` // 百分比 0-100；MaxBytes<=0 = 0
}
```
`GetStats` 方法不变（零回归；旧服务端无 quota 段 → 零值）。

### 2.2 cmd/sclient（新文件 `cmd/sclient/quota.go`）
- `NewCmdQuota(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command`
  —— 签名对齐 `NewCmdStats`；`root.go` 注册 `root.AddCommand(NewCmdQuota(factory, ios))`。
- 命令体：
  ```go
  Use: "quota", Short: "显示当前用户配额水位", Args: cobra.NoArgs
  RunE: svc := factory.NewClient(cmd) → svc.GetStats(cmd.Context())
        → 输出 resp.Quota（见 3）
  ```
- **不扩展 `OutputFormatter` 接口**（13 方法 + 两个实现 + 测试 mock 全要动）：
  命令内按根 flag `--json` 分支输出，对齐 `cmd/sclient/sync.go` 的
  `printSyncTaskResult`（jsonOut 直接 `json.NewEncoder`）先例 —— 零回归。
- 文本输出（中文，对齐现有文案风格）：
  ```
  配额水位
    usage:     12.3 MiB      # client.FormatByte
    max_bytes: 1.0 GiB       # MaxBytes<=0 显示 "不限"
    watermark: 12%            # MaxBytes<=0 显示 "0%（不限）"
  ```
- `--json` 输出：`resp.Quota` 原样 JSON（`{"usage":..,"max_bytes":..,"watermark":..}`）。

## 3. 数据流

```
sclient quota
  → factory.NewClient(cmd)（SproxySig 认证，owner = 凭据 AK）
  → GET /api/stats（authMiddleware 放行；服务端 owner=actor → quotaScopeFor(owner)
     → quotaStatusOf 返回 per-owner QuotaStatus）
  → resp.Quota → 文本/JSON 输出
```

## 4. 错误处理

- 客户端初始化失败：`ios.WriteErrLine("初始化客户端失败: %v", err)` + `errFmtInitClient`
  包装（对齐 `NewCmdList`）。
- `GetStats` 失败：`fmt.Errorf("获取统计信息失败: %w", err)`（对齐 `NewCmdStats`）。
- 服务端 503（存储扫描未完成）：错误信息透传，命令非零退出。
- `--json` 模式下错误同样返回非零（输出纯度与退出码语义对齐既有命令）。

## 5. 测试 + 变异点

- **pkg/client 单测**：`newMockServer`（`pkg/client/client_test.go`）扩展 /api/stats
  响应含 `quota` 段 → 断言 `GetStats().Quota` 解析出 usage/max_bytes/watermark。
  变异：client 结构漏 `Quota` 字段（或 tag 错）→ 解析为零值 → 断言红。
- **cmd/sclient CLI 测试**（httptest mock + `pkg/testutil` CaptureStdout）：
  - 文本输出含 `usage`/字节值/`watermark: N%`（变异：watermark 计算/展示分支错 → 红）；
  - `MaxBytes<=0` → 显示"不限"（变异：删除该分支 → 红）；
  - `--json` 输出可解析且含 `quota` 键（变异：漏 JSON 分支 → 红）。

## 6. 片划分

- **片 1**：`pkg/client/stats.go` 加 `Quota`/`QuotaStatus` + client 单测（含 mock 扩展）。
- **片 2**：`cmd/sclient/quota.go` + `root.go` 注册 + CLI 测试 + `docs/cli.md` 登记。

## 7. 风险与零回归保证

- 服务端**零改动**（quota 段与计算逻辑原样使用）。
- client 结构仅追加字段：JSON 反序列化向后兼容（未知字段丢弃、缺字段零值）。
- 不动 `OutputFormatter` 接口与既有 `sclient stats` 输出：无回归面。
- `quota` 是新命令，无参数变更；`root.go` 仅加一行 AddCommand。
- R15 文档门禁：`docs/cli.md` 需登记新命令与 `--json` 用法（命令无新 flag，风险低）。
- 展示语义与 `quotaStatusOf` 一致：不限配额显示 0%/不限，不做本地二次计算（防漂移）。

## 8. 遗留说明

- 多卷/多租户下 quota 为默认卷聚合（服务端语义），CLI 不展示卷维度 —— 与 stats 现有语义对齐。
