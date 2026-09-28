# 审查：批次 15——新合并功能（27 项，状态/集群/客户端/代理）

- **批次**：15
- **审查基线**：master `55b20e587` → `a72ea5107`（#641-#666，27 提交）
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过（无 P0/P1/P2；2 项 P3 记录）

**发现数**：P0 0 / P1 0 / P2 0 / P3 2

## 发现清单

### [P3] 凭据轮换在途请求 drain 依赖宽限期（#641/#643/#645）
- **位置**：`pkg/client` RenewAccessKey + credrotate（#641）+ mesh node/relay（#643）+ p2p（#645）
- **问题**：轮换时在途请求（用旧 SK 签名中）的处理——服务端多 SK 共存宽限期保证不失败，但无显式 drain（等存量请求完成再切）。
- **建议**：设计 #646 §3.2 已记录「未实施（依赖宽限期兜底）」——旧 SK 宽限期保证在途不失败，风险低；记录为演进约束。

### [P3] X-Mesh-Path 路由头启用时拓扑泄露（#661）
- **位置**：`pkg/httpproxy` RouteInfoer + `--mesh-route-header`
- **问题**：启用时响应/日志带出口节点名（如 sg-t|relay|e2e）——暴露 mesh 拓扑。
- **建议**：**默认关**（防拓扑泄露）——显式启用才开，符合「安全开关可观测」演进原则；记录为演进约束。

## 通过项（无问题面）

- **G1 状态/集群**（#651/#663/#665/#656/#659——本批次实现）：StateStore F1-F2 全迁移（credential/checksum/dedup/share/index 适配器 + 双读单写）+ MongoStateStore TTL 租约 + WriteGuard 只读副本。实现时已 TDD+变异+CI 全绿；审查复核集成面无回归。
- **G2 备份/计量/限流**（#664/#652/#650——本批次实现）：备份 P2（federated 写面 + backupQuotaFS 双预留）+ 计量 owner 维度 + 限流 config 接线。已 TDD+变异+Windows 修复。
- **G3 客户端/代理**（其他 agent 实现）：
  - #662 proxylog recv 方向修复（ca.Recv→ca.Sent 响应字节漏计修正）+ LogAccess extra（route/trace）
  - #661 X-Mesh-Path/Trace 路由头（RouteInfoer 接口 + 默认关）
  - #658 upstream-proxy fallback（TLS 端到端 + CONNECT 认证 + bufferedConn 预读）
  - #657 smart fallback 保留 E2E（禁静默降级明文——安全修复）
  - #654 exit-group 双出口恒加密 + #649 协议盐 + #647 NewIdentityFromSeed
- **G4 docs/回填**（#640/#642/#646/#648/#653/#655/#660/#666）：全部 docs-only 通道合并。
- **聚焦测试全绿**：G3 proxylog/httpproxy/client/sclient 各包 `go test` 全 ok。

## 验证方式

- 源码逐提交审查（recv 方向/fallback 加密/bufferedConn/路由头默认关）
- 聚焦测试（G3 包）+ 实现时 TDD/变异（G1/G2）
