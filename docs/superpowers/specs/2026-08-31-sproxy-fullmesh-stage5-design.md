---
title: sproxy 完全组网·阶段 5 设计（P3 能力完整性 + 技术债铺底）
status: planning
---

# sproxy 完全组网·阶段 5 设计

> 日期：2026-08-31
> 来源：用户 `/plan` 发起「分析当前能力，距离完全组网还差哪些功能，设计发展规划」——3 个 Explore agent 盘点缺口 + 1 个 Plan agent 对峙式核实；用户决策定方案
> 前置：阶段 1–4 已合入（master `b7fad2b`），路线图 `docs/superpowers/specs/2026-08-29-sproxy-fullmesh-roadmap.md`
> 分支：feature/mesh-p5-*（每 PR 独立，从最新 origin/master 拉）

## 1. 目标与范围

阶段 1–4 打通了「完全组网」基础链路（mesh 全连通 + TURN + 证书身份 + 联邦 + 持久化 + 多传输 + 文件同步 + 虚拟 IP）。本阶段补上「能力完整性断点」——**已实现但落不了地 / 不安全 / 无调度**的三条：

1. `--xfer` 服务端 listener 接线 + TCP 传输 TLS（证书 pinning 能力真正可服务于生产拓扑）
2. 域路由 + 服务负载均衡（同名多副本 round-robin + 失败跳过）
3. 动态 TURN（REST 短期凭证）+ 联邦候选 / DHT 持久化

技术债铺底为前置（learnings 入库、flake 残留清理、死字段清理、CLAUDE.md 更新）。

**非目标（用户已排除）**：3+ hub 网状/环形联邦、mesh node 间多跳转发（单链 + 防环已满足）；生产可用性/治理类缺口（同步自动重试、审计、多租户、AK 轮换、metrics+OTel、Web UI 管理面等）——排期阶段 6。

**用户已确认的决策**：
- 主轴 = ①②分层阶段化（阶段 5 能力完整性，阶段 6 生产可用性）
- 技术债铺底纳入前置
- 阶段 5 优先工作项 = `--xfer` 接线 + TCP TLS、域路由 + LB、动态 TURN + 联邦/DHT 持久化
- LB 寻址模型 = **精确 + 多候选轮询**（不加通配）
- learnings = **改入库**（删 .gitignore 规则一次性 add）
- 文档落地 = 写本规格文档

---

## 2. 前置铺底（技术债，小 PR 组先做）

### T0. learnings 入库（高危，先决）
- 现状：`.gitignore:34` 的 `learnings/` 规则导致 `docs/superpowers/learnings/` 24 份文档从未入库（`git ls-files` = 0），clone 即失；内容含阶段 2/3/4 复盘与 virtualip 交接简报等**唯一事实来源**，已核实无密钥凭据。
- 动作：删 `.gitignore:34`（注释说明 learnings 改为入库）；`git add -f docs/superpowers/learnings/` 一次性入库存档。

### T1. 补写 `2026-08-31-stage4-virtualip.md` 最终版
- 现状：虚拟 IP 只有 `stage4-virtualip-handoff.md`（v2 设计交接）与 step4-filesync.md，无「已落地实现」复盘。
- 新建：目标 → 用户决策 → 架构决策（AD-1~6）→ 实现要点（与 handoff v2 设计的差异）→ 验证结论（PR #134）。

### T2. syncmgr integration flake 残留
- `pkg/server/syncmgr/integration_external_test.go:169` `waitForStatus(…,"syncing",5s)` 仍是固定短死等（#136 只处理了 mock executor 的测试）；CI -race+cover 下有 flake 面。
- 改造：确定性信号（select + status/handler 通知），弃轮询；顺带统一 manager_test 同类 `waitForStatus`。

