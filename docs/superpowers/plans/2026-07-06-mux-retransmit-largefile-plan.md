# mux 重传机制与大文件传输优化 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 将 mux 层 `sendFrame()` 的同步阻塞重试改为异步重传队列，并在加密模式 streamBody 中添加预读缓冲。

**架构：** 两个独立阶段：(1) 异步重传队列 — 不修改帧格式，失败帧入队后由 writeLoop 在空闲时扫描重试；(2) 加密模式预读缓冲 — 复用已有的 64KB 预读缓冲逻辑，在加密分支中先填充缓冲区再从缓冲区读取。

**技术栈：** Go 标准库（sync、time、io），`pkg/tunnel/mux` 包，`pkg/tunnel/stream.go`

---

## 文件结构

### 阶段一：异步重传队列

| 文件 | 操作 | 说明 |
|---|---|---|
| `pkg/tunnel/mux/mux.go` | 修改 | 新增 retransmitEntry 类型、Mux 字段、sendFrame 改为非阻塞、writeLoop 扫描、scanRetransmitQ、enqueueRetransmit 方法 |
| `pkg/tunnel/mux/mux_test.go` | 修改 | 新增重传测试 |

### 阶段二：加密模式预读缓冲

| 文件 | 操作 | 说明 |
|---|---|---|
| `pkg/tunnel/stream.go` | 修改 | streamBody.Read 加密分支添加预读缓冲 |

---

## 阶段一：异步重传队列

### 任务 1.1：编写异步重传失败的测试

**文件：**
- 修改：`pkg/tunnel/mux/mux_test.go`

- [ ] **步骤 1：阅读现有测试模式**

运行：`cd D:/workdir/leon/cocomhub/sproxy && go test -run "TestMux" -count=1 -v ./pkg/tunnel/mux/... 2>&1 | head -20`
预期：现有测试通过，了解 `newPipePair`、`newMuxPair` 等辅助函数的使用方式

关键辅助函数（位于 `edge_test.go`）：
```go
func newPipePair(t *testing.T) (client, server xfer.Conn)
func newMuxPair(t *testing.T) (dialer, listener *mux.Mux)
```

- [ ] **步骤 2：编写重传耗尽测试**

在 `mux_test.go` 末尾添加：

```go
// TestRetransmitQueue_Exhausted 验证重试耗尽后 mux 关闭。
// 使用一个会持续失败的 conn.Send 来触发重传队列耗尽。
func TestRetransmitQueue_Exhausted(t *testing.T) {
    // 使用 pipe pair 创建 mux pair
    c, s := xfertest.Pipe()
    dm := mux.New(c, mux.RoleDialer)
    lm := mux.New(s, mux.RoleListener)
    t.Cleanup(func() { dm.Close(); lm.Close() })

    // 关闭底层连接，使 conn.Send 失败
    c.Close()
    s.Close()

    // 尝试写入数据，触发重传
    ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
    defer cancel()

    stream, err := lm.Open(ctx)
    if err != nil {
        // 可能在 Open 阶段就失败，这是合理的
        t.Logf("Open failed (expected): %v", err)
        return
    }

    _, err = stream.Write([]byte("test data"))
    if err != nil {
        t.Logf("Write failed (expected): %v", err)
    }

    // 等待 mux 关闭（重传耗尽后应自动关闭）
    <-dm.Done()
    <-lm.Done()
}

// TestRetransmitQueue_WriteAfterClose 验证 mux 关闭后写入返回错误。
func TestRetransmitQueue_WriteAfterClose(t *testing.T) {
    c, s := xfertest.Pipe()
    dm := mux.New(c, mux.RoleDialer)
    lm := mux.New(s, mux.RoleListener)
    dm.Close()
    lm.Close()

    _, err := dm.Open(t.Context())
    if err == nil {
        t.Fatal("expected error when opening stream on closed mux")
    }
}
```

- [ ] **步骤 3：运行测试验证失败**

运行：`cd D:/workdir/leon/cocomhub/sproxy && go test -run "TestRetransmitQueue_Exhausted|TestRetransmitQueue_WriteAfterClose" -count=1 -v ./pkg/tunnel/mux/... 2>&1`
预期：测试失败（函数未定义或编译错误，如 `dm.Done()` 不存在）

### 任务 1.2：实现异步重传队列

**文件：**
- 修改：`pkg/tunnel/mux/mux.go`

- [ ] **步骤 1：新增 retransmitEntry 类型和常量**

在 `mux.go` 的常量区域（约第 23-30 行）修改：

```go
const (
    maxRetries     = 5
    retryBaseDelay = 100 * time.Millisecond
    retryMaxDelay  = 3 * time.Second
)
```

在 `sendFrame` 函数之前（约第 449 行）添加重传类型和 Mux 字段：

```go
type retransmitEntry struct {
    frame    []byte
    retries  int
    deadline time.Time
}
```

在 `Mux` 结构体（约第 271 行）新增字段：

