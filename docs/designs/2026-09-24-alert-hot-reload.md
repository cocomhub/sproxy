# 设计：告警规则热加载（11.1-②）

## 背景/目标
- 现状：`handleSighup`（cmd/sproxy/root.go）重载后仅 log_level/log_format 生效，notify.alerts 无热加载——改告警阈值/渠道必须重启进程，运维窗口内告警口径失真。
- 目标：SIGHUP 补 `notify.alerts` 重载：AlertEngine 规则集**原子替换**，渠道随 NotifyCenter 已注册渠道保持（渠道配置属 notify 段，本期不重建渠道实例，仅换规则）；重启生效项维持现状。

## 组件与接口
- `pkg/server/alerts.go`：
  - 新增 `func (e *AlertEngine) ReloadRules(rules []AlertRule)`：
    - `e.mu.Lock()` 下 `e.rules = slices.Clone(rules)`（**原子替换**，读方（fire/rulesFor/轮询）持锁快照，无撕裂）；
    - **保留既有 firing 状态**（state 不清空）——规则变更不误发恢复通知，也不重复告警；新增/删除规则后，下轮事件/轮询按新规则判定。
  - 语义约束（注释写明）：阈值类 source（disk_watermark/quota_watermark）新规则在下个轮询 tick 生效（≤ PollInterval 延迟）；事件类 source（volume_degraded/sync_failed/login_locked/nat_failure）下个事件即生效。
- `cmd/sproxy/root.go`：
  - `runServer` 把 AlertEngine 句柄传出（现有 `h.AlertEngine()` 已可经 Handlers 获取——装配后 `h.AlertEngine()` 非 nil；handleSighup 无 h 引用，需把 engine 指针经包级/参数传递）。
  - `handleSighup` 补：`if eng := h.AlertEngine(); eng != nil { eng.ReloadRules(newCfg.Alerts.Rules) }`，并 `slog.Info("alerts 规则已热加载", "rules", len(...))`。
  - `newCfg.Alerts.Enabled` 从 true→false 或反之：本期**仅规则热加载**；启用状态翻转需重启（与 notify 渠道重建同语义，注释声明）——`alerts.enabled` 属于装配期决策（渠道 AdoptNotifyChannels 在装配时完成），SIGHUP 不重建装配。
- `pkg/server/handlers.go`（若 h.AlertEngine 已存在则复用；未装配时 nil 判断）。

## 数据流
SIGHUP → cfgProvider.Refresh + LoadFromProvider → 既有硬配置比对告警（addr/storage_root 等维持需重启日志）→ `eng.ReloadRules(newCfg.Alerts.Rules)` → 原子换规则 → 后续事件/轮询按新规则 fire/recover（state 保留）→ 渠道不变（仍走 NotifyCenter 注册实例）。

## 错误处理
- 配置文件解析失败：既有 `handleSighup` 返回路径不变（报错并保持旧配置，**不部分应用**）。
- alerts.enabled 翻转：Warn「alerts.enabled 修改需重启进程生效」，规则仍按新 Rules 加载（规则集独立于 enabled 位，行为一致）。
- engine 为 nil（alerts 未启用）：跳过 ReloadRules，零影响。

## 测试 + 变异点
- `TestAlertEngine_ReloadRules_AtomicSwap`：注入规则 A 触发 fire → ReloadRules(B) → 新事件走 B（B 渠道命中、A 不命中）。**变异**：ReloadRules 未持锁直接赋值 → race/红；slices.Clone 换成引用赋值（旧切片被外部改）→ 红。
- `TestAlertEngine_ReloadRules_PreservesState`：firing 状态跨 ReloadRules 保留（不误发恢复通知、不重复 fire）。**变异**：ReloadRules 清空 state → 红。
- `TestAlertEngine_ReloadRules_ThresholdNextTick`：disk_watermark 阈值 80→90，轮询 tick 后新阈值生效（mock ticker 或直接调 checkDiskWatermark）。**变异**：ReloadRules 只换部分字段 → 红。
- SIGHUP 集成：`TestHandleSighup_ReloadsAlertRules`（cmd/sproxy 既有测试模式，testSignalCh 注入 SIGHUP；断言新规则生效 + 日志）。**变异**：handleSighup 漏调 ReloadRules → 红。
- **变异验证**：把 ReloadRules 改成 append（旧规则残留）→ 红。

## 片划分
- 片 1：pkg/server `ReloadRules` + 3 单测。
- 片 2：handleSighup 接线（engine 传递）+ SIGHUP 集成测试 + 文档（docs/config.md 标注 alerts 为软配置可热加载）。

## 风险与零回归保证
- ReloadRules 是新增方法，既有调用零改动；SIGHUP 行为对未配置 alerts 的部署完全不变。
- 原子替换保证并发轮询/事件读取无撕裂；state 保留避免热加载瞬间误报。
- 风险：规则热加载生效与轮询 tick 存在 ≤PollInterval 延迟——文档明示；enabled 翻转需重启为有意限制（装配期渠道 Adopt 不可逆）。
