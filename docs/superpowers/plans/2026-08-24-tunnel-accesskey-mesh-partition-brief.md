# M-9 简报：mesh 独立分表（每 mesh 独立 RouteTable）

> 这是你的需求，包含要逐字使用的精确取值。不要读整个设计/计划，先读本简报。
> 项目根：`D:\workdir\leon\cocomhub\sproxy`（Windows，Go 1.26，git 仓库，分支 feature/mesh-tunnel）。

## 目标

**每 mesh 独立 RouteTable**：mesh 的节点/服务/路由表完全隔离（连节点名都不共享），消除
M-9（任一合法 AK 可列出 hub 上全部节点/服务，含他 mesh）。mesh 由 SproxySig AccessKey
解析（`tunnel.AccessKeyMesh(ak)`，已存在）。数据面加密已按 mesh 隔离（HKDF(SK, mesh)），
本任务补上**元数据面 + 路由面**隔离。

## 架构决策（逐字采用，勿改）

### 1. mesh 来源与传播
- 节点注册时：`mesh := tunnel.AccessKeyMesh(reg.AccessKey)`，存入 `NodeInfo.Mesh`。
- 无 mesh（AK 为 `sk-<hex>` 或非 sk- 格式 → 空串）→ 归入默认 mesh `""`（单 mesh 部署等价现行为）。
- server 层请求的 mesh：`authMiddleware` 已通过 `verifySproxySig` 返回命中 AK，派生 mesh 后
  **放入 request ctx**（新增私有 ctx key + `MeshFrom(ctx)` helper），供 `/api/hub/nodes`、
  信令、metrics 按 mesh 过滤。

### 2. 新增 `MeshRouteTable`（聚合层，`pkg/tunnel/hub/mesh_route_table.go`）
`RouteTable`（单 mesh 表）**保持不变**（它是现有成熟实现）。新增聚合层：

```go
// MeshRouteTable 是每 mesh 独立 RouteTable 的聚合：map[mesh]*RouteTable + nodeID→mesh 映射。
// 转发/列表/信令按 mesh 隔离；无 mesh（""）为默认表，行为等价单 mesh。
type MeshRouteTable struct {
	mu       sync.RWMutex
	tables   map[string]*RouteTable // meshID → 单 mesh 路由表
	nodeMesh map[NodeID]string      // nodeID → mesh（转发时查目标 mesh；LookupInfo 需 mesh 校验时用）
}
```

方法（签名按此实现）：
- `Table(mesh string) *RouteTable` — 获取/惰性创建某 mesh 的表
- `Add(mesh string, info NodeInfo, svcs []Service)` — 写入对应表（`info.Mesh = mesh`）
- `AddNode(mesh string, id NodeID, m *mux.Mux)` — 低层节点注册（AddWithInfo 的变体）
- `Lookup(id NodeID) *mux.Mux` — **转发用**：查 `nodeMesh[id]` → 对应表 Lookup
- `LookupInfo(id NodeID) (NodeInfo, bool)` — 查 nodeMesh → 对应表（NodeInfo 含 Mesh）
- `Has(id NodeID) bool` — 查 nodeMesh → 对应表
- `Remove(id NodeID) bool` / `RemoveIfOwned(id NodeID, m *mux.Mux) bool` — 查 nodeMesh → 对应表
- `List(mesh string) []NodeInfo` — 某 mesh 的节点列表（`/api/hub/nodes` 用）
- `ListServices(mesh string) []NodeService` — 某 mesh 的服务宣告
- `NodeCount(mesh string) int` — 某 mesh 节点数（metrics 用）
- `MeshOf(id NodeID) string` — 查 nodeID 所属 mesh
- `SetRemoveHook(fn func(NodeID))` — 为**每个**内部 RouteTable 挂 onRemove（SignalBroker 收件箱清理）
- `AllMeshes() []string` — 所有 mesh 列表（debug/管理用）

内部一致性：Add 时同时更新 `nodeMesh[id]`；Remove 成功时删除 `nodeMesh[id]` 并清空空表。

### 3. `HubServer` 改用 `*MeshRouteTable`
`pkg/tunnel/hub/router.go`：
- `HubServer.rt` 类型改 `*MeshRouteTable`。
- `NewHubServer(rt *MeshRouteTable, ...)` 签名改。
- `registerNode`：注册时 `mesh := tunnel.AccessKeyMesh(reg.AccessKey)`；`info.Mesh = mesh`；
  用 `s.rt.Add(mesh, info, ...)`。disc- 临时身份校验不变（同一 mesh 表内查 real_node）。
- `RemoveIfOwned` 调用点、`Has` 等改为 MeshRouteTable 方法（按 id 自动查 mesh）。

