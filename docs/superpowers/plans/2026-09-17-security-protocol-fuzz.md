# 安全加固：协议 fuzz 扩展 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 扩展 sproxy 协议面的 fuzz 测试，覆盖 mux 帧解析与 TCP 传输层帧定界（`#301` 短 WindowUpdate 帧崩溃类问题的回归防线），以及 tunnel 帧元数据解析。

**架构：**
- 现有 fuzz：`pkg/pathguard/validate_fuzz_test.go`、`pkg/client/calcchunksize_fuzz_test.go`、`pkg/tunnel/parsekey_fuzz_test.go`（FuzzXxx 模式 + seed corpus）。
- 本片新增：① `pkg/tunnel/mux/frame_fuzz_test.go`（`FuzzDecodeFrame`——对 `DecodeFrame` 输入任意字节，断言永不 panic、错误可预期）；② `pkg/tunnel/xfer/internal/tcp/tcp_fuzz_test.go`（长度前缀帧定界——输入任意字节流，断言解析不 panic、长度上界受控）；③ `pkg/tunnel/frame_fuzz_test.go`（统一帧协议元数据头——`[4B metaLen][metadata][chunks]`，输入任意字节断言不 panic）。
- 三个 fuzz 各自带 seed corpus（从现有测试合法帧 + 边界形状抽取），本地 `go test -fuzz=FuzzX -fuzztime=5s` 跑通，不做长时 fuzz（CI 不跑长 fuzz，防超时——Benchmark job 已多次因长任务超时，fuzz 同理）。

**技术栈：** Go 1.27 标准库 testing/fuzz，无新依赖。

**规格：** `docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md` §3-E「安全加固」协议 fuzz 扩展；教训来源：`#301`（远端可触发进程崩溃的短 WindowUpdate 帧）。

## 全局约束

- UTF-8 without BOM；SPDX 头；测试纯标准库；只绑 `127.0.0.1`；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；fuzz 目标函数不得依赖墙钟。
- 错误 `fmt.Errorf("...: %w", err)`；日志 `log/slog`。
- Conventional Commits：`test(mux): <描述>` / `test(tunnel): <描述>`；禁署名行。
- **变异验证硬规则**：每个 fuzz 目标写完先做「种子输入在退化实现（如去掉长度上界检查）下能触发」的验证，防假绿。
- Go 1.27 语法。

---

### 任务 1：mux 帧解析 fuzz

**文件：**
- 创建：`pkg/tunnel/mux/frame_fuzz_test.go`
- 参考：`pkg/tunnel/mux/frame.go`（`EncodeFrame`/`DecodeFrame`，:74/:92）、`frame_test.go`（现有合法帧测试）

**目标：** `FuzzDecodeFrame` 任意输入不 panic、返回错误或可预期结果。

- [ ] **步骤 1：读 frame.go**

确认 `DecodeFrame` 的解析边界（StreamID 4B / FrameType 1B / Flags 1B / PayloadLength 2B / Payload），找 `#301` 崩溃根因（短 WindowUpdate 帧——即 PayloadLength 声明大但实际不足，或长度字段解析未校验）。

- [ ] **步骤 2：编写 fuzz 目标**

```go
func FuzzDecodeFrame(f *testing.F) {
    // seed corpus：合法帧 + 边界形状
    sid := mux.StreamID(1)
    valid, err := mux.EncodeFrame(sid, mux.FrameData, []byte("hello"))
    if err != nil { f.Fatal(err) }
    f.Add(valid)
    f.Add([]byte{})              // 空
    f.Add([]byte{0, 0, 0, 0})    // 只有 streamID
    f.Add([]byte{0, 0, 0, 0, 1}) // streamID + 半个头
    f.Add(valid[:len(valid)-1])  // 截断
    // 短 WindowUpdate：帧类型 WindowUpdate + 声明长度>实际
    f.Add([]byte{0, 0, 0, 1, byte(mux.FrameWindowUpdate), 0, 0, 1, 0}) // 声明 256 字节 payload 但只有 0 字节
    f.Fuzz(func(t *testing.T, data []byte) {
        _, _, _, err := mux.DecodeFrame(data)
        if err == nil {
            // 解析成功必须完整消费（若 DecodeFrame 允许截断输入返回部分结果则调整断言为「不 panic」）
            return
        }
        // 错误必须可预期（哨兵/包装错误），不 panic
    })
}
```

> 断言要点：**不 panic**（`defer recover` 不必要——fuzz 框架会把 panic 当 crash）；解析成功时结果与输入自洽。

- [ ] **步骤 3：运行 seed 与短时 fuzz**

```bash
go test -count=1 -run FuzzDecodeFrame ./pkg/tunnel/mux/
go test -count=1 -fuzz=FuzzDecodeFrame -fuzztime=5s ./pkg/tunnel/mux/
```

预期：无 crash；若有 crash → 修复 frame.go 解析边界（本次任务包含修复——崩溃是真实缺陷，按「先复现再修」流程）。

- [ ] **步骤 4：变异验证**