### T3. `SignalMsg.Cand` 死字段清理
- `pkg/tunnel/hub/signaling.go:32` `Cand` 字段仅 `signaling_client.go:152 post()` 写入且恒传 `""`，生产无 sender/consumer；`SignalCandidate` endpoint 仅注册无人 POST（webrtc 走 `Signaler` 接口、ICE 全内联 SDP，trickle 未实现）。
- 删：`Cand` 字段、`post(...cand)` cand 参数、`SignalCandidate` 常量/endpoint（srvMux + localMux 两处）、persist_test `Cand:` 用例。

### T4. `test/e2e_mesh_node_test.go:224` 20s 死等
- `TestE2E_MeshNode_ServiceAccess` 用 ≤20s 轮询 stderr 出直连日志，仍是死等。
- 改造：确定性信号（复用 `pkg/tunnel/mesh/node.go` 的 `DiscoveryPeers` / `GatewayNotify` 观测通道 + select）；若 stderr 子进程方案必须保留则绑 timeout + t.Cleanup 收口。

### T5. CLAUDE.md「已知的技术债务」更新
- 4 条已核实全修复（信号 goroutine → `runSignalHandler` + stopSigCh；findModuleRoot → e2eModuleRoot；mux retransmitLoop → 内联 writeLoop；parseDuration → 已删）→ 从「已知技术债务」移除/归档。

### T6. 子 module 测试门禁（可选，可推迟到工作项后）
- `scripts/check-test-files.sh` 扩展覆盖子 module 目录（继承 .notestignore）；不硬加子 module 覆盖率（pion/webrtc 链路意义低）。

---

## 3. 工作项 1：`--xfer` 服务端 listener 接线 + TCP 传输 TLS

### 3.1 现状（已核实）
- `pkg/client/client.go getTunnelMux`：`xfer.Get(name).Dial(ctx, hubURL)` → `mux.New(conn, RoleDialer)` → `tunnel.NewTunnel(m, key, opts...)`；握手 = ECDH + 身份签名 + pin（`pkg/tunnel/ecdh.go`），无 hub 注册帧，直接对 listener 做隧道。
- 服务端 `cmd/sproxy/root.go` 只挂 hub WS `/ws` 与裸 TCP 中继两个 listener，**无接收 `tunnel --xfer` 会话的独立 xfer listener**（`docs/cli.md` N-3 明说"待后续接线"）。
- `pkg/tunnel/xfer/internal/tcp/tcp.go`：`Dial/Listen` 裸 `net`，init 注册 `"tcp"`，无 TLS。

### 3.2 目标 / DoD
1. sproxy 配 `hub.enabled` + `access_keys` + 新 `hub.transports.xfer_tcp`/`xfer_tls` 段后，`sclient tunnel --xfer tcp --hub 127.0.0.1:<port> /path` 直连 sproxy 本地文件 API（不再依赖测试 listener）。
2. TCP 数据面 TLS：`tcp+tls` 传输默认 TLS；裸明文 tcp 仅显式 option。
3. 服务端身份 + 指纹 pinning：客户端 `peer_fingerprints` = 服务端 xfer 身份指纹 → 握手成功；错 pin → fail-closed 拒绝。
4. 无 access_keys → xfer listener 拒启（fail-closed，对齐 M-8 语义）。
5. 连接经 `TryHandleConn` 入 hub 连接数上限（maxConns）；**不注册进路由表**（xfer 是文件 API 隧道面，非中继节点面；不参与节点/VIP/DHT）。
6. `docs/cli.md` N-3 标记更新为已接线。

### 3.3 关键架构决策

**AD-1：TCP 传输加 TLS 变体（而非改造 Registry 泛型）**
- `internal/tcp` 新增 `DialTLS/ListenTLS(ctx, addr, tlsCfg)`，`tcpConn` 底层换 `tls.Conn`（复用 `newTCPConn`）；注册名 `"tcp+tls"`（init 自动注册，`xfer.Get("tcp+tls")` 接入 `tunnel --xfer tcp+tls` / `relay --transport tcp+tls`）。
- Dialer 侧 TLS：默认系统根池 + `ServerName`（addr host）；`--ca-file` 复用 federation `loadCertPool` 模式；`--insecure-skip-verify` 仅限 loopback（复制 federation Config.Validate 限制）。

