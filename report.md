# REPORT: 传输质量感知选路（SmartDial 候选质量加权）

## 交付
`feat(mesh)`：SmartDial 竞速候选按历史质量（mux 重传率）加权——roadmap 5.3/6.3 P1 传输质量感知选路。

## 改动（4 files +305，commit 2dd266f8 分支 feat/quality-routing）
| 文件 | 内容 |
|------|------|
| `pkg/tunnel/mesh/quality_routing.go`（新） | `muxQualitySource` 接口（QualityMetrics）；`RegisterQualitySource`/`QualityOf`（重传率 + Score 0-1；无历史中性 0.5；健康 1.0；高重传趋近 0.5）；`qualityStaggerDelay`=100ms；`logQualityWeighting` 可观测 |
| `pkg/tunnel/mesh/quality_routing_test.go`（新） | TDD 3 测试：ScoreBounds / PicksBetterQuality / NeutralWhenNoHistory（fakeQualityMux 注入） |
| `pkg/tunnel/mesh/smart.go` | `SmartOptions.QualityRouting`（默认 false 零回归）；开启后候选二次预排序（同 Priority 健康优先）+ 劣化候选延迟启动（100ms < RaceWindow 不误伤多跳） |
| `docs/cli.md` | mesh connect 补 `--quality-routing` 说明 |

## TDD + 变异验证
- **红灯**：QualityOf/RegisterQualitySource/qualityNeutralScore 未定义 → 编译红
- **绿灯**：实现后 3 测试全过（-race）
- **变异 3 命中**：
  - 组合开关失效（两处 `if so.QualityRouting` 全禁用）→ PicksBetterQuality 红
  - 延迟启动失效（stagger 分支禁用）→ PicksBetterQuality 红
  - 分数恒 1（劣化不反映）→ ScoreBounds 红
  - （单开关失效变异因另一处开关仍生效未红——组合变异补足）

## 设计决策
1. **重传率分母 = 发送帧数 + 重传次数**（重传也是实际传输，0-1 归一；无传输 = 健康零重传）
2. **分数映射 `1 - rate*0.5`**：健康 1.0 > 无历史 0.5 > 高重传趋近 0.5——健康候选始终优先，劣化候选被降权但不归零（可自愈）
3. **延迟启动**（非硬过滤）：劣化候选晚 100ms 启动——健康候选先胜出，劣化不抢先；小于 RaceWindow（5s）不误伤多跳
4. **无历史中性**：不歧视首次候选（无历史 = 0.5，与健康可比不虚高）
5. **显式开关默认关**：零回归（测试断言默认关选快候选）

## 验证证据
- `go test -count=1 -race ./pkg/tunnel/mesh/` 22.8s 全绿（含既有 smart 竞速测试不回归）
- `golangci-lint run ./pkg/tunnel/mesh/` 0 issues
- `make deadcode-check` PASS（无未登记符号）
- `make prepare` + `go test ./internal/archcheck/` 绿（R18）
- gofmt / go vet 干净

## 残余 / 后续
- **装配层接线未做**：RegisterQualitySource 目前由测试/外部调用方注册——生产装配（pkg/server 或 mesh 建连处把 mux 实例注册为质量源）留后续片（跨 pkg 依赖，需 main 包装配，TASK 未要求）
- CLI `--quality-routing` flag 未加（文档已写；flag 注册在 cmd/sclient meshconn，涉装配层，留后续）
- 延迟启动 100ms 是常量——未来可按候选历史 RTT 自适应（P2）
- 分数只反映重传率；RTT 维度（Latency）已在竞速内自然体现（质量快照含 Latency 字段预留）
