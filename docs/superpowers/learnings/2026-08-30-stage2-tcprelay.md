# 阶段 2 子任务 5：TCP relay 独立使能

> 日期：2026-08-30
> 分支：`feature/mesh-p2-tcprelay`
> 状态：已实现 + 测试全绿 + lint 0 + 审查闭环 + PR 就绪

## 功能摘要

hub 在现有 WS 中继之外支持**裸 TCP 中继**：`sclient relay start --transport tcp` 走裸
TCP 注册 + 数据面；`relay dial` 在**无 WS 场景**下也能经 `/api/relay/stream` 中继拨号
成功。传输层从 ws 扩到 tcp，注册/鉴权/中继/信令协议零改动（`xfer.Conn` 载体不同）。

DoD（双保险）：无 WS 纯 TCP 场景下 `relay dial` 通——in-process 全链路测试 +
CLI 级测试 + 真实二进制手动验证均通过。

## 新增/修改文件

| 文件 | 动作 | 职责 |
|------|------|------|
| `pkg/tunnel/xfer/builtin/builtin.go` | 新建 | 对外注册桥：blank import `internal/tcp`，让 cmd/hub 能用内置 TCP 传输（internal 可见性约束） |
| `pkg/tunnel/xfer/builtin/builtin_test.go` | 新建 | tcp 注册 + 真实往返 |
| `pkg/tunnel/xfer/internal/tcp/tcp.go` | 修改 | Receive 尊重 ctx deadline + 1 MiB 单条上限 + Send 写超时 60s + 超长/错位读 failConn 断连 + closed 改 atomic.Bool |
| `pkg/tunnel/xfer/internal/tcp/tcp_test.go` | 修改 | ctx 超时/关闭解除阻塞/超大帧断连回归 |
| `pkg/tunnel/hub/router.go` | 修改 | `HubServer.ListenTCP`/`AcceptTCP`（复用 HandleConn 注册/鉴权/中继） |
| `pkg/tunnel/hub/tcp_server_test.go` | 新建 | TCP 注册/准入失败/断开移除/并发注册/ctx 取消 |
| `pkg/server/config.go` | 修改 | `hub.transports.tcp.{enabled,listen}` + 端口冲突校验 |
| `pkg/server/config_test.go` | 修改 | TCP 配置解析/默认/校验 |
| `pkg/server/hub_tcp_relay_test.go` | 新建 | DoD 全链路 + 并发拨号 + 404 |
| `pkg/server/relay_stream.go` | 修改 | `targetMux.Open` 受 relayStreamDialResultTimeout 约束 |
| `cmd/sproxy/root.go` | 修改 | hub 装配重构（HubServer 提升到 ws/tcp 共用）+ TCP listener |
| `cmd/sclient/relay.go` | 修改 | `--transport tcp` + ws:// URL 清晰错误 |
| `cmd/sclient/relay_tcp_test.go` | 新建 | CLI 级 DoD + flag 默认/校验 |
| `config.example.yaml` | 修改 | TCP 传输配置文档 |

## 架构决策

1. **xfer/internal/tcp 的 internal 可见性**：`internal/tcp` 仅能被 import 路径以
   `pkg/tunnel/xfer` 为根的包引用；`cmd/sproxy`、`cmd/sclient`、`pkg/tunnel/hub` 都无法
   直接 blank import。新建 `pkg/tunnel/xfer/builtin` 作对外注册桥（blank import
   `internal/tcp`），任何包 `import _ "pkg/tunnel/xfer/builtin"` 即可保证
   `xfer.Get("tcp")` 可用。放 `pkg/tunnel/xfer/` 下而非更外层，是因为 internal 规则。
2. **hub 侧 TCP 复用 HandleConn**：`HubServer.ListenTCP`（同步绑定，fail-fast）+
   `AcceptTCP`（accept 循环 + TryHandleConn 信号量）——与 WS accept 循环等价，仅传输层
   不同。ws/tcp 共用同一 HubServer（路由表/信号量/鉴权器）。
3. **sclient `--transport tcp`**：`runRelayOnce` 内 switch 传输层，tcp 走
   `xfer.Get("tcp").Dial(ctx, hubURL)`（hubURL 为裸 host:port），ws 走 `mesh.HubWSDial`。
   注册/信令/数据面协议完全一致。
