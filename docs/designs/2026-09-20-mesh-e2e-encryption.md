<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# mesh 端到端加密 + SmartDial 竞速修正实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。
> 步骤使用复选框（`- [ ]`）语法跟踪进度。
> **文档生命周期**：本文件为分支内过程产物，合并前必须收敛进权威文档 + 归档精华，不留 master（见
> `.pi/skills/sproxy-docs-lifecycle/SKILL.md` §2）。

**目标：** 修复 mesh 多跳（via-node / via-direct）的安全缺口与竞速精度缺口：① 端到端加密（数据面密钥与 SK 解耦），
根治"中间人 X 持有 SK 即可解密"；② SmartDial 竞速改为**最终到达目标的整体链路耗时**比较；③ 纳入已知注意问题（信任收敛、
多跳竞速窗口加权、mDNS 信任、出口审计）。

**架构：** 数据面加密复用 `pkg/tunnel` 的 `Tunnel`（ECDH 会话密钥 + Ed25519 身份 + 指纹 pinning，`remote_read_listener`
已验证），密钥材料**不来自 SK**（X 持有 SK 也读不到明文）；竞速度量在既有 `DialResultFrames`（I27，hub 中继 200=数据面就绪）
与 webrtc 直连回帧基础上，把 `Latency` 定义为**首字节可读时刻**（真实端到端链路就绪，而非各段建连耗时的加法近似）。

**技术栈：** Go 1.27；`pkg/tunnel`（AES-256-GCM + ECDH + Ed25519）、`pkg/tunnel/mux`、`pkg/tunnel/mesh`（SmartDial）、
`pkg/tunnel/xfer/ext/webrtc`、`pkg/tunnel/hub`（信令/中继）、`pkg/server`（relay_stream）。

**规格：** 无独立规格文档（本计划即规格）。设计决策来源与安全分析见 `docs/archive/mesh-evolution.md` §6
（SmartDial/via-node/via-direct 演进）与本文件 §1 背景。

---

## 全局约束

- 代码、注释、日志、错误信息、CLI 文案一律简体中文（代码/命令/路径保持原文）。
- 所有 Go 源文件携带 SPDX 头（`addlicense` 强制，`make fmt` 自动）。
- **纯标准库测试**：不用 testify/gomock/gomega；测试只绑 `127.0.0.1`；Windows 兼容。
- **R18 并发门禁**：顶层 `TestX(t)` 默认必须 `t.Parallel()`；无法并行的必须显式豁免登记
  （`internal/archcheck/serial_budgets.tsv`，上行同步 `docs/testing/virtual-time-conversions.md`）。
- **测试网络客户端隔离**：禁 `http.DefaultClient`；每测试自建 `&http.Client{Transport: &http.Transport{}}`。
- **webrtc 测试铁律**：任何 webrtc Dial/Listen 测试必须 `webrtctest.New(t)` + `SetHostOnly(true)` **成对使用**
  （只 SetHostOnly 不够——pion/ice 仍会全接口 ListenUDP，Windows 弹防火墙授权）。
- 提交用 suixibing 身份；禁 `Co-authored-by`；禁 `--no-verify`；提交前 `export PATH="$PATH:$(go env GOPATH)/bin"`。
- 提交类型遵循 Conventional Commits（`feat(mesh)!:` 格式，scope 后感叹号）；破坏性变更必须标 `!` 或
  `BREAKING CHANGE:` footer（release-please 纪律，见 `.pi/skills/sproxy-release-discipline/SKILL.md`）。
- 本仓规则文档：`docs/superpowers/learnings/2026-09-13-agent-operating-rules.md`（§1/§2 三子项目通用）。
- **R4 分层门禁**：`pkg/tunnel/mesh` 测试不得 import `pkg/server`（e2e 自建 in-process hub + 最小信令桥）。

---

## 背景与决策记录（2026-09-19/20 分析结论）

### §1.1 安全缺口：SK 是群密钥，中间人 X 必持有

