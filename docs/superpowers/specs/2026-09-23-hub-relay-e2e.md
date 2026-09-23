<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# hub 中继 E2E 透传设计（2026-09-23）

- 状态：已批准（用户确认：接受主仓改动，场景为 mac→国内 relayd→新加坡 hub）
- 关联：stealth-builder 场景（公网中间节点 + 加密跳板）

## 问题

`mesh connect --e2e` 经 hub 中继（`POST /api/relay/stream`）时，数据面加密**静默失效**：

1. hub 中继写**普通 dial 帧**（`{dial, await_result, path:via-relay}`，无 E2E）给叶子
2. 叶子收到普通帧 → 出口拨号 → 进入泵送循环
3. 客户端 `DialE2EStream` 在 RelayStream 返回后写 e2e 帧 → 到达叶子时被**当远程数据泵送**
4. E2E 握手帧永远无法成为叶子的首帧 → 加密未生效（数据面明文）

**实测确认**（2026-09-23 本地三端）：mac mesh connect --e2e → hub 中继 → sg-t mesh node，sg-t 日志 `path=via-relay` 普通泵送（非 E2E 分支）。

## 方案：RelayStreamRequest 透传 E2E/Path

hub 中继请求体（`RelayStreamRequest`）携带 E2E 标记与 Path，hub 据此写对应的 dial 帧。

### 帧语义

| req.E2E | req.Path | hub 写帧 | 叶子行为 |
|---|---|---|---|
| false | （忽略） | `{dial, await_result, path:via-relay}` | 普通出口拨号（零回归） |
| true | 空 | `{dial, e2e:true}` | T 解密（直连 T：mac→新加坡） |
| true | `via-relay` | `{dial, e2e:true, path:via-relay}` | X 透传（多跳：mac→国内 X→新加坡 T） |

### 改动点

1. **pkg/client/relay.go**：`RelayStreamRequest` 加 `E2E bool` + `Path string`；新增
   `RelayStreamE2E(ctx, target, addr string, e2e bool, path string) (net.Conn, error)`
   （复用 RelayStreamWithHeaders 内部，序列化带 E2E/Path）
2. **pkg/server/relay_stream.go**：解码请求后：
   ```go
   frame := hub.DialRequest{Dial: req.Addr, AwaitResult: true, Path: "via-relay"}
   if req.E2E {
       frame.E2E = true
       frame.Path = req.Path // 空 = 直连 T
   }
   ```
3. **pkg/tunnel/mesh/mesh.go** 中继回落 E2E：`RelayStreamE2E(ctx, node, addr, true, "")`
4. **pkg/tunnel/mesh/via_node.go** via-relay E2E：`RelayStreamE2E(ctx, xID, addr, true, "via-relay")`
5. **pkg/tunnel/mesh/smart.go**：若 smart 候选经 RelayStream 且 E2E，同步适配

### 帧序解决

E2E 帧作为**首帧**（`e2e:true`）到达叶子 → leaf.go dOK 分支：
- Path 空 → **解密分支**（E2EServe）→ 出口拨号后解密再泵送（T 角色）
- Path=`via-relay` → **透传分支**（X 中间节点，纯字节泵，不见明文）

### 兼容性

- **旧叶子**（无 E2EServe）：收到 e2e 帧 → fail-closed 报错（leaf.go 已实现，不静默明文）
- **旧客户端**（无 E2E 字段）：请求体无 e2e → hub 写普通帧（零回归）
- **旧 hub**（不读 req.E2E）：写普通帧 → 客户端 E2E 静默失效——**接受**（hub 与客户端同仓同步发布；stealth 场景部署同版本）

### 验证

1. 单测：hub 中继 E2E 帧写对（e2e:true + path 组合）
2. 集成：mesh connect --e2e 经 hub 中继 → 叶子 E2EServe 解密 → 数据往返
3. 回归：无 E2E 普通帧（旧行为不变）
4. 多跳：via-relay:X E2E → X 透传 → T 解密（leaf_via_relay_e2e_test 扩展）
