<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 架构分析与协议选择

> 编写日期：2026-08-04
> 分析范围：tunnel_mux 核心价值、多协议适用场景、链路选择必要性、跨国/跨墙场景协议推荐

## 一、隧道层架构全景

### 1.1 两条隧道路径

```
传统隧道路径（tunnel.go）：
  sclient TunnelDo ──→ HTTP POST /tunnel ──→ NewHandler ──→ decrypt → 本地路由
  单向：客户端主动发起，服务端被动响应
  消费端：所有 sclient 子命令（factory.NewClient）、FileClient.WithTunnel
  测试覆盖：✅ 成熟

多路复用隧道路径（tunnel_mux.go + mux/）：
  sclient WithXfer ──→ xfer.Get("ws") ──→ WS Conn ──→ mux.Mux ──→ Tunnel.Do/Serve
  双向：两端均可主动发起请求，支持中继转发
  消费端：FileClient.WithXfer("ws")、sclient relay、relay handler（中继转发）
  测试覆盖：✅ 有测试但不如传统隧道成熟
```

### 1.2 消费链修正

**与初步分析不同的发现**：之前认为 tunnel_mux 和 mux 层 0 消费，实际追踪发现：

```
pkg/client/client.go:WithXfer("ws", hubURL, hexKey)
  └── getTunnelMux()
      └── xfer.Get("ws") → WS 拨号
      └── mux.New(conn, RoleDialer)
      └── tunnel.NewTunnel(m, key)
      └── Tunnel.Do(req)

cmd/sclient/relay.go
  └── xfer.Get("ws") → WS 连接 Hub
  └── mux.New(conn, RoleDialer)
  └── tunnel.NewTunnel(m, nil)  ← 无隧道密钥（中继节点不加密）
  └── tunnel.Serve(ctx, localHandler)  ← 接收中继请求

pkg/server/relay.go:relayHandler
  └── RouteTable.Lookup(targetID)
  └── mux.Open() → stream
  └── tunnel.NewTunnel(targetMux, nil)
  └── Tunnel.Do(req)  ← 转发请求到目标节点
```

**结论**：tunnel_mux 有 3 个真实消费者，不是无人消费。但消费者链路较窄（relay + WithXfer）。

## 二、tunnel_mux 的核心价值

### 2.1 它解决了什么问题

传统隧道（`POST /tunnel`）是 **请求-响应** 模式——客户端发 HTTP POST，服务端返回。这有一个根本限制：

```
传统隧道（单向）：
  sclient ──POST /tunnel──→ sproxy
  sclient ←──响应───────── sproxy
  sproxy **不能主动**向 sclient 发起请求

多路复用隧道（双向）：
  sclient ──mux 持久连接── sproxy (Hub)
  sclient → 发请求给 sproxy          ✅
  sproxy → 通过 mux 流转发请求给 sclient  ✅（中继）
```

**tunnel_mux 的核心价值 = 双向通信能力**。没有它，sproxy 无法实现中继转发。

### 2.2 relay 的真实场景

`cmd/sclient/relay.go` 让 sclient 作为中继节点连接到 Hub，等效于 **frp 的 frpc 模式**：

```
部署拓扑：

  [内网服务] ←── sclient (relay 模式) ──WS 持久连接── [公网 sproxy (Hub)]
                                                          │
  [远程用户] ──→ sclient upload ──→ Hub ──relay──→ sclient → 内网服务
```

场景：内网没有公网 IP，内网服务器不能直接被外网访问。通过 sclient relay 连接到公网 Hub，外网用户通过 Hub 中继访问内网服务。

**没有 tunnel_mux/mux，这个场景无法实现。**

### 2.3 精简建议

tunnel_mux + mux 是必要的，但可以精简：

| 保留 | 删除/标记实验性 |
|------|----------------|
| 帧协议（FrameOpen/Data/Close） | 重传机制（retransmit.go） |
| 流管理（stream.go） | mux 层独立心跳（TCP Keep-Alive 已够） |
| 事件循环（loop.go） | FrameReject 机制 |
| Tunnel.Do/Serve | ECDH PFS（当前无场景） |
| 传统隧道加密（AES-GCM） | 重放保护（当前无场景） |

## 三、多协议在不同场景的价值

### 3.1 各协议的适用场景矩阵

