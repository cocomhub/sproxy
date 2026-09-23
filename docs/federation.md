# 联邦卷设计规格（roadmap 3.3 P2）

> 目标：把**远端 hub 的卷**以只读挂载形式暴露到本地卷视图（跨节点联邦卷）。
> 复用现有 mesh 载体（hub 联邦节点表 + mesh_sync 的 A 侧装配）与 FileClient 隧道数据面。
> 本文档是**实现前设计**（当前未实现）；实现按片推进，每片功能完整可用。

## 1. 拓扑

```
本地 hub（federation.enabled + peers）
   │  hub 联邦（节点表同步，已落地 #4xx）
   ▼
远端 hub（路由表含目标节点 T，T 暴露只读卷）
   │  mesh 数据面（A 侧经 hub 中继 → T，加密）
   ▼
T 节点（sproxy 实例，持卷 user 桶）
```

本地服务端装配 `federated_volumes[]`：`{name, hub, volume, path?}`——`name` 是本地卷视图
名，`hub` 是远端 hub 地址，`volume` 是远端卷名，`path` 是远端卷内子路径（可选）。

## 2. 与现有能力的衔接

| 能力 | 现状 | 联邦卷复用 |
|------|------|-----------|
| hub 联邦节点表 | `federation.peers` 周期拉取（已落地） | 发现远端节点 T（t 持卷） |
| mesh 数据面 | A 侧 mesh 载体（mesh_sync，FileClient 经 hub 中继拨号 T） | 读代理经同一链路 |
| 认证 | SproxySig v2（mesh.access_key/secret + skey-id） | 远端 hub 读 API 复用 |
| 卷状态 | 外部后端探针 → healthy/degraded/unknown | 联邦卷探针 = 远端可达性 |

## 3. 数据面（读代理）

```go
// federatedBackend 实现 sync.FS（只读）：每个方法经 FileClient 隧道调远端 hub API。
type federatedBackend struct {
    client *client.FileClient // 经 hub 中继（WithXfer + 远端 hub）
    remote string             // 远端卷名
    path   string             // 卷内子路径（可选）
}
// ListDir  → GET /api/files?subdir=<path>&volume=<remote>
// Stat     → HEAD /api/files/stat?filename=<path>&volume=<remote>
// OpenRead → GET /download?filename=<path>&volume=<remote>
// 写方法（WriteFile/Rename/Delete/MakeDir）→ 返回 ErrReadOnly（只读语义 fail-closed）
```

- 装配点：`pkg/volume/registry.RegisterBackend("federated", ...)`——与 s3/sftp 同构；
  卷视图读路径（List/Stat/Download 经 VolumeRouter）无感。
- 数据面隧道：FileClient `WithXfer(name, hubURL, key)`——服务端 `newMeshHubClient`
  已有 base 装配（mesh_sync.go:406），扩展为含数据面选项。

## 4. 认证与安全

- 远端 hub 读 API 走 SproxySig v2（mesh.access_key/secret）——fail-closed：缺凭据不挂载。
- **信任边界（审查 P3 明示）**：`FederationClient` 对 peer 的 `syncPeer` 拉取——配置了
  AccessKeySecret 时必须同时配 AccessKeyID（v2 skey-id 必传，缺失 fail-closed 报错）；
  **完全无凭据（secret 为空）时裸请求不签名**——语义 = 信任该 peer 为无认证调试 hub
  （仅在可信内网/调试环境使用；生产部署必须为每个 peer 配 mesh.access_key/secret）。
- 只读强制：写方法恒返回 `ErrReadOnly`（防绕过）；卷视图标记只读（`volumes_api.go`
  的 State 扩展 `readonly` 或 ACL deny 写）。
- 数据面加密：mesh 中继链路已有 E2E 能力（#406/#408/#410）——联邦卷读默认走加密。

## 5. 限制（明确不做，避免范围膨胀）

- **只读**：不做远端写（写回远端是 P2 后续片，需冲突语义）。
- **无缓存**：读每次经隧道（读放大可控；高频读可后续加本地缓存片）。
- **单跳**：本地 hub → 远端 hub 直连（多跳中继是 via-node 能力，联邦卷不引入）。
- **一致性**：远端卷实时视图（无本地副本语义——副本是 mirror_targets #484 的域）。

## 6. 实施片划分（每片独立 PR）

| 片 | 内容 | 验收 |
|----|------|------|
| F1 | `federatedBackend` 库（sync.FS 只读适配 + ErrReadOnly） | 单测：ListDir/Stat/OpenRead 经 mock hub + 写方法拒绝 |
| F2 | registry 装配 + 配置（`federated_volumes[]`）+ 探针（远端可达性 → degraded） | 装配后列表可见联邦卷 + 探针状态可观测 |
| F3 | 服务端数据面隧道装配（newMeshHubClient 扩展 WithXfer）+ 端到端测试 | 本地卷视图读远端卷文件成功（真实 mesh 链路） |

## 7. 相关文档

- [architecture.md](./architecture.md)：mesh 载体分层
- [config.md](./config.md)：federation / mesh 配置
- [mesh-testing.md](./mesh-testing.md)：mesh 链路测试方法