经代码核实（`cmd/sclient/relay.go`、`pkg/tunnel/hub/auth.go`、`pkg/tunnel/mesh/node.go`）：

- **X 必须持有 SK 才能注册为候选节点**：`ComputeRegisterProof(accessKeySecret, ...)`（`auth.go:56`）以 SK 计算注册
  proof，hub 用 ring 条目验证——**无 SK 无法成为 via-node 候选**。
- 因此"X 是恶意中间人"场景里，**X 几乎总是持有 SK**；而当前数据面加密（xfer/tunnel 各段）全部以 SK 派生
  （`DeriveTunnelKey(skHex, meshID)`，`tunnel.go:141`）——X 可用同一公开公式派生密钥解密 L→T 密文。
- **结论**：SK 是"群准入凭证"，不能兼任"端到端数据面密钥"；端到端加密必须**与 SK 解耦**。

### §1.2 数据面现状（已核实）

| 数据面段 | 现状 | 泄露风险 |
|---|---|---|
| L ⇄ hub（via-relay） | 隧道协议自带 AES-256-GCM（SK 派生密钥） | hub 不可读（但 X 可派生同款密钥） |
| L ⇄ X（via-direct，webrtc 直连） | SRTP 加密（webrtc 内置 DTLS-SRTP） | X 只看到加密流 |
| X → T（**所有多跳出口段**） | **裸 TCP 明文**（`relay/leaf.go:204 net.DialTimeout("tcp",...)` 后直接 pump） | **X 看到全部明文** |

### §1.3 复用能力盘点

- `tunnel.Tunnel`（`tunnel_mux.go`）：ECDH 会话密钥 + 静态密钥参与派生（C-1）+ Ed25519 身份 + 指纹 pinning
  （`WithIdentity` / `WithPeerFingerprints`），握手失败 fail-closed——`remote_read/write_listener` 已用该模式
  （`server-identity.json` 身份 + `mesh_readers` 指纹白名单）。
- `cmd/sclient/identity.go`：`identity generate/show/fingerprint` 子命令已存在（生成 Ed25519 密钥对），
  **但 mesh connect 主链路未接入**（`pkg/tunnel/mesh/*.go`、`cmd/sclient/mesh.go` 中无 `NewTunnel` 调用）。
- `DialResultFrames`（I27，`relay/leaf.go` + `pkg/server/relay_stream.go:258`）：叶子出口拨号后回
  `[4B len][{"dial_result":"ok"}]`，hub 写 200 前读该帧——**200 语义 = "数据面就绪"（含 X→T 拨号完成）**。
  当前实现**只用于 hub 中继路径**（`node.go:224`）；webrtc 直连必须关闭（`mdns_node.go:113`：结果帧会污染数据流）。
  数据面就绪时点 = 叶子上游 net.Conn 已建立 → **端到端链路已通**，可用作"整体链路耗时"的度量锚点。

### §1.4 关键设计约束（用户确认）

1. **竞速必须用"最终到达目标的整体链路耗时"比较**——不是各段建连耗时的加法近似，而是首字节可读时刻。
   - via-relay：`RelayStreamWithHeaders` 的 200 响应已含"叶子数据面就绪"语义（I27），`Latency = time.Since(start)`（发起→200）；
   - via-direct：webrtc 直连**不适用** DialResultFrames（污染数据流），需**独立回帧通道**确认 X 出口拨号完成
     （见任务 2.2 设计：mux 流首帧拨号结果，非数据面内联帧）；
   - direct：`DialWebRTC` 打洞完成即数据面就绪（webrtc 直连无中间出口拨号，打洞即链路通）。
2. **SmartDial 按建议修改**：`--smart` 默认**保持 false**（零回归），多跳候选竞速窗口加权等（见任务 2.3）。
3. **其他问题纳入排期**（见 §1.5）。
4. **端到端加密与 SK 解耦**（§1.1 结论），复用 `Tunnel` + 指纹 pinning。

### §1.5 纳入排期的其他问题（分析结论，对应任务）