```go
type Mux struct {
    // ... 现有字段（conn, role, logger, metrics, mu, streams, nextID, acceptCh, writeCh, done, activeStreams, maxStreams, lastPongNano, ctxOnce, ctx, ctxCancel）...

    retransmitMu  sync.Mutex
    retransmitQ   []retransmitEntry
}
```

- [ ] **步骤 2：修改 `sendFrame` 为失败即入队**

找到 `sendFrame` 方法（约第 449 行），将数据帧的同步重试改为入队逻辑：

```go
func (m *Mux) sendFrame(msg writeMsg) {
    m.metrics.FramesSent.Add(1)
    if msg.isRaw {
        if err := m.conn.Send(m.Context(), msg.data); err != nil {
            m.metrics.Errors.Add(1)
            m.logger.Error("mux: send error", "err", err)
            m.Close()
        }
        return
    }

    var frame []byte
    switch {
    case msg.data == nil:
        frame = EncodeFrame(msg.streamID, FrameCloseWrite, nil)
    case len(msg.data) == 0:
        frame = EncodeFrame(msg.streamID, FrameClose, nil)
    default:
        frame = EncodeFrame(msg.streamID, FrameData, msg.data)
    }

    if len(msg.data) > 0 {
        // 数据帧：尝试发送，失败时入重传队列
        if err := m.conn.Send(m.Context(), frame); err != nil {
            m.enqueueRetransmit(frame, 0)
            return
        }
        return
    }

    // 控制帧（CloseWrite/Close）：不重传，失败直接关闭
    if err := m.conn.Send(m.Context(), frame); err != nil {
        m.metrics.Errors.Add(1)
        m.logger.Error("mux: send error, closing mux", "stream", msg.streamID, "err", err)
        m.Close()
        return
    }
    if len(msg.data) == 0 && msg.data != nil {
        m.removeStream(msg.streamID, true)
    }
}
```

- [ ] **步骤 3：添加 `enqueueRetransmit` 和 `scanRetransmitQ` 方法**

在 `sendFrame` 方法之后添加：

