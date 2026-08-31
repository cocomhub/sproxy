# 阶段 4·虚拟 IP / 子网分配（工作项 1）交接简报

> 日期：2026-08-31
> 状态：设计已确认（v2），未开始实现；主流程 agent 上下文已达上限，交接给新上下文 agent 实施
> 设计：`docs/superpowers/specs/2026-08-30-sproxy-fullmesh-stage4-virtualip.md`（v2，含对抗审查修正）
> 分支：`feature/mesh-p4-virtualip`（已建，基于 master f472e8d）

## 1. 目标

Tailscale/ZeroTier 风格虚拟 IP 寻址：mesh 节点（node-id）获得稳定唯一虚拟 IP；`mesh connect <vip>:<port>` 拨号到对端节点本机 <port> 服务（虚拟主机语义）；本地网关 NAT 路由到对端。

## 2. 用户已确认的决策（2026-08-30）

1. **分配双实现**：hub 权威递增分配 + mDNS 无 hub 回落本地确定性哈希。
2. **虚拟主机语义 + 端口白名单（安全红线修正 C-1）**：`vip:port` 直通对端本机，但只放行 `--service` 宣告端口或 `--vip-allow-port`；未开放端口拒绝。
3. **REG_OK 下发自身 VIP（能力位）**：不依赖 discovery 环（Discover=false 的 relay 出口节点也不静默失效）。
4. **实现顺序**：文件同步先（已完成 A-E），虚拟 IP 后。

## 3. 关键架构决策（设计 v2）

- **AD-1 分配器抽象**：`Allocator` 接口 + `hubAllocator`（pkg/tunnel/hub/vip.go，递增分配 + 持久化 + 重启从快照重建）；`deterministicAllocator`（pkg/tunnel/mesh 实现 hub.Allocator，hash 到子网）。瞬态节点（disc-/mesh-/p2p-）用 `isTransientNodeID`（router.go:356）过滤不分配。
- **AD-2 默认子网**：CGNAT `100.64.0.0/10`（RFC 6598），可配置 `hub.virtual_subnet`（Validate 限 IPv4）；首地址保留网关，分配从 .2 起。第一版不按 mesh 划子块。
- **AD-3 虚拟主机 + 端口白名单**：出口 DialPolicy 识别「目标 host ∈ 虚拟子网 → ==selfVIP 且端口 ∈ allowPorts（宣告端口 ∪ --vip-allow-port）→ 改写 127.0.0.1:<port> 放行；==self 但端口不在白名单/!=self → 拒绝；虚拟子网外回落」。命中顺序：先 ServiceAddrs 精确匹配（真实 CGNAT 流量逃生口）再判虚拟子网。
- **AD-4 vipTable**：`map[netip.Addr]string`（虚拟 IP→peer node-id）；数据源 = hub 节点列表（带 virtual_ip）/ mDNS TXT `vip=`（签名）；一次性 CLI 用 `pkg/client.ListHubNodes` 拉 /api/hub/nodes。**防注入**：只接受认证数据源。
- **AD-5 CLI**：`mesh connect <vip>:<port>`（vipDialFunc 包装 meshDialFunc）、`mesh status` 显示 virtual_ip、`mesh node --virtual-subnet` + `--vip-allow-port`。
- **AD-6 数据通路**：vip→node 解析 → 已建链路 GatewayConnect 优先 / mesh.Dial 回落（webrtc/hub 中继）→ 写拨号帧 `{"dial":"<vip>:<port>"}` → 出口 DialPolicy 识别 selfVIP → 拨 127.0.0.1:<port>。

## 4. 子任务拆分（4 个，逐个 PR）