4. **TCP 传输健壮性**：裸 TCP 是网络面 raw listener，补三件事——(a) Receive 尊重 ctx
   deadline（注册帧 10s 超时不能永久占连接槽/goroutine）；(b) 单条消息 1 MiB 上限（防
   恶意超大长度前缀 OOM）；(c) Send 写超时 60s（peer 停读不永久阻塞）。与 WS 传输对齐。
5. **安全边界（loopback 默认）**：`hub.transports.tcp.listen` 默认 `127.0.0.1:18084`
   （loopback）——裸 TCP 中继是网络面服务，全接口绑定属 SSRF/暴露面攻击目标；远程可达
   需显式配 `":18084"` 或具体网卡 IP。注册准入由 SproxySig AccessKey + HMAC proof
   fail-closed 保证（未配置 access_keys 时拒绝所有注册）。明文 TCP 无 TLS（WS 可用
   WSS），远程 WAN 场景应优先 WSS 或 VPN——文档化取舍。

## 测试矩阵

| 测试 | 类型 | 覆盖 |
|------|------|------|
| `TestBuiltinRegistersTCP` / `_RoundTrip` | 单测 | builtin 注册桥 + TCP 往返 |
| `TestTcpReceive_RespectsCtxTimeout` | 单测 | Receive 尊重 ctx deadline（注册帧超时不再无限阻塞） |
| `TestTcpReceive_CloseUnblocksBlockedReceive` | 单测 | cancel-only ctx + conn.Close 解除阻塞（mux 收尾路径） |
| `TestTcpReceive_RejectsOversizedMessage` | 单测 | 超大长度前缀拒绝 + 错误后连接关闭（ErrConnClosed） |
| `TestHubTCP_RegisterViaTCP` | 单测 | 叶子经裸 TCP 注册进路由表（无 WS） |
| `TestHubTCP_InvalidAccessKeyRejected` / `_EmptyAccessKeysFailClosed` | 单测 | 错误 AK/空 AK fail-closed |
| `TestHubTCP_DisconnectRemovesNode` | 单测 | TCP 断开后节点异步移除 |
| `TestHubTCP_ConcurrentRegistrations` | 单测 | 8 叶子并发注册（保持连接存活再断言，防移除竞争） |
| `TestHubTCP_AcceptCtxCancel` | 单测 | AcceptTCP ctx 取消正常返回 |
| `TestTCPRelay_NoWS_RelayDial` | DoD 集成 | 无 WS 纯 TCP：叶子注册 → relay dial → echo（in-process 全链路） |
| `TestTCPRelay_NoWS_ConcurrentRelayDial` | DoD 集成 | 6 并发 relay dial 同叶子（mux 多路复用无串流） |
| `TestTCPRelay_NoWS_TargetNotFound` | 集成 | 未注册目标 404 |
| `TestRelayStart_TCPTransport_NoWS_RelayDial` | CLI DoD | 真实 `runRelayOnce`（--transport tcp）+ relay dial |
| `TestRelayStartCmd_TransportFlag` | CLI | --transport flag 默认 ws + 非法值校验 |
| `TestConfig_SetDefaults_TCPListen` / `_Validate_TCPPortConflict` / `_TCPTransport` | 配置 | 默认 loopback、端口冲突校验、YAML 解析 |

## 关键坑

1. **xfer/internal/tcp 无法被外部包引用**：internal 可见性规则（import 路径须以
   `pkg/tunnel/xfer` 为根）。建 `xfer/builtin` 桥。不能直接改 xfer 包（会形成
   xfer → xfer/internal/tcp → xfer 循环）。
2. **TCP Receive 原实现忽略 ctx**：注册帧 10s 超时对 TCP 无效——对端连上不发数据即永久
   占连接槽 + goroutine（hub 裸 TCP 的 DoS 面）。修复：ctx deadline 映射 socket 读
   deadline。
3. **`tcpConn.closed` 数据竞争**：Receive 无锁读、Close 有锁写 → -race 报错。改
   atomic.Bool。
