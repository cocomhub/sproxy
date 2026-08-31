# 阶段 3 子任务 2：hub 联邦 A——节点表同步（hub-to-hub peering）

> 日期：2026-08-30
> 分支：`feature/mesh-p3-federation-sync`
> 状态：已实现 + 测试全绿 + lint 0 + 审查闭环 + CI 全绿 + 待人工 squash 合并（PR #126）

## 功能摘要

hub-to-hub peering：联邦 hub 之间周期交换节点表（同步 + 去重 + 生效）。
`/api/hub/nodes` 合并联邦候选（**路由表仍本 hub 权威，联邦只提供发现/可达性**）。
入站端点 `/api/hub/federation/nodes` 供对端拉取本 hub 路由表节点（带 mesh，按调用方
mesh 过滤，只返回路由表防同步环路）。

## 新增/修改文件

| 文件 | 动作 | 职责 |
|------|------|------|
| `pkg/tunnel/hub/federation.go` | 新建 | `FederationClient` 周期拉取对端节点表 + per-peer TLS（ca_file/insecure/默认严格）+ body 限流 + stale-while-error |
| `pkg/tunnel/hub/federation_test.go` | 新建 | 拉取/去重/mesh 保留/并发/ctx 取消/CA 池/默认严格校验测试 |
| `pkg/server/hub_handler.go` | 修改 | `federationNodesHandler`（入站端点）+ `mergeFederationNodes`（联邦候选合并，mesh 严格隔离） |
| `pkg/server/hub_federation_test.go` | 新建 | 合并/隔离/认证两侧/真实签名 mesh 过滤/双 hub peering 测试 |
| `pkg/server/config.go` | 修改 | `hub.federation.*` 配置 + Validate（远程凭据成对、ca_file 互斥、insecure 限 loopback） |
| `pkg/server/config_federation_test.go` | 新建 | 配置默认值/校验边界测试 |
| `pkg/server/handlers.go` | 修改 | `SetFederationClient` + 联邦端点路由注册（authMiddleware 保护） |
| `cmd/sproxy/root.go` | 修改 | 联邦装配（NewFederationClient 返回 error，CAFile 传递） |
| `config.example.yaml` | 修改 | hub.federation 文档（ca_file / insecure loopback 限制 / mesh 凭据对齐） |
| `test/e2e_federation_test.go` | 新建 | CLI 级真实双二进制 + sclient relay 注册互见 |

## 架构决策

1. **联邦只做发现，不改路由表**：联邦候选（`FederationNode{ID,Addr,Mesh}`）只进
   `/api/hub/nodes` 合并，绝不写入 `MeshRouteTable`（本 hub 无法转发到远程节点，
   转发仍按路由表）。与 DHT 候选一致，满足「路由表权威；联邦只提供发现/可达性」。
2. **入站端点防环路**：`/api/hub/federation/nodes` 只返回本 hub 路由表节点
   （不合并 DHT/联邦候选），否则 A 拉 B、B 又拉 A 造成无限回声。
3. **mesh 隔离严格相等比较**：合并用 `c.Mesh != mesh`（空 mesh 只对默认 mesh 请求者
   放行），与 `mergeDHTNodes` 一致（阶段 2 DHT 曾踩过默认 mesh 泄漏）。入站端点按
   调用方 AK 派生 mesh（`authMiddleware` → `AccessKeyMesh`）过滤，拉取方用哪个 mesh
   凭据只能拿哪个 mesh 的节点——peering 的 mesh 对齐由「双方共享同一 mesh 凭据」保证。
4. **默认 loopback 安全面**：peer.URL 空回落 `http://127.0.0.1:18083`；远程 peering
   必须显式配置 URL + 成对凭据（Validate fail-closed）。
5. **TLS 安全面（S-Medium 闭环）**：per-peer http.Client。ca_file（PEM 受信 CA）→
   专属证书池严格校验；insecure_skip_verify → **仅限 loopback**（远程禁止，Validate
   拒绝）；默认 → 系统根池严格校验（fail-closed，不静默降级）。远程自签 hub 的正确
   配置是 ca_file 而非跳过校验。
6. **核心 go.mod 零三方依赖**：全程标准库（crypto/tls、crypto/x509、net/http、os），
   未执行 `go mod tidy`，无新依赖。

## 安全边界（DoD 2）

- 入站联邦端点受 `authMiddleware`（SproxySig）保护，fail-closed：hub 配置 access_keys
  后无凭据请求 401。
- 出站拉取用 `sproxysig.SignRequest` 签名（SK 只本端计算，永不上线）；sk 为空时
  不签名（仅限目标 hub 无认证调试模式）。
- 空 ID 节点丢弃；响应 body 4 MiB 限流；重复 peer（空 URL/空 ID 归一冲突）启动拦截。

## 踩坑与经验

1. **TLS 校验收紧的连锁调整**：`NewFederationClient` 从返回单值改为 `(*Client, error)`
   （CA 读取失败 fail-fast），波及 root.go 与全部测试调用点。批量 sed `fc := X(` →
   `fc, _ := X(` 时漏掉 `fc2 := X(` 一行（前缀不匹配），编译才发现——**批量替换后
   必须 go build 兜底**。
2. **config 校验顺序陷阱**：hub.enabled=true 会先触发「transports 必须启用」校验，
   联邦配置单测需 `Hub.Enabled=false` 隔离（联邦校验门控在 federation.enabled 而非
   hub.enabled）。
3. **远程 peer + insecure 被新限制拒绝**：既有测试 `TestFederationConfig_Validate_RemotePeerWithCreds`
   配了 `InsecureSkipVerify:true`，被 loopback 限制新校验拒绝——安全收紧必然打破旧
   测试，需同步改测试语义（远程改走受信任证书）。
4. **e2e 进程残留**：手动验证时 sproxy/sclient 进程要 taskkill 干净（sproxy 与
   sproxy.exe 是两个可执行体），残留进程占用端口导致后续验证假失败。
5. **httptest.NewTLSServer 自签证书**：证书可用 `srv.Certificate()` 取到，转 PEM 写
   临时文件即作 ca_file 测「受信 CA 严格校验成功」；错误 CA 用自生成证书（ecdsa）。
6. **既有 e2e flaky 判断**：`TestE2E_MeshNode_ServiceAccess` 本机失败但 origin/master
   同样失败 → 环境 flaky（webrtc 网关 i/o timeout），与本次改动无关；CI（ubuntu/
   windows）上通过。判断方式：detached HEAD 切回 origin/master 复跑对比，而非凭直觉。

## 验证

- `go test -race -count=1` hub/server/核心 module 全量通过；e2e 联邦 `-race` 连跑 3 次通过。
- 主 module + 全部子 module `golangci-lint` 0 issues；`make build-all` / `test-all` /
  `check-loopback` / `fmt` 通过。
- CI（ubuntu + windows）：Build/Lint/Test/SonarQube/UI E2E/Benchmark 全 pass。
- 手动双 hub 真实二进制：sclient relay 注册 node-b 到 hub-B，hub-A 经联邦 `/api/hub/nodes`
  看到 node-b；无凭据请求联邦端点 401（fail-closed）。
