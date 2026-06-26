# Sproxy 项目功能完整性与质量提升设计

**日期**: 2026-06-26
**状态**: 已批准
**策略**: 全量修复（方案 A）

---

## 1. 背景与范围

### 1.1 当前状态

| 维度 | 指标 |
|------|------|
| 测试总数 | ~700（667 Test + 11 Benchmark + 3 Fuzz + 15 Example） |
| 测试通过率 | 99.7%（WebSocket 2 个测试失败） |
| 核心服务覆盖率 (`pkg/server`) | 74.4% |
| 核心客户端覆盖率 (`pkg/client`) | 80.0% |
| sclient CLI 覆盖率 | 52.0% |
| QUIC 传输覆盖率 | 11.0% |
| sclientcfg/sproxycfg 覆盖率 | 0% |

### 1.2 核心发现

- **文件服务**：全部路由功能完整，测试覆盖良好
- **加密隧道**：核心加解密/帧协议/多路复用完整且稳定
- **Hub 中继/P2P**：架构完备但部分传输层（gRPC、WebRTC、QUIC）测试不足
- **WebSocket 传输**：2 个接口合规性测试失败
- **Goroutine 泄漏**：信号处理 goroutine 在 ListenAndServe 失败时不退出（已知技术债务）
- **Web UI**：功能齐全但无自动化测试

### 1.3 范围

修复所有已知缺陷，将项目整体质量提升到生产级标准。不新增功能特性。

---

## 2. Phase 1 — 关键缺陷修复

### 2.1 WS 传输测试修复

**根因**：`pkg/tunnel/xfer/ext/ws/ws.go` 的 `wsConn.Send()` 通过 buffered channel 传递给后台发送 goroutine。连接关闭后 channel 未关闭，`Send` 在 channel 写入上永久阻塞。`ContextCancellation` 失败原因相同——`Send` 没有监听 `ctx.Done()`。

**修复**：

```
wsConn 增加：
- closed    atomic.Bool        // 已关闭标志
- done      chan struct{}      // 关闭时广播

Send() 增加 select 监听：
- ctx.Done()
- c.done

Close() 中：
- 设置 closed=true
- close(c.done)
- 后台 goroutine 在 defer 中 close(done)

Dial 失败路径：
- wsConn 创建失败时也关闭 done channel
```

**涉及文件**：`pkg/tunnel/xfer/ext/ws/ws.go`

**验收标准**：
- `TestWS/ws/CloseWhileBlocking` PASS
- `TestWS/ws/ContextCancellation` PASS
- 全部 xfertest 套件通过

---

### 2.2 Goroutine 泄漏修复

**根因**：`cmd/sproxy/root.go:runSignalHandler` 中 `for sig := range signalChan` 循环在 `srv.ListenAndServe()` 失败时永不退出，因为 `signalChan` 未关闭。

**修复**：

```
runSignalHandler 内部：
1. 创建 done chan struct{}
2. 启动信号监听 goroutine，增加 select 监听 done
3. 当 startPlainListener / startTLSListener 返回 error 时 close(done)
4. 使用 context.AfterFunc 或 defer close(done) 在所有退出路径上通知
```

**涉及文件**：`cmd/sproxy/root.go`

**验收标准**：`go test -race -count=1` 在 `cmd/sproxy` 子模块中无 goroutine 泄漏检测

---

### 2.3 QUIC 传输测试补全

**问题**：测试覆盖率仅 11%，`quic_test.go` 缺少 `xfertest` 通用传输套件。

**修复**：
1. 引入 `xfertest` 通用传输套件（与 WS/TCP 测试使用相同模式）
2. 补充子测试：`CloseWhileBlocking`、`ContextCancellation`、`ConcurrentSends`、`BasicEcho`

**涉及文件**：`pkg/tunnel/xfer/ext/quic/quic_test.go`

**验收标准**：覆盖率 ≥ 50%

---

## 3. Phase 2 — 覆盖率提升

### 3.1 sclient CLI 覆盖率 52% → 70%