**AD-2：服务端装配 = 第二条 accept 循环 → Tunnel.Serve → LocalHandler**
- root.go `cfg.Hub.Enabled` 块内新增：`hubSrv.ListenTLS(ctx, listenAddr, tlsCfg)` → accept 循环 → `mux.New(conn, RoleListener)` → `tunnel.NewTunnel(m, key, WithIdentity(srvIdentity))` → `tun.Serve(ctx, h.tunnelHandler)`。
- 复用现成组件：`tunnel.NewTunnel` + `tunnel.Serve`（`pkg/tunnel/tunnel_mux.go:279` 已实现 listener 侧握手与 accept 循环）；handler 用 `server.RegisterRoutes` 造的 `tunnel.NewLocalHandler(nil, localApiHandler, …)`。

**AD-3：握手密钥一致性（本工作项核心正确性点）**
- listener 侧密钥 = `access_keys[0]` 的 SK → `tunnel.DeriveTunnelKey(sk, AccessKeyMesh(ak))`（`pkg/tunnel/tunnel.go:140,175` HKDF，salt 域分离 + info=mesh_id）。
- 客户端 `sclient tunnel --xfer` 已走同一派生（`cmd/sclient/internal/clientfactory/factory.go:199`）。两端同 AK/SK（同一 mesh 解析实现）→ 派生 key 相等 → ECDH 握手成功。**禁止客户端/服务端各写一套 mesh 解析**。

**AD-4：服务端身份解耦**
- 新增 `hub.xfer_identity_file`（默认 `<uploads-dir>/sproxy/server-identity.json`），`tunnel.LoadOrCreateIdentity(path)`（`pkg/tunnel/identity.go` 现成：不存在自动生成、损坏 fail-closed 不覆盖）。
- **TLS 证书与 Ed25519 身份解耦**：TLS 管传输机密性（复用 `cfg.TLS/*` cert/key，certmgr 抽成可复用 `newCertMgr(cfg)` 或独立 xfer_tls cert_file）；Ed25519 管握手 pin。

### 3.4 TDD 测试点（先失败）
1. `internal/tcp/tls_test.go`：DialTLS↔ListenTLS loopback 握手 + 载荷往返；错误 CA → 握手失败（复用 `xfertest` harness 加 TLS 变体）。
2. `xfer.Get("tcp+tls")` 注册非 nil。
3. Tunnel listener 侧 pin 矩阵：对端身份错误 → Serve 返回 error；正确 → accept 流入 handler。
4. **两端同 AK/SK → DeriveTunnelKey 相等 → loopback 全链路 `tunnel --xfer tcp` 上传/列表成功**（仿 `test/e2e_tunnel_accesskey_test.go`）。
5. `cmd/sproxy` 装配：xfer_tcp enabled 无 access_keys → 拒启；有 access_keys + 错 client pin → 客户端 `HandshakeErr`。
6. 全 `127.0.0.1`、`-race`、Windows 兼容。

### 3.5 风险与安全边界
- 明文 tcp 仅显式 option、文档警示；TLS 默认开校验、`insecure` 仅 loopback。
- xfer_tls listener 默认绑 loopback（`127.0.0.1:<port>`），远程可达须显式 `listen: ":<port>"`。
- 握手 30s 超时 + 连接信号量 + 1MiB 帧上限 + mux 心跳兜底；TLS record 序号天然防重放。
- 身份文件缺失自动生成 ≠ 匿名放行（握手仍需密钥）；损坏 fail-closed。

### 3.6 子任务（PR 粒度，依赖序）
1. **PR-1**：`internal/tcp` TLS 变体 + `tcp+tls` 注册 + 测试（纯 stdlib，x509 自签 loopback）。
2. **PR-2**：`pkg/server` 装配辅助 `XferListenerConfig` / `BuildXferTLSConfig` / `HubXferKey` + 身份 `LoadOrCreateIdentity` + 单测。
3. **PR-3**：root.go 第二 listener 装配 + fail-closed 校验 + 装配测试。
4. **PR-4**：sclient `--xfer tcp+tls` 的 `--ca-file`/`--insecure`（限 loopback）+ 服务端指纹获取文档化。
5. **PR-5**：e2e 全链路 + `docs/cli.md` N-3 更新 + `config.example.yaml` 补段。