| 传输协议 | 适用场景 | sproxy 中此场景存在？ | 是否必要 |
|----------|----------|----------------------|----------|
| **TCP/HTTP POST** | 公网/内网直连，低延迟高吞吐 | ✅ 传统隧道核心 | ✅ **核心** |
| **WebSocket** | 浏览器通信、防火墙友好、NAT 穿透 | ✅ relay 节点连接 Hub | ✅ **核心** |
| **QUIC** | UDP 环境、WiFi 切换不中断、弱网优化、0-RTT | ❌ 项目无弱网优化需求 | ❌ 过度 |
| **gRPC** | 微服务双向流、多语言生态、Protobuf 序列化 | ❌ 项目无多语言服务 | ❌ 过度 |
| **WebRTC** | P2P 直连、NAT 穿透（无需中继）、浏览器端 | ⚠️ P2P 场景存在但无人构建 | ❌ 过度 |

### 3.2 协议选择的核心判断标准

**协议的价值 = 它解决的问题是否在你的场景中存在。**

sproxy 的真实场景：
1. **内网文件服务**（sproxy 在内网，sclient 在公网/另一内网）→ TCP/HTTP 就够了
2. **中继穿透**（sclient 通过 Hub 中继访问内网服务）→ WS 就够了
3. **P2P 直连**（两个 sproxy 直接传输，无需 Hub）→ 当前这个场景不存在

### 3.3 如果场景扩展

如果未来场景扩展为：

| 扩展场景 | 有价值协议 | 优先级 |
|----------|-----------|--------|
| 移动端 App 通过 sproxy 传输文件 | QUIC（WiFi ↔ 4G 切换不中断） | 中 |
| 多语言微服务之间使用 sproxy 隧道 | gRPC（多语言 stub 生成） | 低 |
| 浏览器之间 P2P 文件传输 | WebRTC（浏览器原生支持） | 低 |
| IoT 设备通过 sproxy 传输数据 | QUIC（0-RTT 握手 + UDP 省电） | 低 |

### 3.4 维护成本权衡

```
协议数量    维护成本    用户价值
───────── ────────── ──────────
TCP         低          高（核心）
TCP + WS   中          高（核心 + 穿透）
TCP + WS
+ QUIC     高          低（当前无场景）
+ gRPC     高          低（当前无场景）
+ WebRTC   高          低（当前无场景）
```

维护 5 种传输协议的成本：
- **构建**：每个 ext 有独立 go.mod，CI/构建需分别处理（make build-all）
- **测试**：8 个测试文件、1.5k+ 行测试代码
- **兼容性**：WS/QUIC/gRPC/WebRTC 第三方库版本升级需单独管理
- **文档**：各协议使用说明、配置示例

**建议当前阶段固定 TCP + WS 两种协议，其他标记实验性。**

## 四、客户端链路选择的必要性

### 4.1 当前链路选择机制

sproxy 已具备基本的链路选择能力：

```go
// 选项 1：直连（无加密）
NewFileClient("https://server:18083")

// 选项 2：传统隧道（加密 + 认证）
NewFileClient("https://server:18083", WithTunnel(hexKey))

// 选项 3：xfer 传输（多路复用隧道 + 中继）
NewFileClient("https://server:18083", WithXfer("ws", hubURL, hexKey))

// sclient CLI 通过 --xfer 参数
sclient --xfer ws://hub:18084/ws upload file.txt
```

**当前是静态选择**：编译/启动时决定，不能运行时动态切换。

### 4.2 链路选择场景分析

#### 场景 A：公网直连 → 隧道 fallback（✅ 高价值）

```
sclient → 尝试直连 sproxy（最快、无额外开销）
         → TCP 连接失败（NAT 阻挡）
         → 自动回退到经中继 Hub 的隧道
         → 穿透 NAT 成功
```

**用户价值**：用户不需要知道 sproxy 在 NAT 后面还是公网上。配置一个优先级列表，客户端自动尝试。

#### 场景 B：弱网环境自动降级（❌ 低价值）

```
sclient → TCP 隧道（低延迟）
         → TCP 重传超时 → 切换 QUIC（抗丢包）
```

**价值低的原因**：项目当前场景不涉及弱网优化。如果未来做移动端，才有价值。

#### 场景 C：大文件走直连、小请求走隧道（❌ 不实用）

```
sclient → 上传大文件（>100MB）→ TCP 直连（更高吞吐）
         → 上传小文件（<1MB）→ 隧道（更安全）
```

**不实用原因**：增加复杂度远大于收益。要么全走隧道要么全走直连，混合策略的业务价值不清晰。

#### 场景 D：声明式链路配置（✅ 中高价值）

```yaml
# sclient.yaml
tunnel:
  mode: xfer          # direct | classic | xfer
  transport: ws       # tcp | ws
  hub: ws://hub:18084/ws
  fallback:
    - mode: classic     # 优先传统隧道（直连）
      transport: tcp
    - mode: xfer        # fallback 到中继
      transport: ws
      hub: ws://hub:18084/ws
```

**用户价值**：声明式配置比 CLI flag 更方便，降低心智负担。YAML 配置已在其他场景使用（config.yaml），用户习惯一致。

