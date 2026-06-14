# sproxy 测试基础设施与覆盖提升设计方案

**日期**: 2026-06-15
**状态**: 已批准
**优先级**: 基础设施 > 功能补齐 > 债务清理 > 测试覆盖

## 1. 背景与目标

### 1.1 当前状态

- 总计覆盖率 ~74.7%，cmd/sclient 仅 39.4%
- 74 个测试文件，多种测试模式（table-driven / httptest / fuzz / E2E）
- 5 项已知技术债务（signal leak、os.Exit 阻断、重复代码、路由注册冗余、findModuleRoot 冗余）
- 无覆盖率门禁、无 benchmark 基线、linter 仅 8 个

### 1.2 目标

- 覆盖率提升至 ~90%（每包独立测量）
- Benchmark 基线系统（本地保存 10 次 + CI artifact + Web 趋势图）
- 覆盖率门禁（CI 模式阈值 85%）
- linter 从 8 个扩充到 16 个
- Pre-commit hook
- 所有函数/方法进行参数校验，不合法参数返回 error，并补充对应测试

## 2. 四阶段计划

### 阶段 1：基础设施

| 项目 | 改动文件 | 说明 |
|------|---------|------|
| 覆盖率门禁 | `Makefile` | `CI=true make cover` 检查阈值 85% |
| Benchmark 基线 | `Makefile`、`.github/workflows/ci.yml`、`tools/genbenchview/main.go` | `make bench` / `bench-compare` / `bench-web` |
| 增强 linter | `.golangci.yml` | +revive/gocritic/gosec/whitespace/goimports/paralleltest/thelper/reassign |
| Pre-commit hook | `.githooks/pre-commit`、`Makefile` | `go vet` + loopback 检查 + `gofmt -l` |
| 工具依赖 | `Makefile` | tools 目标安装 addlicense、benchstat |

### 阶段 2：功能补齐

| 项目 | 改动文件 |
|------|---------|
| os.Exit 全面消除 | cmd/sclient/ 下所有命令文件（~15 个）|
| sclient 命令完善 | tunnel/genkey/search/stat/diag/mv/relay/archive/batch + 各对应测试 |
| relay base64 TODO | pkg/server/relay.go |
| Web UI 测试 | web/embed_test.go |

### 阶段 3：技术债务清理

| 项目 | 改动文件 |
|------|---------|
| Signal handler 泄漏 | cmd/sproxy/root.go + root_test.go |
| captureStdout 重复 | cmd/sclient/cmd_test.go（改为 pkg/testutil）|
| newTestServerWithAllRoutes 重复 | pkg/server/integration_test.go |
| findModuleRoot 冗余 | test/e2e_test.go |
| context.TODO→Background | pkg/server/server_handler_gaps_test.go、server_hub_test.go |
| t.Skip 移除/确认 | cmd_rune_test.go（5 处）|

### 阶段 4：测试覆盖提升

| 包 | 目标 | 新增测试内容 |
|----|------|-------------|
| cmd/sclient | 39%→90% | 全部子命令 + error path |
| cmd/sproxy | 62%→90% | config/flags/signal/logger 错误路径 |
| pkg/server | 70%→90% | slogger/relay/handler 错误分支 |
| pkg/client | 74%→90% | format.go + error path |
| pkg/tunnel | 82%→90% | 超时/取消/最大流数边缘路径 |
| fuzz | 3→5 个 | validate + chunked |
| E2E | 扩展 | 隧道/分片/批量/relay 链路 |

## 3. 关键设计决策

### 3.1 Benchmark 基线存储格式

```
build/benchmark/
  data/
    <branch>-<commit-short>-<YYYYMMDDTHHMMSS>.txt
  web/
    index.html    # Chart.js 趋势图
```

- 每条记录包含：分支名、commit hash、时间戳、完整 benchmark 输出
- 滚动保留最近 10 条（按文件名时间戳排序，删除最旧）
- `bench-compare` 使用 `benchstat` 对比最近两次

### 3.2 覆盖率门禁

- `CI=true` 环境变量区分本地和 CI 运行
- 阈值 85%（当前 74.7%，阶段 4 达成前 CI 预期会失败）
- 仅阻止覆盖率下降，不阻止未达标的 PR——但给出明确告警

### 3.3 参数校验规则

在阶段 2+4 的实现中，对所有函数/方法补充：
1. 空字符串参数 → `ErrInvalidArgument` 或 `fmt.Errorf`
2. 负值/零值数值参数 → 返回 error
3. nil 接口/指针参数 → 返回 error
4. 超出允许范围的值 → 返回 error
5. 每个校验点对应一个测试 case

## 4. 风险与缓解

| 风险 | 缓解 |
|------|------|
| go.work 多模块下 linter 不生效 | CI 中对每个 module 分别运行 golangci-lint |
| benchstat 跨平台问题 | benchmark 仅在 ubuntu CI 运行 |
| relay base64 改造影响现有 client | 向后兼容：IsBase64 标记 |
| Windows 测试差异 | 保留平台特定 skip，用 build tag 隔离 |