1. **A：hub 侧分配 + 下发**——`pkg/tunnel/hub/vip.go`（Allocator + hubAllocator + 快照重建）、`NodeInfo.VirtualIP`、`registerNode` 瞬态过滤 + 分配、`REG_OK` 能力位下发、`persist.go NodeSnap.VirtualIP`、`/api/hub/nodes` 带 virtual_ip、`config.HubConfig.VirtualSubnet` 三段式（IPv4 校验）。
2. **B：出口侧 NAT**——`pkg/tunnel/relay/leaf.go` `NewVirtualIPDialPolicy(subnet, selfVIP, allowPorts, allowCIDRs, serviceAddrs)`（宣告地址优先 + 端口白名单）；mesh node/relay start/p2p listen 装配。
3. **C：mesh 路由 + CLI**——`pkg/tunnel/mesh/vip.go`（vipTable + deterministicAllocator + ParseVirtualAddr）、`ListHubNodes` 扩展带 virtual_ip、gateway.go 虚拟 IP 分支（newGateway 注入 vipTable）、`VipDial`、`mesh connect <vip>:<port>`、`--virtual-subnet`/`--vip-allow-port`、status 显示、mDNS TXT vip=。
4. **D：E2E + 对抗式审查 + 修复**。

## 5. 安全红线（设计 §5 + 审查修正）

- 虚拟 IP 分配权在 hub（注册准入 SproxySig + HMAC proof），节点不可自选；vipTable 只接受认证数据源。
- mesh 隔离不破：虚拟 IP 只在所属 mesh 下发/路由。
- 出口 SSRF 边界：DialPolicy 只放行 ==selfVIP 且端口 ∈ 白名单，改写 127.0.0.1:<port>；虚拟子网内非本节点/非白名单端口拒绝。
- 默认 loopback；`--dial-allow` 门控不变。
- **端口白名单（C-1）**：mesh connect <vip>:18085（网关）/SOCKS/未宣告端口被拒。

## 6. 踩坑清单（阶段 2/3/4 经验，必须传给子代理）

1. **核心 go.mod 零三方依赖**：只 yaml.v3 + x/sys + x/crypto + x/net；禁止 `go mod tidy`。
2. **`.gitignore` 吞源码**：`data*` 模式曾吞 datagram.go；push 前 `git status` 确认新文件跟踪。
3. **Windows**：测试 127.0.0.1；`make check-loopback` 扫源码/注释；组播测试 loopback 收敛。
4. **Go 1.26**：`omitempty` 对 time.Time 无效 → `omitzero`。
5. **并发**：共享状态 -race 稳定；goroutine 监听 ctx.Done；`select{<-sendCh; default}` 非阻塞写。
6. **安全边界**：新监听默认 loopback、fail-closed 认证、SSRF 边界、mesh 隔离严格相等。
7. **logger**：直接 slog.Error，禁止 wrapper 导致行号偏移。
8. **防环/去重自洽**：不依赖可选配置静默失效（REG_OK 下发自身 VIP 解决）。
9. **出站拨号 TLS 阶段**：tls.Dialer.Timeout 不覆盖 TLS 握手。
10. **DoD 双保险**：自动（in-process + CLI 级 + -race 连跑 3 次）+ 手动真实二进制。
11. **测试环**：pkg/server 不 import pkg/client；接口 + 实现包打破环。
12. **CI flake**：死等固定超时必然 flake，用确定性信号（channel）同步。

## 7. 验证标准（每子任务）

- `go test -race -count=1 ./...`（受影响包）+ `golangci-lint` 0（主 + 每个子 go.mod）+ `make build-all` + `make test-all` + `make check-loopback`。
- 对抗式审查（全新上下文只读）全部发现（含 Minor/参考级）修复。
- PR → CI 全绿 → 用户 squash 合并 → 清理分支 → 写 `docs/superpowers/learnings/2026-08-31-stage4-virtualip.md`。

## 8. 参考接入点（已核实源码）

- `pkg/tunnel/mesh/gateway.go`（Gateway/GatewayConnect/gatewayRequest，:105-281）
- `pkg/tunnel/mesh/mesh.go:132` `mesh.Dial`（webrtc 优先→hub 中继回落）
- `cmd/sclient/mesh.go:30` `meshDialFunc`（CLI 选路注入点）
- `pkg/tunnel/hub/router.go:301` `registerNode` + `isTransientNodeID`（:356）
- `pkg/tunnel/hub/persist.go`（NodeSnap + onChange 驱动落盘）
- `pkg/tunnel/relay/leaf.go:529` `NewServiceDialPolicy`
- `pkg/server/relay_stream.go:174`（中继回落翻译点，可选增强）
- `pkg/tunnel/hub/mesh_route_table.go`（NodeByVirtualIP 反查）
- `cmd/sclient/mesh_node.go`（--virtual-subnet/--vip-allow-port 接入）