### 4. server 层改造
- `RegisterRoutesOpts.RouteTable` 类型改 `*hub.MeshRouteTable`（`pkg/server/handlers.go`）。
- `cmd/sproxy/root.go`：`hub.NewRouteTable()` → `hub.NewMeshRouteTable()`。
- **`pkg/server/auth.go`**：新增 `meshFromRequest(r)` helper——从 ctx 读 mesh（authMiddleware
  verifySproxySig 成功时写入）；无则返回 `""`。
- `pkg/server/hub_handler.go`（`/api/hub/nodes`、`/api/hub/stats`、remove）：
  - 列表/统计按 `meshFromRequest(r)` 过滤（`rt.List(mesh)`、`rt.NodeCount(mesh)`）。
  - remove 用 `rt.Remove(id)`（自动按 nodeMesh）。
- `pkg/server/relay_stream.go`：转发 `rt.Lookup(target)` 不变（MeshRouteTable.Lookup 自动查 mesh）。
- `pkg/server/signaling_handlers.go`（SignalBroker）：
  - `callerNode` 校验：从 `rt.LookupInfo(node)` 取 `NodeInfo.Mesh`，要求与请求 ctx mesh 一致
    （防跨 mesh 信令）。
  - 收件箱按 nodeID 存不变；`PurgeNode` 由每个表的 onRemove 触发。
- `pkg/server/metrics.go`：节点数统计改按 mesh（无 mesh 请求时汇总所有，或按 ctx mesh）。

### 5. 信令跨 mesh 校验
`SignalBroker` 发消息时校验 `from` 与 `to` 同 mesh（`LookupInfo(from).Mesh == LookupInfo(to).Mesh`），
跨 mesh 信令拒绝（403）。这是元数据隔离的纵深（防信息面交叉）。

## 文件清单

| 文件 | 改动 |
|------|------|
| `pkg/tunnel/hub/mesh_route_table.go` | **新建**：MeshRouteTable（上述方法） |
| `pkg/tunnel/hub/route_table.go` | `NodeInfo` 加 `Mesh string` 字段 |
| `pkg/tunnel/hub/router.go` | HubServer 用 MeshRouteTable；registerNode 解析 mesh；转发/移除适配 |
| `pkg/tunnel/hub/router_test.go` | 测试适配 MeshRouteTable（大部分用默认 mesh "" 即等价原行为，少量加 mesh 维度断言） |
| `pkg/tunnel/hub/mesh_route_table_test.go` | **新建**：跨 mesh 隔离测试（同 mesh 互见、跨 mesh 列表/转发/服务发现均不可见） |
| `pkg/server/handlers.go` | RegisterRoutesOpts.RouteTable 类型改 MeshRouteTable |
| `pkg/server/auth.go` | ctx 写/读 mesh（verifySproxySig 成功时）；`MeshFromRequest` |
| `pkg/server/hub_handler.go` | /api/hub/nodes、stats 按 mesh 过滤；remove 适配 |
| `pkg/server/relay_stream.go` | 适配（Lookup 不变，类型改） |
| `pkg/server/signaling_handlers.go` | 信令 from/to 同 mesh 校验 |
| `pkg/server/metrics.go` | 节点统计按 mesh |
| `cmd/sproxy/root.go` | NewMeshRouteTable |
| `test/e2e_relay_test.go`、`e2e_mesh_node_test.go` | 适配（如有直接构造 RouteTable 处） |

## 测试要求
- 主仓 `go test ./pkg/... ./cmd/... ./test/` 全过。
- **新增跨 mesh 隔离断言**（mesh_route_table_test.go + 集成）：
  - 两个 mesh（`sk-mesh-a-<hex>`、`sk-mesh-b-<hex>`）注册节点，各自 `List(mesh)` 只见本 mesh；
  - `Lookup(跨mesh nodeID)` 返回 nil（路由面隔离）；
  - `/api/hub/nodes` 用 mesh-a 的 AK 请求只见 mesh-a 节点；
  - 跨 mesh 信令（from mesh-a → to mesh-b）403。
- `golangci-lint run ./pkg/... ./cmd/...` 0 issues。

## 约束
- 中文注释；测试纯标准库；127.0.0.1 回环；Windows 兼容；新文件 SPDX 头。
- **不要 commit**（主流程统一提交）。BLOCKED 时写报告回传，不猜测硬做。
- 改动只限上表文件，不做无关重构。

## 报告
- 写到 `docs/superpowers/plans/2026-08-24-tunnel-accesskey-mesh-partition-report.md`。
- 回传：状态（DONE/BLOCKED）、改动文件清单、`go test`/lint 结果、跨 mesh 隔离测试摘要、疑虑。