| # | 问题 | 任务 |
|---|---|---|
| N1 | 端到端加密：X 有 SK 也能解密当前数据面（§1.1） | T1（本片） |
| N2 | 竞速度量非"整体链路耗时"（§1.3 锚点分析） | T2（本片） |
| N3 | mDNS 无 hub 场景信任薄弱（仅 `--mdns-secret` 共享密钥，无指纹 pinning） | T4（本片，身份广播 + 指纹 pinning） |
| N4 | `--trust-x` 中间节点白名单（信任收敛） | T5（本片） |
| N5 | 多跳候选竞速窗口加权（5s 竞速窗口系统性偏袒短路径） | T2.3（本片） |
| N6 | 出口拨号审计日志（via-direct 与 via-relay 共用 relay.Serve，需可追溯） | T6（本片） |
| N7 | P0 可见性：Kind+Latency 展示、metrics 路径维度 | T7（本片） |
| N8 | 主动探测 `--probe`（链路劣化感知） | 下片（非本片） |
| N9 | QUIC/TCP+TLS 作为竞速候选 | 下片（非本片） |

**安全结论一句话**：SK 是"群准入凭证"，不是"端到端加密密钥"——拆开这两件事（复用 `Tunnel` 的 ECDH + 指纹 pinning），
X 有 SK 也只能中转密文，读不到明文。

---

## 文件结构

### 本片改动（T1-T7，逐任务独立提交）

| 文件 | 职责 | 动作 |
|---|---|---|
| `pkg/tunnel/mesh/endtoend.go`（新） | 端到端加密拨号/接受：L 侧 `DialE2E`（Tunnel 内层）+ X 侧出口 `ServeE2E`（隧道流透传） | 创建 |
| `pkg/tunnel/mesh/endtoend_test.go`（新） | TDD 红灯测试（见任务 1） | 创建 |
| `pkg/tunnel/mesh/smart.go` | 竞速度量改整体链路耗时（Latency 定义 + via-direct 回帧）；多跳窗口加权 | 修改 |
| `pkg/tunnel/mesh/via_node.go` | via-direct 出口结果回帧（独立回帧通道） | 修改 |
| `pkg/tunnel/mesh/via_node_test.go` | via-direct 回帧单测 | 修改 |
| `pkg/tunnel/mesh/smart_test.go` | 竞速度量新断言（整体链路耗时） | 修改 |
| `pkg/tunnel/mesh/mesh.go` | Result 扩展（EndToEndKind / 链路耗时字段） | 修改 |
| `pkg/tunnel/mesh/mdns_node.go` | mDNS 身份广播（指纹） + 接受侧指纹 pinning | 修改 |
| `pkg/tunnel/mesh/node.go` | NodeConfig 身份字段 + 装配 | 修改 |
| `cmd/sclient/mesh_node.go` | `--trust-x` 白名单 flag + 装配 | 修改 |
| `cmd/sclient/mesh.go` / `socks.go` | `--smart` 语义更新（默认 false）+ 出口审计日志接线 | 修改 |
| `pkg/server/metrics.go` | 竞速结果 metrics 路径维度（新增 label） | 修改 |
| `pkg/tunnel/relay/leaf.go` | 出口拨号审计日志（谁拨了哪个地址、经哪条路径） | 修改 |
| `docs/cli.md` / `docs/config.md` / `docs/tunnel.md` / `docs/mesh-testing.md` | 权威文档收敛（新 flag/配置/协议语义） | 修改 |
| `docs/archive/mesh-evolution.md` | 精华归档（端到端加密 + 竞速度量演进） | 修改 |

### 下片（非本片，仅登记）

- `--probe` 主动探测（N8）、QUIC/TCP+TLS 竞速候选（N9）。

---

## 任务 1：端到端加密（N1，安全最高优先）

**文件：**
- 创建：`pkg/tunnel/mesh/endtoend.go`
- 创建：`pkg/tunnel/mesh/endtoend_test.go`
- 修改：`pkg/tunnel/mesh/mesh.go`（Result 扩展）