### 4.3 链路选择推荐方案

| 阶段 | 方案 | 价值 |
|------|------|------|
| **立即做** | 直连 → 隧道 fallback（场景 A） | ✅ 用户可在代码或配置中指定链路优先级 |
| **短期内** | 声明式链路配置 YAML（场景 D） | ✅ 降低心智负担，一致性体验 |
| **暂缓** | 运行时健康检测 + 自动链路切换 | ⚠️ 有场景但不紧迫 |
| **不做** | 请求级动态路由（大文件走 TCP 小请求走隧道） | ❌ 复杂度过高，收益不清 |

## 五、跨国/跨墙场景协议分析

### 5.1 核心约束

跨墙场景中，协议的选择 **不是由技术指标决定的**，而是由 **流量特征的可识别性** 决定的。

GFW 的核心能力：
1. **DPI（深度包检测）**：识别协议指纹（TLS SNI、HTTP Host/Content-Type、WebSocket Upgrade）
2. **流量特征分析**：连接模式、包大小分布、时序特征
3. **主动探测**：对疑似代理的端点发起连接测试

所以关键不是"哪种协议加密更强"，而是 **"哪种协议的流量看起来最像正常网络流量"**。

### 5.2 各协议跨墙表现

| 协议 | GFW 识别难度 | 流量伪装度 | CDN 兼容 | 推荐度 |
|------|-------------|-----------|----------|--------|
| **TCP 隧道**（POST /tunnel） | ❌ 易识别（自定义 Content-Type + 固定帧长密文） | ❌ 低 | ✅ 好 | ⭐ |
| **WS over TLS（WSS）** | ✅ 难识别（大量 Web 应用使用 WS，流量混杂） | ✅ 高 | ✅ 非常好 | ⭐⭐⭐⭐ |
| **QUIC over UDP** | ⚠️ 中等（Google/Cloudflare 大量使用，但国内 UDP 限速） | ⚠️ 中 | ⚠️ 部分 | ⭐⭐⭐ |
| **gRPC** | ❌ 易识别（Content-Type: application/grpc + HTTP/2 指纹明显） | ❌ 低 | ✅ 好 | ⭐ |
| **WebRTC** | ✅ 难识别（UDP 流量与视频通话无法区分） | ✅ 高 | ❌ 不支持 | ⭐⭐⭐ |

### 5.3 详细分析

#### TCP 隧道（传统模式）— ❌ 不推荐

```
流量特征：HTTP POST，Content-Type: application/x-tunnel-frame
          body 为固定帧长的 AES-GCM 密文
```

- DPI 看到自定义 Content-Type + 二进制密文，特征明显
- 即使走 HTTPS，GFW 可根据连接模式（持续 POST）做主动探测

#### WS over TLS（WSS）— ✅ 推荐（当前最佳选择）

```
流量特征：标准 HTTP Upgrade: websocket
          后续 WS 帧在 TLS 隧道内
```

- 大量 Web 应用使用 WebSocket（聊天、协作、实时更新），流量混杂度高
- **通过 CDN 前置后**，WS 流量与正常 WebSocket 流量完全无法区分
- CDN（Cloudflare）前置时，SNI 显示 CDN 域名，非你的服务器 IP
- **关键：必须走 WSS（TLS），不能走明文 WS**

**推荐理由**：WS 是唯一有消费端、有人维护的 ext 传输。WS over TLS + CDN 是目前最成熟、维护成本最低的方案。

#### QUIC — ⚠️ 有条件推荐

```
流量特征：UDP + TLS 1.3，与 HTTP/3 的 QUIC 相同
```

- GFW 对 UDP 审查能力弱于 TCP
- Google/YouTube/Cloudflare 大量使用 QUIC，无法全面封锁
- **但问题**：中国运营商对 UDP 限速/丢包；QUIC 实现无人维护

#### gRPC — ❌ 不推荐

```
流量特征：HTTP/2 双向流，Content-Type: application/grpc
          Protobuf 序列化二进制
```

- HTTP/2 的 SETTINGS 帧和 gRPC 的 Content-Type 是强指纹
- 有针对性封锁先例
- HTTP/2 multiplexing 在跨墙场景下有害——一条连接被重置所有流中断

#### WebRTC — ⚠️ 有条件推荐

```
流量特征：UDP + DTLS + SRTP/SCTP，ICE 协商（STUN）
```

- 与视频通话流量相同，极难区分
- 大量国内应用使用 WebRTC，不可全面封锁
- **但问题**：需要信令服务器做 ICE 协商；国内 NAT 复杂，STUN 打洞成功率低；需要 TURN fallback

### 5.4 关键战术：CDN 前置比协议选择更重要

**跨墙场景中，CDN 前置比协议选择更重要。**