**修复**：
1. 审计所有 `os.Exit(1)` 调用点，替换为返回 `error`
2. 涉及子命令：`upload.go`、`download.go`、`delete.go`、`batch.go`、`archive.go`
3. 补充测试场景：`config show/set`、`relay`、`diag`、`stat`、`search` 各子路径
4. 沿用现有本地 `captureStdout`/`captureStderr` 辅助函数（`package main` 无法导入 `pkg/testutil`）

**涉及文件**：`cmd/sclient/upload.go`、`download.go`、`delete.go`、`batch.go`、`archive.go`、`cmd_test.go`

**验收标准**：`cmd/sclient` 子模块覆盖率 ≥ 70%

---

### 3.2 零覆盖包测试

**范围**：

| 包 | 类型 | 测试策略 |
|------|------|----------|
| `pkg/provider` | 接口定义 | 基础实现测试，验证接口契约 |
| `cmd/sproxy/internal/sproxycfg` | ViperProvider | 验证从 viper 实例加载配置的完整路径 |
| `cmd/sclient/internal/sclientcfg` | ViperProvider | 同上，验证客户端配置路径 |

**验收标准**：每个包覆盖率 > 0%（`pkg/provider` → ≥80%，cfg 包 → ≥60%）

---

### 3.3 Web UI e2e 测试

**架构**：独立 Go 子模块，不污染主仓库依赖

```
web/e2e/
  go.mod                    # 独立子模块
  ui_e2e_test.go            # Playwright 浏览器测试
```

**测试场景**（9 个）：

| 测试 | 场景 |
|------|------|
| `TestUILoads` | 首页加载、`/ui/` 重定向、静态资源可访问 |
| `TestAuthFlow` | Token 输入认证、localStorage 持久化 |
| `TestFileList` | 文件列表渲染、面包屑导航、分页加载更多 |
| `TestUploadFlow` | 上传文件、进度显示、完成后列表刷新 |
| `TestDownloadFlow` | 下载文件按钮、流式下载（隧道模式） |
| `TestFileOperations` | 重命名、删除、创建目录、搜索 |
| `TestBatchOperations` | 批量删除、批量重命名、打包下载 |
| `TestDirectoryNavigation` | 进入/返回子目录、面包屑点击 |
| `TestStatsPanel` | 监控弹窗（磁盘使用、请求统计） |

**测试辅助**：
- 启动 `sproxy` 测试实例（`httptest.Server`）作为后端
- 预置测试文件（临时目录 + `t.TempDir()`）
- 每个测试前后清理 localStorage + 文件状态

**CI 集成**（`.github/workflows/ci.yml` 新增可选 job）：
```yaml
ui-e2e:
  if: github.event_name == 'pull_request' || github.ref == 'refs/heads/master'
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
    - run: cd web/e2e && go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium
    - run: cd web/e2e && go test -v -count=1 ./...
```

**依赖隔离**：`web/e2e/go.mod` 为独立子模块，不加入 `go.work`（或仅在需要时手动加入）。主仓库 `go.mod` 不受影响。

**验收标准**：9 个测试场景全部 PASS

---

## 4. Phase 3 — 代码清理

### 4.1 根目录散落文件清理

**问题**：根目录散落覆盖率中间产物（`cover*.out`、`coverage.out`、`coverage.tmp`、`size.cover`、`full.cover`、`e2e.cover`）。

**修复**：
1. `.gitignore` 增加：`cover*.out`、`coverage.tmp`、`*.cover`（覆盖率文件）、`*.exe`（Windows 编译产物仅限根目录）
2. `make clean` 增加：`rm -f cover*.out coverage.tmp *.cover`
3. 执行一次清理，`git rm --cached` 已追踪的临时文件

**涉及文件**：`.gitignore`、`Makefile`

---

### 4.2 统一 `newTestServerWithAllRoutes` 路由注册

**问题**：`pkg/server/integration_test.go` 中的 `newTestServerWithAllRoutes` 手动重复了 `RegisterRoutes` 的完整路由表，新增路由时容易不同步。

**修复**：
1. 改为直接调用 `RegisterRoutes(ctx, opts)`
2. 通过 `RegisterRoutesOpts` 控制是否需要本地路由/auth 路由
3. 删除手工维护的路由表副本