**设计要点**（复用 `Tunnel`，密钥与 SK 解耦）：

```go
// DialE2E 是 L 侧端到端加密拨号：在外层数据面（webrtc 直连 X / hub 中继到 X）之上
// 建 tunnel.Tunnel，会话密钥 = ECDH(L身份, T身份) + 静态密钥（公开指纹派生，非 SK）。
// X 只透传密文流（Tunnel 数据面与 mux 帧混流，X 无 L/T 私钥无法派生会话密钥）。
// opts.EndToEnd 启用；身份经 opts.Identity（Ed25519 密钥对）提供，指纹 pinning 经
// opts.PeerFingerprints（T 的公钥指纹白名单）。
func DialE2E(ctx context.Context, outer net.Conn, opts EndToEndOptions) (net.Conn, error)

// ServeE2E 是 X 侧出口：把外入的 mux 流（隧道密文）原样透传到 X→T 出口连接。
// X 不接触明文——pump 层透传密文字节。
func ServeE2E(ctx context.Context, stream mux.Stream, dialPolicy DialPolicy) error
```

**关键约束**：
- 静态密钥 = `tunnel.DeriveRemoteStaticKey(本地指纹)`（公开指纹派生，非 SK）——与 `remote_read_listener` 一致，
  但**不**用它做数据面主密钥；真正的会话密钥 = ECDH(L 私钥, T 公钥)，X 无法派生。
- 指纹 pinning fail-closed：无 pin 拒绝（对齐 `remote_read_listener` "无 pin 拒绝启动"）。
- `EndToEndOptions`：`Enabled bool` / `Identity *tunnel.Identity` / `PeerFingerprints []string` / `StaticKey []byte`。

- [ ] **步骤 1：编写失败的测试**（`endtoend_test.go`）

```go
// TestDialE2E_MiddlemanWithoutKeysCantRead：中间人 X 只透传密文，无 L/T 私钥读不到明文。
func TestDialE2E_MiddlemanWithoutKeysCantRead(t *testing.T) {
    t.Parallel()
    // 1. 生成 L 与 T 的 Ed25519 身份（tunnel.GenerateIdentity）
    // 2. 建 127.0.0.1 回环：L --net.Pipe-- X(middleman 只 pump) --net.Pipe-- T
    // 3. L 侧 DialE2E(身份 L, pin=T指纹) → 经 X 透传 → T 侧 ServeE2E 出口到本地 echo
    // 4. L 写明文 "TOP-SECRET-PAYLOAD" → 从 echo 读回一致
    // 5. 关键断言：X 的 pump 通道里只出现密文（不含 "TOP-SECRET" 明文子串）
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run TestDialE2E_MiddlemanWithoutKeysCantRead ./pkg/tunnel/mesh/`
预期：FAIL（`DialE2E`/`ServeE2E` 未定义，编译失败即红灯）

- [ ] **步骤 3：实现 `DialE2E` / `ServeE2E` + Result 扩展**

（最少实现：复用 `tunnel.NewTunnel` + `WithIdentity` + `WithPeerFingerprints`；外层数据面用 `net.Pipe` 注入；
X 侧 pump 透传。）

- [ ] **步骤 4：运行测试验证通过**