```
无 CDN 前置：
  客户端 ──→ 你的服务器 IP（直接暴露）
            GFW 看到不常见 IP + 非标准流量
            → 易被主动探测封锁

有 CDN 前置（推荐）：
  客户端 ──→ CDN 节点（Cloudflare IP）──→ 你的服务器
            GFW 看到正常 HTTPS 访问 Cloudflare
            → 极难封锁（封锁 Cloudflare IP 会误伤数百万网站）
```

**sproxy 的已有能力**：
- ✅ TLS + ACME 自动证书 → 可配置 CDN 回源使用 HTTPS
- ✅ WebSocket 传输 → CDN 支持 WS 回源（Cloudflare 已支持）
- ❌ **缺少 CDN 前置直接配置支持**（需手动配置 nginx/caddy 反向代理）

### 5.5 最佳跨墙配置

```
                  WSS                          HTTPS
  [远程用户] ──→ [Cloudflare] ←──TLS──→ [sproxy 服务端]
                  端口 443                    ACME 自动证书
                  无额外配置                  WebSocket 传输
```

**推荐配置**：
- 协议：WebSocket over TLS（WSS）
- 传输：ext/ws
- 端口：443（标准 HTTPS 端口）
- 路径：自定义（避免用 `/ws` 等明显路径）
- CDN：Cloudflare 或其他支持 WS 代理的 CDN
- 证书：sproxy ACME 自动证书

### 5.6 跨墙能力提升建议

| 优先级 | 措施 | 说明 |
|--------|------|------|
| **P0** | CDN 前置配置向导 | 自动生成反向代理配置示例（nginx/caddy/Cloudflare） |
| **P1** | 协议混淆增强 | WS 帧随机 padding、自定义 WS 路径 |
| **P1** | 连接复用 | 一条 WS 连接多路复用多个请求（减少连接建立特征） |
| **P2** | 连接保活优化 | 可配置 ping/pong 间隔，避免长连接被静默切断 |
| **不做** | 自实现协议混淆层 | 复杂度高，GFW 会跟进封锁→不如 WS + CDN |

### 5.7 不建议在跨墙场景做的

| 不建议 | 原因 |
|--------|------|
| 纯 TCP 隧道过墙 | DPI 特征明显，无流量伪装 |
| gRPC 过墙 | Content-Type + HTTP/2 指纹太明显 |
| 自实现协议混淆 | 工程复杂，GFW 跟进封锁的快于你迭代 |
| 仅依赖 QUIC 过墙 | 国内 UDP 限速，实现无人维护 |

## 六、最终架构建议

### 6.1 当前阶段推荐

| 功能 | 推荐 | 原因 |
|------|------|------|
| **TCP 隧道（传统模式）** | **固定核心** | 90% 场景足够，0 额外依赖 |
| **WebSocket 传输** | **固定配套** | NAT 穿透 + relay + 跨墙场景必须 |
| **tunnel_mux + mux 核心** | **保留但精简** | relay 场景必须，删除重传/冗余心跳 |
| **QUIC/gRPC/WebRTC/Kademlia** | **标记实验性** | 0 消费场景，维护成本高 |
| **自动链路选择** | **先做 fallback** | 直连 → 隧道 → 中继 fallback 有场景 |
| **声明式链路配置** | **增强配置能力** | YAML 配置降低心智负担 |

### 6.2 精简后架构

```
当前：
  xfer 层: TCP | WS | QUIC | gRPC | WebRTC
  mux 层: 完整（重传 + 心跳 + 拒绝）
  tunnel 层: 传统模式 + 多路复用模式
  ext/: 5 个子模块（4 个无人消费）

精简后：
  xfer 层: TCP（内置）+ WS（唯一 ext）
  mux 层: 保留帧协议 + 流管理，删除重传
  tunnel 层: 传统模式 + 多路复用模式（保留，relay 需要）
  experimental/: QUIC | gRPC | WebRTC | Kademlia | P2P
  链路选择: 直连 → 隧道(TCP) → relay(WS) 三级 fallback
```

### 6.3 核心原则

1. **TCP + WS 两种协议就够**，不需要 5 种
2. **自动链路选择有价值**，但只做"直连 → 隧道 → 中继"的 fallback
3. **跨墙用 WSS + CDN**，不需要引入 QUIC/gRPC/WebRTC
4. **双向通信能力是 tunnel_mux 的核心价值**，不是性能或加密

## 七、关联文档

- [sproxy 与开源方案对比分析](./2026-08-04-sproxy-vs-opensource-comparison.md) — 整体对比与场景评估
- [sproxy 过度设计分析](./2026-08-04-sproxy-overengineering-analysis.md) — 消费链追踪与量化总结