```go
// enqueueRetransmit 将失败帧加入重传队列。
func (m *Mux) enqueueRetransmit(frame []byte, retries int) {
    entry := retransmitEntry{
        frame:    frame,
        retries:  retries,
        deadline: time.Now().Add(retryBaseDelay),
    }
    m.retransmitMu.Lock()
    // 队列满时丢弃最早条目（保护内存）
    if len(m.retransmitQ) >= 256 {
        m.retransmitQ = m.retransmitQ[1:]
    }
    m.retransmitQ = append(m.retransmitQ, entry)
    m.retransmitMu.Unlock()
}

// scanRetransmitQ 扫描重传队列，重试到期的条目。
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
            continue // 成功，出队
        }
        entry.retries++
        if entry.retries >= maxRetries {
            m.metrics.Errors.Add(1)
            m.logger.Error("mux: retransmit exhausted", "retries", entry.retries)
            go m.Close()
            return
        }
        entry.deadline = now.Add(backoffDuration(entry.retries))
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

- [ ] **步骤 4：修改 `writeLoop` 末尾扫描重传队列**

找到 `writeLoop` 方法（约第 494 行），在 select 中添加 `time.After` 分支，并在循环末尾调用 `scanRetransmitQ`：

```go
func (m *Mux) writeLoop() {
    for {
        select {
        case <-m.done:
            return
        case msg := <-m.writeCh:
            m.sendFrame(msg)
        case <-time.After(50 * time.Millisecond):
            // 定时醒来，扫描重传队列
        }
        m.scanRetransmitQ()
    }
}
```

- [ ] **步骤 5：添加 `Done()` 方法（供测试用）**

在 `Mux` 结构体上添加方法：

```go
// Done 返回一个 channel，当 mux 关闭时关闭（用于测试）。
func (m *Mux) Done() <-chan struct{} {
    return m.done
}
```

- [ ] **步骤 6：运行测试验证通过**

运行：`cd D:/workdir/leon/cocomhub/sproxy && go test -run "TestRetransmitQueue_Exhausted|TestRetransmitQueue_WriteAfterClose" -count=1 -v ./pkg/tunnel/mux/... 2>&1`
预期：PASS

- [ ] **步骤 7：运行全部 mux 测试检查回归**

运行：`cd D:/workdir/leon/cocomhub/sproxy && go test -count=1 -race ./pkg/tunnel/mux/... 2>&1`
预期：全部 PASS，无 race

- [ ] **步骤 8：Commit**

```bash
cd D:/workdir/leon/cocomhub/sproxy
git add pkg/tunnel/mux/mux.go pkg/tunnel/mux/mux_test.go
git commit -m "feat(mux): async retransmit queue instead of blocking retries"
```

---

## 阶段二：加密模式预读缓冲

### 任务 2.1：编写加密模式预读缓冲测试

**文件：**
- 修改：`pkg/tunnel/stream_test.go`（可能不存在，需确认）

- [ ] **步骤 1：确认 streamBody 测试文件位置**

运行：`cd D:/workdir/leon/cocomhub/sproxy && ls pkg/tunnel/*_test.go 2>/dev/null`
预期：列出测试文件，确认 `stream_test.go` 是否存在

- [ ] **步骤 2：编写加密模式预读缓冲测试**

在 `pkg/tunnel/stream_test.go` 中添加（若文件不存在则创建）：

```go
package tunnel

import (
    "bytes"
    "crypto/rand"
    "io"
    "testing"
)

func TestStreamBodyEncryptedReadBuffer(t *testing.T) {
    key := make([]byte, 32)
    rand.Read(key)

    // 构造 1MB 测试数据
    dataSize := 1024 * 1024 // 1MB
    data := make([]byte, dataSize)
    rand.Read(data)

    // 创建 Pipe pair 模拟 mux 流
    pr, pw := io.Pipe()
    done := make(chan struct{})

    // 写入 goroutine：加密写入
    go func() {
        defer close(done)
        defer pw.Close()
        _, err := EncryptStream(key, bytes.NewReader(data), pw)
        if err != nil {
            t.Errorf("encrypt stream: %v", err)
        }
    }()

    // 读取端：使用 streamBody 读取
    sb := &streamBody{
        stream: pr,
        key:    key,
    }
    defer sb.Close()

    var total int64
    buf := make([]byte, 4096) // 小缓冲区读取，测试预读
    for {
        n, err := sb.Read(buf)
        total += int64(n)
        if err == io.EOF {
            break
        }
        if err != nil {
            t.Fatalf("read: %v", err)
        }
    }

    <-done

    if total != int64(dataSize) {
        t.Fatalf("expected %d bytes, got %d", dataSize, total)
    }
}

func TestStreamBodyEncryptedReadBuffer_SmallData(t *testing.T) {
    key := make([]byte, 32)
    rand.Read(key)

    data := []byte("small data")
    pr, pw := io.Pipe()
    done := make(chan struct{})

    go func() {
        defer close(done)
        defer pw.Close()
        _, err := EncryptStream(key, bytes.NewReader(data), pw)
        if err != nil {
            t.Errorf("encrypt stream: %v", err)
        }
    }()

    sb := &streamBody{
        stream: pr,
        key:    key,
    }
    defer sb.Close()

    got, err := io.ReadAll(sb)
    if err != nil {
        t.Fatalf("read all: %v", err)
    }

    <-done

    if !bytes.Equal(got, data) {
        t.Fatalf("expected %q, got %q", data, got)
    }
}
```

- [ ] **步骤 3：运行测试验证失败**

运行：`cd D:/workdir/leon/cocomhub/sproxy && go test -run "TestStreamBodyEncryptedReadBuffer" -count=1 -v ./pkg/tunnel/... 2>&1`
预期：预期失败或编译错误（取决于当前实现是否已有预读）

### 任务 2.2：实现加密模式预读缓冲

**文件：**
- 修改：`pkg/tunnel/stream.go`

- [ ] **步骤 1：修改 `streamBody.Read` 加密分支**

找到 `streamBody.Read` 方法（约第 163 行），将加密分支改为使用预读缓冲：

```go
func (b *streamBody) Read(p []byte) (int, error) {
    if b.key != nil {
        // 加密模式：使用预读缓冲
        if len(b.rdBuf) == 0 || b.rdOff >= len(b.rdBuf) {
            b.rdBuf = make([]byte, streamBodyBufSize)
            // 懒初始化 Pipe + goroutine 解密（仅一次）
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

    // 非加密模式：预读缓冲（原逻辑不变）
    if b.rdOff >= len(b.rdBuf) {
        b.rdBuf = make([]byte, streamBodyBufSize)
        n, err := io.ReadAtLeast(b.stream, b.rdBuf, 1)
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
```

- [ ] **步骤 2：运行测试验证通过**

运行：`cd D:/workdir/leon/cocomhub/sproxy && go test -run "TestStreamBodyEncryptedReadBuffer" -count=1 -v ./pkg/tunnel/... 2>&1`
预期：PASS

- [ ] **步骤 3：运行全部 tunnel 测试检查回归**

运行：`cd D:/workdir/leon/cocomhub/sproxy && go test -count=1 -race ./pkg/tunnel/... 2>&1`
预期：全部 PASS，无 race

- [ ] **步骤 4：Commit**

```bash
cd D:/workdir/leon/cocomhub/sproxy
git add pkg/tunnel/stream.go
git add pkg/tunnel/stream_test.go
git commit -m "feat(tunnel): add read buffer for encrypted streamBody"
```

---

## 验证

所有阶段完成后，运行全量测试：

```bash
cd D:/workdir/leon/cocomhub/sproxy
go test -count=1 -race ./pkg/tunnel/... 2>&1
go test -count=1 -race ./pkg/server/... 2>&1
go build ./... 2>&1
```

预期：全部 PASS，无 race，无编译错误。