---

## 4. 工作项 2：域路由 + 服务负载均衡（精确 + 多候选轮询）

### 4.1 现状（已核实）
- `pkg/tunnel/hub/router.go:139 ServiceHosts(name)` 返回同名多节点，`LookupService`（:155）精确匹配无调度。
- `pkg/client/mesh_refresh.go MeshTargetRefresher.Resolve` 内建多候选 failover（跳过 `lastFailedNode`，TTL 3s），但**按列表顺序取首个**，非轮询；失败由 `Invalidate(failedNode)` 打标。
- 候选从 `/api/hub/services`（`pkg/server/hub_handler.go:211 hubServicesHandler`）拉取。

### 4.2 目标 / DoD
1. `mesh connect <service>` 对同名多副本做 **round-robin + 失败跳过**（复用 `lastFailedNode`）。
2. `LookupService` / `mesh connect` / VIP 寻址 / `NewStaticMeshTargetRefresher` 全部零改动（行为差异仅在 `Resolve` 候选选取）。
3. 候选按 NodeID **排序固化**（map 遍历序不稳定），RR 确定性可测。

### 4.3 关键架构决策
- **调度加在 resolver 层而非 mesh.Dial**：resolver 是单点、dial 是热路径；`MeshGatewayDial`/`MeshVIPDial` 包装链不动。
- `MeshTargetRefresher` 增 `atomic.Uint64` 轮询游标 + 排序候选池；`Invalidate` 现有失败跳过机制保留扩展为多失败候选跳过。
- 不引入最少连接/健康检测（成本高，需动 dial 层计数）；RR 均匀 + 失败跳过 + `MeshConnect` 现有逐候选重试兜底。

### 4.4 TDD 测试点
1. 三候选 RR 1:1:1 分布断言（N 次 Resolve）。
2. 候选 0 失败 `Invalidate` → 后续跳过；全失败回退首个；TTL 缓存命中不更换。
3. e2e：多叶子同名 echo 服务连续建连测 RR；kill 一个叶子后 fallback（全 loopback + `-race`）。

### 4.5 子任务
1. **PR-1**：`MeshTargetRefresher` 轮询化（RR + 排序候选池）+ 单测。
2. **PR-2**：e2e 多副本 RR + 单点故障回归。

---

## 5. 工作项 3：动态 TURN（REST 短期凭证）+ 联邦/DHT 持久化

### 5.1 现状（已核实）
- TURN：`webrtc.go` `SetTURNServers/SetTURNCredential` 进程级全局静态密码，`defaultConfig()` 在 `newPC` 时组装——无动态/REST。
- 联邦：`pkg/tunnel/hub/federation.go` `FederationClient` 只 `cands map` 内存缓存，无 Save/Restore。
- DHT：`pkg/tunnel/hub/dht.go` `DHT` 接口无 Persist 方法；`ext/kad` `Kademlia` 无落盘。

### 5.2 目标 / DoD
1. **TURN REST（coturn 标准）**：新增 `SetTURNRESTURL(url, username, service)`；`defaultConfig()` 建 PC 前惰性拉取 `/turn?...` 返回 `{username, password, ttl}`（username=TTL:user、password=base64(HMAC-SHA1(key, username))），缓存至 TTL 到期前续期；静态凭据并存（REST 优先）；失败降级仅 STUN + 日志不 panic（回落 hub 中继不受影响）。
2. **联邦候选持久化**（发现缓存非权威）：`FederationClient` 加可选 `persistFile`，`SaveCandidates/restoreCandidates` 复用 `hub.Persister` 原子写；快照只存 ID/addr/mesh（**per-node secret 不落盘**）；损坏文件按空候选不 panic。
3. **DHT 桶落盘**：ext/kad `Kademlia` 增 `Save(path)/Load(path)`（k-bucket 序列化，nodeID hex + addr，非法条目丢弃）；`hub.dht_persist_file` 配置；缓存语义非权威。

