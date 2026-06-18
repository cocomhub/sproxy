# 测试工具化与覆盖率提升设计

> 日期: 2026-06-19
> 状态: 拟定

## 1. 目标

1. **测试工具化** — 将 `pkg/testutil` 扩充为跨 module 可导入的测试工具套件，提供 mockxfer、mockdht、mockserver 三个 mock 子包
2. **覆盖率攻坚** — 核心包覆盖率目标：xfer ≥85%、p2p ≥85%、mux ≥90%、server ≥85%
3. **技术债务清理** — 伴随覆盖率工作，修复已知的 4 项技术债务

## 2. 方案选择

**方案 B（工具先行）**：
1. 先扩充 `pkg/testutil/` → 新增 `mockxfer/`、`mockdht/`、`mockserver/` 三个子包
2. 然后用统一工具批量补测试
3. 最后穿插修复技术债务

## 3. 测试工具设计

### 3.1 mockxfer — 传输层模拟

位置：`pkg/testutil/mockxfer/`

```go
// MockConn 实现 xfer.Conn，可控 Send/Receive 返回
type MockConn struct {
    SendFn    func(ctx context.Context, msg []byte) error
    ReceiveFn func(ctx context.Context) ([]byte, error)
    CloseFn   func() error
    // 调用记录
    SendCalls    int
    ReceiveCalls int
}

// MockListener 实现 xfer.Listener，可控 Accept 返回
type MockListener struct {
    AcceptFn func(ctx context.Context) (xfer.Conn, error)
    CloseFn  func() error
    addr     string
}
```

### 3.2 mockdht — DHT 模拟

位置：`pkg/testutil/mockdht/`

```go
// MockDHT 实现 hub.DHT 接口，可控 Lookup/Register 返回
type MockDHT struct {
    LookupFn   func(ctx context.Context, id string) (hub.PeerInfo, error)
    RegisterFn func(ctx context.Context, info hub.PeerInfo) error
    // 调用记录
    LookupCalls   int
    RegisterCalls int
}
```

### 3.3 mockserver — 存储层模拟

位置：`pkg/testutil/mockserver/`

```go
// MockChecksumStore 实现 server.ChecksumStore 接口，内存 map
type MockChecksumStore struct {
    data        map[string]string  // key → checksum
    SetError    error              // 可注入 Set 错误
    GetError    error              // 可注入 Get 错误
    DeleteError error              // 可注入 Delete 错误
}

// MockUploadStore 实现 server.UploadStore 接口，内存 map
type MockUploadStore struct {
    data        map[string][]byte  // filename → content
    SaveError   error              // 可注入 Save 错误
    OpenError   error              // 可注入 Open 错误
    DeleteError error              // 可注入 Delete 错误
}
```

### 3.4 与现有 xfertest 的关系

| 套件 | 定位 | 用途 |
|------|------|------|
| `xfertest.ConnSuite` | 集成测试 | 验证真实 Pipe/传输层的通路正确性 |
| `mockxfer.MockConn` | 单元测试 mock | 验证上层（p2p/mux）在传输层出错时的行为 |

互补不重叠。

## 4. 覆盖攻坚计划

### 4.1 pkg/tunnel/xfer (66.7% → 85%)

| 测试 | 覆盖分支 |
|------|----------|
| `Get("")` 返回 nil | registry.go 空名字边界 |
| 重复 Register 同名 transport（覆盖/panic） | registry.go 优先级/覆盖逻辑 |
| ConnSendAfterClose | core.go 关闭后 Send |
| ConnReceiveAfterClose | core.go 关闭后 Receive |
| TransportNotFound | xfer.Get("nonexistent") |

### 4.2 pkg/tunnel/p2p (71.7% → 85%)

| 测试 | 覆盖分支 |
|------|----------|
| Dial_TransportNotFound | webrtc 未注册 |
| Dial_LookupError | DHT.Lookup 返回 error |
| Dial_TransportDialError | transport.Dial 返回 error |
| Listen_TransportListenError | transport.Listen 返回 error |
| Accept_ContextCancelled | ctx cancel 后 Accept 返回 ctx.Err() |
| Accept_AfterClose | Close 后 Accept 返回 "node closed" |
| Close_WithoutListen | Close 时 listener==nil |
| DoubleClose | 幂等性 |

### 4.3 pkg/tunnel/mux (79.9% → 90%)

| 测试 | 覆盖分支 |
|------|----------|
| Accept_RejectWhenFull | acceptCh 满时发 FrameReject |
| FlowControl_BlockOnFullWindow | window 耗尽时 Send 阻塞 |
| StreamClose_SendAfterClose | 关闭后 Write 返回 error |
| Accept_TimeoutViaContext | context 超时 Accept 返回 |
| Mux_TransportError | MockConn.Send error → mux 行为 |

### 4.4 pkg/server (71.1% → 85%)

| 测试 | 覆盖分支 |
|------|----------|
| MkDir/MkDir_NoName/MkDir_PathTraversal | mkdir handler |
| RmDir/RmDir_NotFound/RmDir_NotDir | rmdir handler |
| Stat_NoName/Stat_PathTraversal/Stat_NotFound/Stat_IsDir | stat handler |
| Rename_SameFile/Rename_TargetExists/Rename_NotFound/Rename_NoChecksum/Rename_PathTraversal | rename handler |
| Upload_ChecksumMismatch | 文件存在但 checksum 不匹配 409 |
| Upload_RequestBodyTooLarge | 超 1GiB 请求体 413 |

## 5. 技术债务清理

| 编号 | 位置 | 问题 | 修复方式 |
|------|------|------|----------|
| TD1 | `cmd/sclient` 多个子命令 | `os.Exit(1)` → 测试不可覆盖 | 改为 `RunE` 返回 error，由 `rootCmd.Execute()` 统一退出 |
| TD2 | `cmd/sproxy/root.go` 信号 goroutine | `ListenAndServe` 失败时 goroutine 泄漏 | 在 `serve` 函数中加 `for range` 收到信号前不启动 goroutine，或 select 双 channel |
| TD3 | `test/e2e_test.go` | `findModuleRoot` 与 `runtime.Caller` 冗余 | 统一使用 `runtime.Caller` 方案，删除 `findModuleRoot` |
| TD4 | `integration_test.go` | 路由注册与 `RegisterRoutes` 不同步风险 | 改为调用 `RegisterRoutes` 而非手动重复路由 |

## 6. 非目标

- 不做 Fuzz 测试扩展（延迟到下一期）
- 不做文档/contributing 完善（延迟到下一期）
- 不引入第三方测试库（保持纯 stdlib 测试）
- 不改动生产代码的公共 API 签名

## 7. 执行顺序

```
Phase 0: 扩充 testutil（mockxfer + mockdht + mockserver）
Phase 1: xfer 覆盖（registry + core edges）
Phase 2: p2p 覆盖（mock-based 错误注入）
Phase 3: mux 覆盖（flow control + stream race）
Phase 4: server 覆盖（handler 边界 + store）
Phase 5: 技术债务清理（os.Exit + 信号泄漏 + 冗余代码 + 路由同步）
```
