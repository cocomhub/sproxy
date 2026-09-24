# 设计：NAT 穿透失败告警（11.1-①）

## 背景/目标
- 现状：`AlertEngine` source 枚举仅 disk_watermark/volume_degraded/sync_failed/login_locked/quota_watermark（alerts.go）；hub/relay/webrtc（STUN/TURN）拨号失败只有日志，未接告警——出口断链、打洞失败等网络故障运维无感知。
- 目标：补 `nat_failure` source + `OnNATFailure/OnNATRecovered` 事件接口，服务端 mesh 相关拨号失败 → 通知渠道外发；恢复（同 peer 后续拨号成功）自动发恢复通知。

## 组件与接口
- `pkg/server/alerts.go`：
  - 常量 `SourceNATFailure = "nat_failure"`（与既有 source 字符串同风格）。
  - `func (e *AlertEngine) OnNATFailure(ctx, peer, detail string)`：`rulesFor(SourceNATFailure)` → `fire(ctx, "nat_failure\x00"+peer, r, "NAT/中继拨号失败 ...")`。
  - `func (e *AlertEngine) OnNATRecovered(ctx, peer string)`：`recover(ctx, "nat_failure\x00"+peer, ...)`。recover 内部已判 `state != "firing"` 即 no-op，故每次拨号成功都可安全调用。
  - AlertRule.Source 注释补 nat_failure 取值。
- `cmd/sproxy/root.go`（main 装配层，pkg/server 不 import mesh 拨号，防包环）：
  - `withNATAlert(dial DialFunc, e *server.AlertEngine, peer string) DialFunc`：包装闭包——`derr != nil → e.OnNATFailure(ctx, peer, derr)`；成功 → `e.OnNATRecovered(ctx, peer)`；返回原 conn/err（告警是旁路副作用，绝不吞错）。`e == nil` 时直接返回原 dial（零开销）。
  - 挂点①：`buildCloudExitDial` 产物（云端下载经 mesh 出口，peer=cfg.CloudDownloadExitNode）。
  - 挂点②：`startMeshNodeRoleWithCreds` 的出口/webrtc 拨号路径（peer=目标 node id）。
  - 挂点③（可选）：hub 联邦拉取失败（peer=peer.ID）。

## 数据流
拨号错误 → withNATAlert → OnNATFailure → rulesFor 匹配 `nat_failure` 规则 → fire（状态机：首次 firing 才通知，同 peer 去抖）→ dispatch → NotifyCenter 渠道（wecom/serverchan/…）→ 渠道失败由既有 dispatch Warn + NotifyCenter 重试。同 peer 后续拨号成功 → OnNATRecovered → recover → 恢复通知。

## 错误处理
- AlertEngine 未启用（nil）→ 包装 no-op，拨号行为零变化。
- 拨号错误仍原样向上传播（fail-closed 语义不变：出口失败不静默回退本地）。
- dispatch 失败由既有 dispatch 内 Warn 处理（不改）。
- 无 `nat_failure` 规则 → fire/recover 均无渠道命中，零输出。

## 测试 + 变异点
- `TestAlertNATFailure_FireAndRecover`：注入 mock 渠道 + 规则 → 失败事件发通知（state=firing）；随后成功事件发恢复通知（state=ok）。**变异**：删除 OnNATFailure 内 fire → 红；删除 OnNATRecovered → 红。
- `TestAlertNATFailure_KeyPerPeer`：peer A 失败不影响 peer B 状态；同 peer 重复失败只通知一次。**变异**：key 拼接符/peer 参数写错 → 红。
- `TestAlertNATFailure_NoEngineNoOp`：nil engine 包装透传原错误。**变异**：wrapper 吞错 → 红。
- `TestAlertNATFailure_NoRuleSilent`：无 nat_failure 规则时事件不通知。**变异**：默认匹配所有规则 → 红。
- 装配侧：`TestCloudExitDial_WrapsNATAlert`（mock AlertEngine 断言 peer 与错误传播）。

## 片划分
- 片 1：pkg/server 侧 source + 两方法 + 单测（纯引擎，无网络）。
- 片 2：main 装配 withNATAlert + 两个挂点 + 测试；docs/ 更新（新 source 写入 alerts 文档与配置示例）。

## 风险与零回归保证
- 新 source 仅在规则显式引用 `nat_failure` 时生效（无规则零告警）；既有 5 个 source 路径零改动。
- 包装器只在新增挂点注入，既有 dial 路径（本地直连/普通出口）不触碰。
- 状态机复用 fire/recover，去抖与恢复语义与既有 source 完全一致。
- 风险：恢复事件依赖「后续成功拨号」触发——若故障期间无新拨号则保持 firing（符合预期，避免误报恢复）。