### 5.3 涉及文件
- TURN：`pkg/tunnel/xfer/ext/webrtc/{webrtc.go, turnrest.go 新}` + `cmd/sclient/{mesh.go, mesh_node.go, p2p.go, relay.go}` `--turn-rest` 族 flag + `mesh.NodeConfig` + `config.example.yaml`。安全：默认强推 `https://`（`http://` 仅 loopback，复用 sync_remotes 明文校验模式）。
- 联邦持久化：`pkg/tunnel/hub/federation.go` + `cmd/sproxy/root.go`（`hub.federation.persist_file`）+ `pkg/server/config.go`。
- DHT：`pkg/tunnel/hub/ext/kad/kad.go`（子模块内 JSON 原子写，零新依赖）+ `cmd/sproxy/root.go`（`hub.dht_persist_file`）。

### 5.4 TDD 测试点
1. `turnrest_test.go`：httptest 假 REST server（loopback）返回短期凭证；TTL 内缓存不重拉；过期/404/非 2xx → 降级不 panic。
2. `turn_cred_test.go` 扩展：REST 就绪后 `iceserver` 含 TURN 且 username=TTL:user 格式；静态+REST 并存优先级。
3. `federation_test.go`：Save→Load→`Candidates()` 恢复；损坏文件 → 空候选。
4. `kad_test.go`：Save/Load 往返后 `GetClosestNodes` 一致；非法条目丢弃。
5. 装配：`persist_file` 缺省关闭（零行为变更）。

### 5.5 子任务
1. **PR-1**：TURN REST（webrtc 子模块 + httptest）→ CLI flag。
2. **PR-2**：联邦候选持久化（配置 + 装配 + 测试）。
3. **PR-3**：kad `Save/Load` + root.go 装配。
4. **PR-4**：文档（cli.md / config.example.yaml）+ e2e 冒烟。

---

## 6. 验证

每个子任务 PR 遵循既有流程（multi-stage-agent-orchestration）：
1. TDD：先写失败测试（核心 + 全部边界场景）。
2. `go test -race -count=1 <受影响包>/...` + `make lint`（主 + 每子 go.mod，0 issues）+ `make build-all` + `make test-all` + `make check-loopback` + `make web-test`（若涉前端）。
3. 派独立对抗式审查 agent：修复**全部**发现（含 Minor/参考级），必要时补回归测试再复审。
4. CI 全绿（Build×6 / Lint / Test ubuntu+windows / Benchmark / UI E2E / SonarQube）→ 用户人工 squash 合并。
5. 每阶段完成写 learnings（已入库）+ 阶段 5 复盘文档。

端到端手测样例：
- 工作项 1：本地起 sproxy（hub.enabled + access_keys + xfer_tls），`sclient tunnel --xfer tcp+tls --hub 127.0.0.1:<port> /path` 上传/列表成功；错误 pin 被拒。
- 工作项 2：两个叶子宣告同名 echo，连续 `mesh connect` N 次验证 RR 分布；kill 一个后自动跳过。
- 工作项 3：配 TURN REST 确认短期凭证 + TTL 续期；重启 hub 联邦候选/DHT 桶恢复。

## 7. 节奏

**前置铺底（2–3 天，小 PR 组）→ 工作项 1（4–5 天）→ 工作项 2（2–3 天）→ 工作项 3（3–4 天）**，合计约 2 周。工作项 1 是数据面 TLS 基础设施、优先验证；工作项 3 TURN 纯客户端依赖最少随后做。阶段 6（生产可用性：同步自动重试/审计/多租户/AK 轮换/metrics+OTel/Web UI 管理面）在阶段 5 合入后再规划排期。