运行：同上命令
预期：PASS；`-race` 通过

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mesh/endtoend.go pkg/tunnel/mesh/endtoend_test.go pkg/tunnel/mesh/mesh.go
git commit -m "feat(mesh): 端到端加密（ECDH 会话密钥 + 指纹 pinning，与 SK 解耦）"
```

---

## 任务 2：SmartDial 竞速度量改"整体链路耗时"（N2 + N5）

**文件：**
- 修改：`pkg/tunnel/mesh/smart.go`
- 修改：`pkg/tunnel/mesh/via_node.go`
- 修改：`pkg/tunnel/mesh/via_node_test.go`
- 修改：`pkg/tunnel/mesh/smart_test.go`
- 修改：`pkg/tunnel/mesh/mesh.go`

### 任务 2.1：Latency 语义统一为"首字节可读/数据面就绪"（N2）

- [ ] **步骤 1：编写失败测试**：`smart_test.go` 新增 `TestDialSmart_LatencyIsWholePathTime`——直连 10ms + 多跳 200ms，
  断言胜者 Latency = 真实链路就绪时刻（非各段加法）。当前实现 Latency 为各段 `time.Since(start)` 加法 → FAIL。
- [ ] **步骤 2：运行确认失败**
- [ ] **步骤 3：实现**：via-relay 用 `RelayStreamWithHeaders` 200 响应时点（I27 已含"数据面就绪"语义）
  记 `Latency = time.Since(start)`；via-direct 用任务 2.2 的独立回帧时点；direct 用打洞完成时点。
- [ ] **步骤 4：运行验证通过**（含 `-race`）
- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mesh/smart.go pkg/tunnel/mesh/via_node.go pkg/tunnel/mesh/smart_test.go pkg/tunnel/mesh/via_node_test.go
git commit -m "fix(mesh): 竞速 Latency 统一为整体链路就绪耗时（首字节可读，非各段加法）"
```

### 任务 2.2：via-direct 出口结果独立回帧通道（N2 的 via-direct 侧）

- [ ] **步骤 1：失败测试**：`via_node_test.go` 新增——via-direct 候选在 X 出口拨号完成后才计入竞速
  （当前 webrtc 直连打洞完成即返回，未含 X→T 拨号时间）→ 用 mock 信令器模拟，断言 Latency 含出口拨号耗时 → FAIL。
- [ ] **步骤 2：运行确认失败**
- [ ] **步骤 3：实现**：mux 流首帧加"拨号结果"语义（**独立回帧通道**，非数据面内联帧，不污染数据流）：
  X 侧 `relay.Serve` 出口拨号完成后回 `[4B len][{"dial_result":"ok"}]`（复用 `DialResultFrame` 结构），
  L 侧 via-direct 拨号读完该帧才返回 Result（`Latency` 含出口拨号）。
  **注意**：与现有 `DialResultFrames=false`（webrtc 直连）的约束区分——本回帧走**独立的专用流首帧**
  （拨号结果帧前置于数据面），不污染数据流。
- [ ] **步骤 4：运行验证通过**
- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mesh/via_node.go pkg/tunnel/mesh/via_node_test.go
git commit -m "feat(mesh): via-direct 出口结果独立回帧（竞速含 X→T 拨号耗时）"
```

### 任务 2.3：多跳候选竞速窗口加权（N5）

- [ ] **步骤 1：失败测试**：`smart_test.go`——多跳候选（via-node）需要比单跳候选更长的竞速窗口
  （当前统一 5s RaceWindow，慢但最终更快的多跳被提前放弃）→ 注入多跳 6s + 直连 5s 失败，断言多跳胜出 → FAIL。
- [ ] **步骤 2：运行确认失败**
- [ ] **步骤 3：实现**：`SmartOptions` 加 `MultihopRaceExtend`（默认 2×RaceWindow 或可配）；
  竞速窗口按候选是否多跳（via-node/via-direct）动态延长；多跳候选在基础窗口内未胜出时**不立即放弃**，
  等待延长窗口（其余单跳候选已关闭）。
- [ ] **步骤 4：运行验证通过**
- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mesh/smart.go pkg/tunnel/mesh/smart_test.go
git commit -m "feat(mesh): 多跳候选竞速窗口加权（避免短路径系统性偏袒）"
```

---

## 任务 3：`--smart` 默认关闭 + 优雅降级（用户确认保持默认 false）

**文件：**
- 修改：`cmd/sclient/mesh.go`、`cmd/sclient/socks.go`（flag 文档 + 降级路径）

- [ ] **步骤 1：确认现状**：`--smart` 默认 false（`mesh.go:286`）已符合；补"优雅降级"：
  当 hub 列表无可用 X 且直连/中继候选均失败时，回退原固定顺序 `mesh.Dial`（而非报"无可选路径"）。
