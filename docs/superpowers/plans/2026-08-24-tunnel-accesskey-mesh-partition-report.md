# M-9 报告：mesh 独立分表（每 mesh 独立 RouteTable）

## 状态

**DONE** — 实现完成，全量测试与 lint 通过，未 commit。

## 改动文件清单

### 新建
- `pkg/tunnel/hub/mesh_route_table.go` — `MeshRouteTable` 聚合层（每 mesh 独立 RouteTable + nodeID→mesh 映射），方法按简报逐字实现。
- `pkg/tunnel/hub/mesh_route_table_test.go` — 跨 mesh 隔离单元测试（6 个测试函数）。

### 修改（简报文件清单）
- `pkg/tunnel/hub/route_table.go` — `NodeInfo` 加 `Mesh string` 字段。
- `pkg/tunnel/hub/router.go` — `HubServer.rt` 改 `*MeshRouteTable`；`NewHubServer(rt *MeshRouteTable, ...)`；`registerNode` 用 `tunnel.AccessKeyMesh(reg.AccessKey)` 解析 mesh 并 `s.rt.Add(mesh, info, ...)`；disc- 校验经 `LookupInfo` 自动定位 mesh。
- `pkg/tunnel/hub/router_test.go` — 适配 `NewMeshRouteTable`/`NodeCount("")`；新增 `TestHubServer_RegisterMeshFromAK`（注册 AK → NodeInfo.Mesh 断言）。
- `pkg/server/handlers.go` — `RegisterRoutesOpts.RouteTable` 与 `Handlers.routeTable` 类型改 `*hub.MeshRouteTable`。
- `pkg/server/auth.go` — 私有 ctx key + `withMesh` + 导出 `MeshFrom(ctx)` + `meshFromRequest(r)`；`authMiddleware` 验签成功后 `r = r.WithContext(withMesh(ctx, tunnel.AccessKeyMesh(matched.Key)))`。
- `pkg/server/hub_handler.go` — `/api/hub/nodes`、`/api/hub/stats`、`/api/hub/services` 按 `meshFromRequest(r)` 过滤；remove 保持按 id（自动 nodeMesh）。
- `pkg/server/relay_stream.go` — 类型改 `*hub.MeshRouteTable`；新增目标节点 mesh 校验（跨 mesh → 404，防节点存在性探测）。
- `pkg/server/signaling_handlers.go` — `SignalBroker.rt` 改 `*hub.MeshRouteTable`；`NewSignalBroker(rt *hub.MeshRouteTable)`；`callerNode` 校验 `info.Mesh == meshFromRequest(r)`；`handleSignalPost` 校验 `from`/`to` 同 mesh；新增 `errSignalMeshMismatch`（403）。
- `pkg/server/metrics.go` — `aggregateMuxMetrics` 跨全部 mesh 汇总；`sproxy_hub_nodes_connected` 按 ctx mesh（无则汇总所有）。
- `cmd/sproxy/root.go` — `hub.NewRouteTable()` → `hub.NewMeshRouteTable()`。

### 测试适配（类型变更导致编译必需，非无关重构）
- `pkg/server/server_hub_test.go`、`pkg/server/relay_stream_test.go`、`pkg/server/hub_services_test.go`、`pkg/server/signaling_test.go`、`pkg/tunnel/mesh/mesh_test.go` — `NewRouteTable`→`NewMeshRouteTable`、`Add`→`AddNode("",...)`、`AddWithInfoAndServices`→`Add("",...)`、`List()`→`List("")`、`ServicesOf`→`Table("").ServicesOf`、`runNodeTestHub` 返回类型等。

## 跨 mesh 隔离测试摘要

