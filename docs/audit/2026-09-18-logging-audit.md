# 日志使用审计报告（2026-09-18）

## 结论：日志体系健康

### 1. auth.go 包级日志（记忆遗留）——已修复，0 处
- 记忆记录「pkg/server/auth.go package-level slog.Warn 每请求 1 条」——**已过时**
- 现状：auth.go 全部 `a.log()/h.log()`（受控 logger），0 处 package-level slog
- routes.go:218 `NewRingAuthenticator(..., WithRingLogger(log))` 注入——生产受控 ✓

### 2. 剩余 63 处 package-level slog——均为低频合理场景
| 位置 | 数量 | 场景 | 评价 |
|---|---|---|---|
| cmd/sproxy/root.go | 14 | 装配期启动日志 | 合理（启动可见性） |
| pkg/tunnel/tunnel_mux.go | 4 | 握手失败/重放检测 | 低频安全告警，合理 |
| pkg/telemetry/ext/otel | 3 | 配置警告 | 合理 |
| webrtc/relay/turnrest | 6 | TURN/配置警告 | 低频合理 |
| 其它（certmgr/client/sproxycfg） | 4 | 一次性告警 | 合理 |

### 3. 高频路径检查
- **无高频路径包级日志**（auth 已修；上传/同步/认证路径全受控）
- 包级日志都在【低频/初始化/安全告警】场景——非噪声

### 4. 潜在优化（可选，非必须）
- webrtc.go/turnrest.go 包级日志 → 注入 logger（低频，收益小）
- root.go 装配日志保持（启动可见性）

## 建议
- 无需大规模改动；如需进一步收敛，仅 webrtc/turnrest 注入 logger（低优先）
