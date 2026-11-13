<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 智能运维 LLM（告警根因建议，11.9-⑥）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-ai-advisor.md` 已于 2026-11-13 归档。
> 配置键与默认值以 `docs/config.md`（§服务端配置 · `notify.ai_advisor.*`）为准（现行事实源），
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图

告警引擎此前只有事实描述（「磁盘水位 85% ≥ 阈值 80%」），无根因/处置建议。目标：告警时经
LLM 网关（OpenAI 兼容 `/chat/completions`）生成根因 + 处置建议，**追加**到通知正文。

## 定案与取舍（仍有效）

1. **AI 是增强层，绝不吞告警（fail-closed）**：未启用 / 无 key / 网关失败 / 超时 ⇒ 返回空建议，
   通知正文 = 固定模板原样。这条是全线红线。
2. **默认关闭零回归**：`enabled=false` ⇒ 网关为 nil ⇒ 建议恒空 ⇒ 通知文本与未装配时逐字节一致。
3. **key 只经环境变量**：配置只写**环境变量名**（`api_key_ref`），key 本体不落 yaml / 不落日志；
   非法环境变量名在启动校验即拒绝（fail-closed 防脚枪），空 + `enabled=true` 允许但装配期 Warn 一次
   并降级（可观测的降级，不是静默）。
4. **同步追加而非异步**：建议在 dispatch 内与渠道发送同级生成（告警低频，最多被拖慢一个超时）；
   不引入队列/缓存/去抖新面。
5. **prompt 注入面收敛**：system prompt 固定，告警正文只作 user 消息的**数据**（不拼接指令）。
6. **只追加不改写**：仅追加「【AI 建议】」段；标题不变；recover 通知同样走 AI；建议文本只进
   通知载荷（渠道/邮件/历史），**不入审计**。
7. **外部契约**：OpenAI 兼容 JSON 形状（`choices[0].message.content` 字符串形态，不做多模态/工具）；
   Anthropic 走兼容端点同一形状（原生 messages API 是后续片，未做）；出口 HTTP 走
   `netutil.IsolatedTransport()`（禁裸 `http.Transport`），响应体读取有上限（防畸形响应撑内存）。
8. **成本/泄露权衡（如实披露）**：每次 fire/recover 一次调用（去抖已把同 key 限制为状态变化级）；
   通知正文（可能含文件名/路径）会发往外部 LLM ⇒ 由部署者按数据敏感度决定是否开启。

## 落地指针

- `pkg/llmgate`（网关客户端：provider/baseURL/model 归一、超时回落、响应体上限）；
- `pkg/server/ai/advisor.go`（`AIAdvisor`/`promptSystem`/`buildUserPrompt`/`Advise` + 截断上限）；
- `pkg/server/alerts/advisor.go`（`NewAIAdvisorFromConfig` 装配 + `Engine.SetAdvisor` 注入点）；
- 配置模型 `pkg/server/config/ai.go`（`AIAdvisorConfig`）+ 校验 `pkg/server/config/validate.go`。

## 实测差异（以代码为准）

- 设计里的「片4」项（Anthropic 原生 messages API、建议缓存/去抖、建议落盘）**至今未实现**——
  当前只有 OpenAI 兼容形状与「只进通知载荷」形态；解读本篇时勿把片4当成现状。
- 具体数值（截断上限 1000 字、响应体上限 64 KiB、超时回落 10s、未知 provider 回落 `openai`）
  以实现为准，权威值见 `docs/config.md`。