4. **超长帧不断连 = 帧对齐破坏**：Receive 遇到超大长度前缀若只返回错误，mux readLoop
   会当瞬时错误重试，但未读的 body 字节残留 → 后续帧错位解析（WS 超限即断连，TCP 需
   显式 failConn）。修复：failConn 关闭连接 + 返回 ErrConnClosed 包装错误。
5. **并发注册测试与异步移除竞争**：hub 断开节点是异步的（mux readLoop EOF 退避重试后
   m.Close），测试若注册后立即 close conn 再断言 rt.Has 会与移除竞争。修复：注册后保持
   连接存活，断言完成后再关闭。
6. **裸 `:port` 默认绑定全接口**（协调者 MEDIUM 反馈）：裸 TCP 是网络面服务，全接口绑定
   属 SSRF/暴露面。修复：默认 loopback `127.0.0.1:18084`，远程可达需显式配置；同时加
   `hub.transports.tcp.listen` 与主 HTTP `addr` 同端口冲突校验。

## 独立审查（对抗式）发现与修复

- **I1（必须，帧对齐）超长帧错误不断连**：Receive 对超大长度前缀只返回错误，mux
  readLoop 当瞬时错误重试，残余 body 字节破坏帧对齐（悬挂 ~90s 等心跳）。修复：
  `failConn()` 关闭连接 + 返回 `xfer.ErrConnClosed` 包装错误；回归测试断言错误后连接已
  关闭。
- **I2（必须，flaky）并发注册断言竞争**：`TestHubTCP_ConcurrentRegistrations` 依赖
  "EOF 重试很慢" 的实现细节。修复：注册后保持连接存活再断言（轮询等待全注册），断言后
  关闭。
- **S1（建议）TcpListener.Accept 孤儿连接 FD 泄漏**：ctx 取消窗口内 accept goroutine
  送出的 conn 无人读取。修复：与 WS HandlerNode 对齐，`select { connCh <- c; <-closeCh:
  c.Close() }`。
- **S2（建议）--transport tcp 传 ws:// URL 报错难懂**：修复：显式检测 ws:// 前缀并报
  "--transport tcp 的 --hub 应为 host:port"。
- **S3（建议）拨号写阻塞叶子挂起**：hub `conn.Send`（ACK 写）持 TCP `tcpConn.mu` 最长
  60s，relay dial 的 `targetMux.Open` 会等锁。修复：`ServeHTTP` 的 Open 受
  relayStreamDialResultTimeout（12s）约束。
- **S4（建议）阻塞读测试未覆盖真实 mux 路径**：`TestTcpReceive_CloseUnblocksBlockedReceive`
  用带 deadline ctx 走错了分支。修复：改 cancel-only ctx。
- **S5（建议）配置注释过时**：默认地址文案更新为 127.0.0.1:18084。

## 未修复（文档化取舍）

- **明文 TCP 无 TLS**：注册帧（AK/nonce/HMAC proof）与数据面明文（WS 可用 WSS）。loopback
  默认缓解本机暴露；远程 WAN 应优先 WSS 或 VPN。为 TCP 传输加 TLS 属未来工作。
- **未认证连接占槽 10s**：客户端连上不发数据占一个 maxConns 槽 10s（256 半开可短暂阻断
  新注册）。WS `/ws` 同等暴露；loopback 默认 + fail-closed 认证已缓解。per-IP 连接限流
  属未来加固。
- **`newTCPPair` Dial 失败时 accept goroutine 短暂孤儿**：仅测试卫生，由 t.Cleanup 关
  listener 兜底（有界）。

## 验证

- `make test-all` 全模块绿（-race）；`make build-all` 10 模块；`make lint` 0（主 + 各子
  module）；`make check-loopback` 通过；`go vet`/`go fix` 干净。
- DoD 测试 `TestTCPRelay_NoWS_RelayDial` + `TestRelayStart_TCPTransport_NoWS_RelayDial`
  -race 连跑 3 次稳定；**手动真实二进制验证通过**（无 WS 纯 TCP hub + `relay start
  --transport tcp` + `relay dial` echo 往返）。
- 依赖：零新增三方依赖（TCP 是 `xfer/internal/tcp` 内置实现；根 go.mod 仅
  yaml.v3 + x/sys + x/crypto，未污染）。
