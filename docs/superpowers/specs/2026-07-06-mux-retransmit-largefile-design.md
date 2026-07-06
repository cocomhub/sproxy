# mux 重传机制与大文件传输优化

> 状态：已确认 | 日期：2026-07-06 | 作者：suixibing

## 一、概述

### 1.1 背景与目标

当前 mux 层已实现基础帧协议和流控（窗口更新），但存在两个问题：

1. **重传阻塞 writeLoop**：`sendFrame()` 中的 `maxRetries=3` 重试是**同步阻塞**的，重试期间 writeLoop 无法处理其他流的写入，导致所有流延迟
2. **大文件传输 streamBody 预读不足**：加密模式下 streamBody 使用 `Pipe + goroutine` 串行解密，未使用预读缓冲，导致小读取多次 mux 内部消息传递

### 1.2 核心需求

1. **异步重传队列**：失败帧进入重传队列，writeLoop 在空闲时扫描重试，不阻塞其他流写入
2. **加密模式预读缓冲**：加密模式下 streamBody 也使用 64KB 预读缓冲，减少 mux 内部消息传递次数
3. **向后兼容**：不修改帧头格式，不引入新帧类型，不破坏现有协议

### 1.3 设计原则

- **简化**：不引入 ACK/序列号等复杂机制（当前无帧级别确认需求），仅解决 writeLoop 阻塞问题
- **增量**：最小改动，保持现有帧格式不变（FrameHeaderSize = 8，帧类型不变）

---

## 二、异步重传队列（2.2）

### 2.1 问题分析

当前 `sendFrame()` 逻辑：

```go
func (m *Mux) sendFrame(msg writeMsg) {
    // ...
    if len(msg.data) > 0 {
        for i := range maxRetries {      // ← 同步重试 3 次
            if err := m.conn.Send(...); err == nil {
                return
            }
            time.Sleep(retryBaseDelay << i)  // ← 阻塞 writeLoop
        }
        m.Close()  // 重试耗尽
    }
}
```

writeLoop 调用 `sendFrame` 是串行处理 writeCh 的，重试期间其他所有流的写入都阻塞在 writeCh 上。

### 2.2 改动

**不修改帧格式**，保持 `FrameHeaderSize = 8`，不引入新帧类型。

**Mux 结构体新增字段：**

```go
type Mux struct {
    // ... 现有字段 ...
    retransmitMu  sync.Mutex
    retransmitQ   []retransmitEntry
}

type retransmitEntry struct {
    frame    []byte
    retries  int
    deadline time.Time
}
```

**`sendFrame` 改为失败即入队：**

```go
func (m *Mux) sendFrame(msg writeMsg) {
    // ... 构建 frame 逻辑不变 ...
    if len(msg.data) > 0 {
        if err := m.conn.Send(m.Context(), frame); err != nil {
            // 入重传队列，不阻塞
            m.enqueueRetransmit(frame, 0)
            return
        }
        return
    }
    // 控制帧（CloseWrite/Close）不重传
    // ...
}
```

**writeLoop 末尾扫描重传队列：**

```go
func (m *Mux) writeLoop() {
    for {
        select {
        case <-m.done:
            return
        case msg := <-m.writeCh:
            m.sendFrame(msg)
        case <-time.After(50 * time.Millisecond):
            // 定时扫描重传队列（非高精度，仅用于退避到期重试）
        }
        m.scanRetransmitQ()  // 每次迭代后检查
    }
}
```

**`scanRetransmitQ` 方法：**

```go
func (m *Mux) scanRetransmitQ() {
    m.retransmitMu.Lock()
    defer m.retransmitMu.Unlock()
    
    now := time.Now()
    remaining := make([]retransmitEntry, 0, len(m.retransmitQ))
    
    for _, entry := range m.retransmitQ {
        if entry.deadline.After(now) {
            remaining = append(remaining, entry)
            continue
        }
        // deadline 已到，重试
        if err := m.conn.Send(m.Context(), entry.frame); err == nil {
            m.metrics.RecvRetries.Add(-1) // 重传成功不计为错误
            continue  // 成功，出队
        }
        entry.retries++
        if entry.retries >= maxRetries {
            m.metrics.Errors.Add(1)
            m.logger.Error("mux: retransmit exhausted", "retries", entry.retries)
            go m.Close()  // 异步关闭，避免持锁
            return
        }
        entry.deadline = time.Now().Add(backoffDuration(entry.retries))
        remaining = append(remaining, entry)
    }
    m.retransmitQ = remaining
}

func backoffDuration(retries int) time.Duration {
    d := retryBaseDelay << min(retries-1, 5)
    if d > retryMaxDelay {
        d = retryMaxDelay
    }
    return d
}
```

### 2.3 常量调整

```go
const (
    maxRetries     = 5                      // 从 3 增加到 5
    retryBaseDelay = 100 * time.Millisecond
    retryMaxDelay  = 3 * time.Second        // 新增：最大退避间隔
)
```

### 2.4 测试

- 单帧发送失败后入重传队列 → 重试成功出队
- 重试耗尽 → mux Close
- 并发写入 + 重传不互斥（-race 通过）
- 现有 mux 测试全部通过（格式未变）

---

## 三、加密模式预读缓冲（2.3）

### 3.1 streamBody 加密模式预读

当前加密模式：

```go
func (b *streamBody) Read(p []byte) (int, error) {
    if b.key != nil {
        // Pipe + goroutine 串行解密
        b.once.Do(func() {
            b.pr, b.pw = io.Pipe()
            go func() {
                _, err := DecryptStream(b.key, b.stream, b.pw)
                b.pw.CloseWithError(err)
            }()
        })
        return b.pr.Read(p)  // ← 每次 Read 可能只读几个字节，频繁 mux 消息传递
    }
    // 非加密模式：64KB 预读缓冲
    // ...
}
```

优化为加密模式也使用预读缓冲：

```go
func (b *streamBody) Read(p []byte) (int, error) {
    if b.key != nil {
        // 加密模式：使用预读缓冲
        if len(b.rdBuf) == 0 || b.rdOff >= len(b.rdBuf) {
            b.rdBuf = make([]byte, streamBodyBufSize)
            // 从解密流读取一个 buff 大小
            b.once.Do(func() {
                b.pr, b.pw = io.Pipe()
                go func() {
                    _, err := DecryptStream(b.key, b.stream, b.pw)
                    b.pw.CloseWithError(err)
                }()
            })
            n, err := b.pr.Read(b.rdBuf)
            if err != nil && err != io.EOF {
                return 0, err
            }
            b.rdBuf = b.rdBuf[:n]
            b.rdOff = 0
            if n == 0 {
                return 0, io.EOF
            }
        }
        n := copy(p, b.rdBuf[b.rdOff:])
        b.rdOff += n
        return n, nil
    }
    // 非加密模式保持不变
    // ...
}
```

### 3.2 测试

- 加密模式下 streamBody 预读缓冲读大文件（1MB+）正确性
- 预读缓冲不改变行为（短文件、空文件、EOF 场景）

---

## 四、影响范围

| 文件 | 改动 |
|---|---|
| `pkg/tunnel/mux/mux.go` | 重传队列、writeLoop 扫描、`sendFrame` 改为非阻塞 |
| `pkg/tunnel/mux/mux_test.go` | 重传相关测试 |
| `pkg/tunnel/stream.go` | streamBody 加密模式预读缓冲 |

**不包含：** 帧格式变更、新帧类型、并发流模型（这些属于后续优化，当前 YAGNI）。