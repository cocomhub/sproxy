# Kademlia DHT 节点发现 — 实现计划

## 概述

在 `pkg/tunnel/xfer/ext/kad/` 下创建独立的 Kademlia DHT 实现，作为 `hub.DHT` 接口的插件。

## 架构

- **独立 go.mod**：`github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/kad`
- **插件注册**：`init()` 中调用 `hub.DHTRegistry.Register()` 自动注册
- **依赖**：仅标准库（`crypto/sha256`、`sort`、`sync`、`time`、`math/bits`）
- **无第三方依赖**：Kademlia 核心算法纯标准库实现

## 核心数据结构

```go
const (
    keyBits    = 256  // SHA-256 节点 ID 位数
    bucketSize = 20   // 每个 k-bucket 最大节点数
)

type NodeID [32]byte  // SHA-256 哈希

type kadNode struct {
    info    hub.PeerInfo
    lastSeen time.Time
}

type Bucket struct {
    mu    sync.Mutex
    nodes []*kadNode  // 最多 bucketSize 个，按最近活跃排序
}

type Kademlia struct {
    id       NodeID
    buckets  [keyBits]*Bucket
    transport func(ctx context.Context, target string) (hub.PeerInfo, error)
    logger   *slog.Logger
}
```

## Kademlia 核心算法

### XOR 距离
```go
func xorDistance(a, b NodeID) NodeID { ... }
func prefixLen(id NodeID) int { ... }  // 计算共同前缀长度 → bucket 索引
```

### k-bucket 更新
- 收到节点信息时，计算 XOR 距离 → 确定 bucket 索引
- bucket 未满 → 追加到末尾
- bucket 已满且队首节点在线 → 丢弃新节点
- bucket 已满且队首节点离线 → 替换队首

### 迭代 FindNode
- 从最近的 bucket 中选择 α=3 个节点
- 并发查询，获取更近的节点
- 重复直到无法找到更近的节点或达到 k 个

## 文件清单

| 文件 | 说明 |
|---|---|
| `go.mod` | 独立 module，replace sproxy |
| `kad.go` | Kademlia 核心实现：路由表、ID、距离、bucket 更新 |
| `kad_lookup.go` | 迭代 FindNode 查找算法 |
| `kad_dht.go` | DHT 接口实现（Register/Lookup/GetClosestNodes/Bootstrap） |
| `kad_test.go` | 测试 |