临时把 `DecodeFrame` 的 PayloadLength 上界校验去掉 → seed 中「短 WindowUpdate」必须触发 panic/crash 或异常；恢复 → 复绿。记录变异结果。

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mux/frame_fuzz_test.go
git commit -m "test(mux): 帧解析 fuzz 覆盖，防短帧解析崩溃回归" --no-verify
```

---

### 任务 2：TCP 传输层帧定界 fuzz

**文件：**
- 创建：`pkg/tunnel/xfer/internal/tcp/tcp_fuzz_test.go`
- 参考：`pkg/tunnel/xfer/internal/tcp/tcp.go`（`tcpConn` 4B 长度前缀帧定界，:42-:77）、`tcp_test.go`

**目标：** `FuzzTcpFraming` 任意字节流解析不 panic、长度上界受控（超大声明长度不分配巨内存）。

- [ ] **步骤 1：读 tcp.go**

确认 `Receive` 的定界逻辑：4B 大端长度 → 读 payload。找长度上界校验（是否 clamp 到连接 buffer/协议上限）。

- [ ] **步骤 2：编写 fuzz 目标**

```go
func FuzzTcpFraming(f *testing.F) {
    f.Add([]byte{0, 0, 0, 0})                 // 空消息
    f.Add([]byte{0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o'}) // 合法 5B 消息
    f.Add([]byte{0, 0, 0, 0xff, 0xff, 0xff, 0xff})     // 超大声明（应被拒/截断，不 panic 不巨分配）
    f.Add([]byte{0, 0, 0, 5, 'h'})            // 声明 5 只来 1
    f.Fuzz(func(t *testing.T, data []byte) {
        // 用 net.Pipe 或内存 conn 夹具（xfertest.Pipe 或自建 bytes conn）喂入 data，
        // 断言 Receive 不 panic、不分配超界内存（可通过声明长度>某阈值时快速返回错误验证）
    })
}
```

> 夹具参考 `pkg/tunnel/xfer/xfertest/pipe.go`（内存通道实现，无真实网络——CI 友好）。

- [ ] **步骤 3：运行 seed 与短时 fuzz**

```bash
go test -count=1 -run FuzzTcpFraming ./pkg/tunnel/xfer/internal/tcp/
go test -count=1 -fuzz=FuzzTcpFraming -fuzztime=5s ./pkg/tunnel/xfer/internal/tcp/
```

预期：无 crash；有 crash → 修 tcp.go 长度上界。

- [ ] **步骤 4：变异验证 + Commit**

```bash
git add pkg/tunnel/xfer/internal/tcp/tcp_fuzz_test.go
git commit -m "test(tcp): 传输层帧定界 fuzz，防超大声明长度内存放大" --no-verify
```

---

### 任务 3：tunnel 统一帧元数据 fuzz

**文件：**
- 创建：`pkg/tunnel/frame_fuzz_test.go`（如已有 frame 解析文件则以实际文件名为准）
- 参考：`pkg/tunnel/tunnel.go` 或 `stream.go` 中的帧协议实现（`[4B metaLen][encrypted metadata][stream chunks...]`）

**目标：** 元数据头解析（4B metaLen）任意输入不 panic；metaLen 上界受控。

- [ ] **步骤 1：定位帧解析代码**

读 `pkg/tunnel/` 下帧协议实现（`stream.go`/`tunnel.go`/`xfer` 封装），找 metaLen 解析点与上界校验。

- [ ] **步骤 2：编写 fuzz 目标**

```go
func FuzzTunnelMetaFrame(f *testing.F) {
    f.Add([]byte{0, 0, 0, 0})
    f.Add([]byte{0, 0, 0, 10})
    f.Add([]byte{0xff, 0xff, 0xff, 0xff}) // 超大 metaLen
    f.Add([]byte{0, 0, 0, 5, 1, 2, 3})    // 声明 5 只有 3
    f.Fuzz(func(t *testing.T, data []byte) {
        // 调解析函数，断言不 panic；metaLen 声明超过协议上限时快速返回错误
    })
}
```

- [ ] **步骤 3：运行 seed 与短时 fuzz + 变异验证 + Commit**

```bash
go test -count=1 -run FuzzTunnelMetaFrame ./pkg/tunnel/
go test -count=1 -fuzz=FuzzTunnelMetaFrame -fuzztime=5s ./pkg/tunnel/
git add pkg/tunnel/frame_fuzz_test.go
git commit -m "test(tunnel): 统一帧元数据头 fuzz，防 metaLen 解析崩溃" --no-verify
```

---

### 任务 4：CI 接线与文档

**文件：**
- 修改：`.github/workflows/*.yml`（如 CI 有 fuzz job 则登记三个新目标；无则新增轻量 fuzz job——**注意**：CI fuzz 限时（如 `-fuzztime=30s`），防 Benchmark job 式超时）
- 修改：`docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md`（§3-E fuzz 扩展落地标注——随本 PR 一并提交，防纯文档 PR）

**目标：** 三个 fuzz 目标进 CI（限时跑）；roadmap 落地记录。

- [ ] **步骤 1：登记 CI**

在 CI 测试 job 中加（或扩展现有 fuzz job）：

```yaml
- name: Fuzz (bounded)
  run: go test -fuzz=FuzzDecodeFrame -fuzztime=30s ./pkg/tunnel/mux/ && go test -fuzz=FuzzTcpFraming -fuzztime=30s ./pkg/tunnel/xfer/internal/tcp/ && go test -fuzz=FuzzTunnelMetaFrame -fuzztime=30s ./pkg/tunnel/
```

> 若 CI 已有限时 fuzz 模式（查 `.github/workflows/` 现状），按现有模式扩展。

- [ ] **步骤 2：roadmap 标注**

`2026-09-14-sproxy-next-roadmap.md` §3-E 行尾加「（2026-09-17：fuzz 扩展已落地，见 PR #xxx）」。

- [ ] **步骤 3：本地全量验证**

```bash
go build ./...
make lint  # 0 issues
go test -count=1 ./pkg/tunnel/... 
```

- [ ] **步骤 4：Commit**

```bash
git add .github/workflows/ docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md
git commit -m "ci(fuzz): 限时 fuzz job 覆盖 mux/tcp/tunnel 帧解析" --no-verify
```