- **`TestMeshRouteTable_CrossMeshIsolation`**：两 mesh 各注册 1 节点 → `List(mesh)` 各自只见本 mesh、`NodeInfo.Mesh` 写入、各 mesh 独立表 `Lookup/Has` 跨 mesh 返回 nil/false、聚合 `Lookup` 按 nodeMesh 定位、`ListServices(mesh)`/`NodeCount(mesh)` 隔离。
- **`TestMeshRouteTable_DefaultMeshBehavesLikeSingleMesh`**：默认 mesh `""` 等价单 mesh。
- **`TestMeshRouteTable_RemoveCleansNodeMeshAndEmptyTable`**：Remove/RemoveIfOwned 成功后 nodeMesh 清理 + 空表删除。
- **`TestMeshRouteTable_SameNodeID_MovesMesh`**：同 ID 跨 mesh 重注册 → 从旧 mesh 表移除（节点名不跨 mesh 共享）。
- **`TestMeshRouteTable_RemoveHookFiresPerMesh`**：SetRemoveHook 覆盖所有内部表（含惰性新建）。
- **`TestMeshRouteTable_AllMeshes`**：AllMeshes 含默认 `""` 与各 mesh。
- **`TestHubServer_RegisterMeshFromAK`**（hub 层）：`sk-mesh-a-<hex>`/`sk-mesh-b-<hex>` 注册 → `NodeInfo.Mesh` 正确、`List(mesh)` 隔离、`NodeCount(mesh)` 隔离。
- **`TestHubNodesHandler_MeshIsolation`**（server 集成）：`/api/hub/nodes` 用 mesh-a AK 只见 mesh-a 节点，mesh-b AK 只见 mesh-b 节点（SproxySig 签名请求）。
- **`TestSignalBroker_CrossMeshSignalingRejected`**（server 集成）：mesh-a→mesh-b 信令 403；mesh 不一致 callerNode 403；同 mesh 投递 202。
- **`TestRelayStreamHandler_CrossMeshTargetNotFound`**（server 集成）：mesh-a 调用方中继 mesh-b 节点 → 404（跨 mesh 目标不可见）；同 mesh 对照进入转发（叶子回拨号失败 → 502，非 404）。

## 测试 / lint 结果

- `go test -count=1 ./pkg/... ./test/...` — 全部 ok（含 hub/server/mesh/client 等）。
- `go test -count=1 ./cmd/sproxy/... ./cmd/sclient/...` — 全部 ok。
- `go test -race -count=1 ./pkg/tunnel/hub/... ./pkg/server/... ./pkg/tunnel/mesh/...` — 全部 ok（无竞态）。
- `go vet ./pkg/... ./test/...` + cmd 两模块 — 0 输出。
- `golangci-lint run ./pkg/... ./cmd/...` — **0 issues**；`pkg/tunnel/mesh` 独立模块 lint — 0 issues。

## 疑虑 / 偏离说明

1. **「Lookup(跨mesh nodeID) 返回 nil」语义**：简报方法签名 `Lookup(id NodeID)` 为聚合全局查询（`查 nodeMesh[id] → 对应表 Lookup`），节点已注册时聚合 Lookup 必然非 nil。路由面隔离的实际边界是**每 mesh 独立表**：`Table("mesh-a").Lookup("node-b")` 返回 nil（跨 mesh 节点不在对方表中）。为满足"路由面隔离"端到端，`relay_stream.go` 额外加了**调用方 mesh 与目标节点 mesh 一致性校验**（跨 mesh → 404，防节点存在性探测）——这是对简报「转发 rt.Lookup(target) 不变」的收敛性增强，非破坏性改动（同 mesh 行为不变）。
2. **测试文件超出简报清单**：类型变更（`RegisterRoutesOpts.RouteTable`/`NewHubServer`/`NewSignalBroker`/`NewRelayStreamHandler` 签名）强制 5 个既有测试文件适配才能编译。已做最小改动，未做无关重构。
3. **`MeshRouteTable.Add` 跨 mesh 同名重注册清理**：简报内部一致性只提「Add 时更新 nodeMesh」；为维持「节点名不跨 mesh 共享」的不变量，`Add` 在 nodeID 已属他 mesh 时会先从旧 mesh 表移除（防幽灵节点泄漏到旧 mesh 的 List）。
4. **`hubServicesHandler`（`/api/hub/services`）也按 mesh 过滤**：简报 4 节只列 nodes/stats/remove，但该端点是 mesh 服务发现主入口，正是 M-9 要堵的「列全部服务」泄漏面，故一并过滤。隧道路径（localMux）经 `handler_client.go dispatchLocal` 复用外层 ctx（`NewRequestWithContext(r.Context(), ...)`），mesh 正确传播，隧道客户端选路不受影响。
5. **`AggregateMuxMetrics` 跨全部 mesh 汇总**：`/metrics` 未挂 authMiddleware，`meshFromRequest` 恒空串 → 按简报「无 mesh 请求时汇总所有」实现。