- [ ] **步骤 2：失败测试**：`cmd/sclient/mesh_test.go` 或 mesh 包——无 X 候选 + 竞速全失败 → 回退固定顺序
  （现有行为返回错误）→ FAIL。
- [ ] **步骤 3：实现**：`DialSmart` 全候选失败时若提供了 fallback 参数（`FallbackDial`），走固定顺序重试。
- [ ] **步骤 4：运行验证通过**
- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/mesh.go cmd/sclient/socks.go pkg/tunnel/mesh/smart.go
git commit -m "feat(mesh): --smart 优雅降级（竞速全失败回退固定顺序）"
```

---

## 任务 4：mDNS 无 hub 场景信任增强（N3）

**文件：**
- 修改：`pkg/tunnel/mesh/mdns_node.go`（身份广播 + 接受侧 pinning）
- 修改：`pkg/tunnel/mesh/node.go`（NodeConfig 身份字段）

- [ ] **步骤 1：失败测试**：`mdns_node_test.go`——配置了 `--mdns-secret` + 身份时，接受侧校验对端指纹
  （现无指纹校验 → 任意节点可连入）→ FAIL。
- [ ] **步骤 2：运行确认失败**
- [ ] **步骤 3：实现**：mDNS TXT 记录携带节点身份指纹；接受侧 `WithPeerFingerprints`（从 mDNS 发现的对端
  指纹构建 pin 列表）；`--mdns-secret` 认证保留（双层：共享密钥 + 指纹）。
- [ ] **步骤 4：运行验证通过**
- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mesh/mdns_node.go pkg/tunnel/mesh/node.go
git commit -m "feat(mesh): mDNS 场景身份广播 + 指纹 pinning（共享密钥 + 指纹双层）"
```

---

## 任务 5：`--trust-x` 中间节点白名单（N4，信任收敛）

**文件：**
- 修改：`cmd/sclient/mesh_node.go`（flag + 装配）
- 修改：`pkg/tunnel/mesh/via_node.go`（Expand 过滤）
- 修改：`pkg/tunnel/mesh/via_node_test.go`

- [ ] **步骤 1：失败测试**：`via_node_test.go`——设置 `--trust-x X1` 后，Expand 只生成 X1 候选
  （现忽略该设置 → X2 也在候选）→ FAIL。
- [ ] **步骤 2：运行确认失败**
- [ ] **步骤 3：实现**：`viaNodeProvider.Expand` 加 `TrustedNodes` 过滤（空 = 全部可信，兼容现状）；
  `mesh_node.go` 加 `--trust-x`（可重复 flag，逗号分隔）。
- [ ] **步骤 4：运行验证通过**
- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/mesh_node.go pkg/tunnel/mesh/via_node.go pkg/tunnel/mesh/via_node_test.go
git commit -m "feat(mesh): --trust-x 中间节点白名单（信任收敛，减少攻击面）"
```

---

## 任务 6：出口拨号审计日志（N6）

**文件：**
- 修改：`pkg/tunnel/relay/leaf.go`（出口拨号审计）
- 修改：`cmd/sclient/mesh.go` / `socks.go`（接线）

- [ ] **步骤 1：失败测试**：`leaf` 测试——出口拨号时记录结构化审计日志（谁拨了哪个地址、经哪条路径）；
  用测试 logger 捕获断言 → 现无此日志 → FAIL。
- [ ] **步骤 2：运行确认失败**
- [ ] **步骤 3：实现**：`leaf.go` dOK 分支拨号成功/失败处加结构化 `logger.Info/Warn`（含 `remote` 对端地址、
  `addr`、`dial` 解析地址、路径类型 via-relay/via-direct、调用方 node-id——后者经 mux 流上下文取）。
- [ ] **步骤 4：运行验证通过**
- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/relay/leaf.go cmd/sclient/mesh.go cmd/sclient/socks.go
git commit -m "feat(mesh): 出口拨号审计日志（谁拨哪个地址、经哪条路径）"
```