**涉及文件**：`pkg/server/integration_test.go`

---

### 4.3 gRPC/WebRTC 传输测试提升

| 传输 | 当前覆盖率 | 目标 | 修复 |
|------|-----------|------|------|
| gRPC (`xfer/grpc`) | 47% | ≥60% | 补充 `xfertest` 通用套件 + 错误路径测试 |
| WebRTC (`xfer/webrtc`) | 59% | ≥65% | 补充 `xfertest` 通用套件 + 超时/信号失败测试 |

**涉及文件**：`xfer/grpc/grpc_test.go`、`xfer/webrtc/webrtc_test.go`

---

### 4.4 杂项清理

| 项 | 操作 | 文件 |
|------|------|------|
| `.golangci.yml.bak` | 删除 | 根目录 |
| `roadmap.md.bak` | 删除 | 根目录 |
| `pkg/tunnel/tunnel_mux.bak` | 删除 | `pkg/tunnel/` |
| `sproxy.exe` / `sclient.exe` | 已由 `.gitignore` 覆盖，确认无追踪 | 根目录 |

---

## 5. 实施路线图

```
Phase 1 (2-3 天)          Phase 2 (4-6 天)               Phase 3 (2-3 天)
┌─────────────────┐     ┌─────────────────────────┐     ┌─────────────────────┐
│ PR#1: WS 修复    │     │ PR#4: sclient os.Exit   │     │ PR#9:  根目录清理   │
│ PR#2: goroutine  │ ──▶ │ PR#5: sclient 测试补全  │ ──▶ │ PR#10: 路由统一     │
│ PR#3: QUIC 测试  │     │ PR#6: 零覆盖包测试      │     │ PR#11: gRPC/WebRTC  │
│                  │     │ PR#7: Web UI e2e 基建   │     │                     │
│                  │     │ PR#8: Web UI e2e 用例   │     │                     │
└─────────────────┘     └─────────────────────────┘     └─────────────────────┘
```

**总计**：11 个 PR，8-12 个工作日

**串行依赖**：
- Phase 2 依赖 Phase 1（WS 修复后 xfertest 套件才能全绿）
- Phase 3 独立于 Phase 1/2，可并行启动

---

## 6. 成功标准

| 指标 | 当前 | 目标 | 测量方式 |
|------|------|------|----------|
| 全部测试通过 | 2 失败 | 0 失败 | `go test -count=1 ./...`（多 module） |
| 核心覆盖率 (`pkg/server`) | 74.4% | ≥74.4%（不退化） | `go test -cover ./pkg/server/...` |
| sclient CLI 覆盖率 | 52% | ≥70% | `cd cmd/sclient && go test -cover ./...` |
| QUIC 覆盖率 | 11% | ≥50% | `cd pkg/tunnel/xfer/ext/quic && go test -cover ./...` |
| gRPC 覆盖率 | 47% | ≥60% | `cd xfer/grpc && go test -cover ./...` |
| WebRTC 覆盖率 | 59% | ≥65% | `cd xfer/webrtc && go test -cover ./...` |
| Web UI e2e | 无 | 9 场景 GREEN | `cd web/e2e && go test -v ./...` |
| Goroutine 泄漏 | 存在 | 无 | `go test -race -count=1 ./cmd/sproxy/...` |
| CI 全绿 | — | ✅ | `make check-ci` |

---

## 7. 风险与缓解

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| `os.Exit` 重构破坏 CLI 行为 | sclient 行为变化 | 仅重构错误路径；保留所有正常路径不变 |
| Playwright 环境安装问题 | CI 不稳定 | 作为可选 job，不影响核心 CI |
| WebRTC 库环境限制 | 测试在 Windows/无头环境失败 | 添加 `//go:build` 约束，仅在支持环境运行 |
| QUIC 依赖编译问题 | 子模块构建失败 | 添加 build tag，与现有 `!windows` 约束一致 |

---

## 8. 不涉及的范围

- 不新增业务功能特性
- 不修改隧道加密协议
- 不修改 Hub/P2P 的网络拓扑
- 不引入新的第三方依赖到主 `go.mod`
- 不修改生产配置文件格式