---

## 任务 7：竞速结果可见性（N7，P0）

**文件：**
- 修改：`pkg/server/metrics.go`（竞速结果路径维度）
- 修改：`cmd/sclient/mesh.go`（`mesh connect -v` / `mesh status` 展示 Kind+Latency）

- [ ] **步骤 1：失败测试**：`metrics` 测试——`RecordMeshDial` 新增候选路径维度（via-relay/via-direct 可区分）
  → 现无此维度 → FAIL；`cmd/sclient` 测试——`mesh connect -v` 输出含 Kind+Latency → FAIL。
- [ ] **步骤 2：运行确认失败**
- [ ] **步骤 3：实现**：metrics label 扩展（compat：carrier 保留，新增 path 维度）；CLI 展示 Result.Kind/Latency。
- [ ] **步骤 4：运行验证通过**
- [ ] **步骤 5：Commit**

```bash
git add pkg/server/metrics.go pkg/server/metrics_test.go cmd/sclient/mesh.go
git commit -m "feat(mesh): 竞速结果可见性（Kind+Latency 展示 + metrics 路径维度）"
```

---

## 文档收敛（合并前，docs-lifecycle §2/§3 硬性）

- [ ] **步骤 1**：`docs/cli.md` 补 `--smart`（默认 false + 优雅降级）、`--smart-ttl`、`--trust-x`、
  `--mdns-secret`+身份双层说明、`-v` 展示 Kind+Latency。
- [ ] **步骤 2**：`docs/config.md` 补端到端加密启用方式（身份 + 指纹 pinning 配置键）。
- [ ] **步骤 3**：`docs/tunnel.md` 补"端到端加密与 SK 解耦"协议语义（ECDH + 指纹 pinning，X 只透传密文）。
- [ ] **步骤 4**：`docs/mesh-testing.md` 补新增测试用例说明。
- [ ] **步骤 5**：`docs/archive/mesh-evolution.md` 补 §6.7 端到端加密 + §6.8 竞速度量演进（精华归档）。
- [ ] **步骤 6**：审核清单（docs-lifecycle §3）：无 `docs/superpowers/plans|specs|designs` 断链引用；
  新 flag/配置已入权威文档；无"已移除功能"残留。
- [ ] **步骤 7：Commit**

```bash
git add docs/cli.md docs/config.md docs/tunnel.md docs/mesh-testing.md docs/archive/mesh-evolution.md
git commit -m "docs(mesh): 端到端加密与竞速度量收敛进权威文档 + 归档精华"
```

---

## 收尾验证（合并前，operating-rules §交付自检）

- [ ] `gofmt -l` 与 `goimports -l` 均无输出
- [ ] `go build ./...` + `make build-all`
- [ ] `make lint` + `make lint-all` 0 issues
- [ ] `go test ./pkg/... ./internal/...`（mesh 相关包全绿）
- [ ] `make test-all`（含子 module）
- [ ] `-race` 通过；`make deadcode-check`；`go test ./internal/archcheck/`（R14/R18 门禁）
- [ ] `make check-ci`（70% 覆盖率门禁）
- [ ] 文档审核清单（上节）
- [ ] 提交身份 suixibing + 无 Co-authored-by 检查

---

## 自检（writing-plans §自检）

**1. 规格覆盖度**：N1→T1、N2→T2、N3→T4、N4→T5、N5→T2.3、N6→T6、N7→T7；
N8/N9 明确排下片（登记非本片，符合"多阶段允许跨 PR，每片合并前收敛"）。无遗漏。

**2. 占位符扫描**：无"待定/TODO/后续实现"等占位；每个任务有具体文件 + 失败测试 + 实现要点 + 验证命令 + commit。

**3. 类型一致性**：`EndToEndOptions`（T1）→ `DialE2E`/`ServeE2E`（T1）→ `SmartOptions.MultihopRaceExtend`（T2.3）→
`viaNodeProvider.TrustedNodes`（T5）→ `RecordMeshDial` path label（T7），前后一致。
