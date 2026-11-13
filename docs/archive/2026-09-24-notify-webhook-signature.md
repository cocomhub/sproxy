<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 通知出站签名（roadmap 11.10-⑤）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-notify-webhook-signature.md` 已于 2026-11-13 归档。
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**；配置字段与用法以 `docs/config.md` 为准，
> 实现以代码与 git 历史为准。

## 背景
webhook 渠道出站是裸 POST（`{title,text,object,action}`），无任何身份/防伪机制 —— 接收方无法区分
「sproxy 真实事件」与「伪造回调」。目标：出站携带 HMAC-SHA256 签名头，接收方用共享密钥验签；其它渠道
（wecom / serverchan / email / alertmanager / grafana）行为零变化。

## 定案与取舍（仍有效）
1. **secret 为空 = 不签名**，逐字旧行为（golden 头断言锁定）；签名是**显式开启**的能力，未配置即零回归。
   代价：空 secret 仅适用于内网/受信网络，必须在文档中写明而不是默认强签（强签会打断既有部署）。
2. **签名格式带版本前缀** `sha256=<ts>.<hex>`：算法演进（换 hash / 加 nonce）时接收方按前缀分流，
   `sha256=` 前缀本身即对外契约的一部分 —— 去掉前缀等价于破坏性协议变更。
3. **防重放靠时间戳 + ±skew 窗口**（默认 5m，可配）：设计初期设想的「时间戳 + 随机 nonce」中
   **nonce 未落地**；当前最小可用集 = 时间戳 + 时钟漂移容忍。若后续要抗长窗口重放需补 nonce（协议加字段）。
4. **验签失败不吞**：网络失败才重试；配置/时钟类错误记 WARN（含渠道名，**不含 secret**）。
   重试语义沿用既有 `NotifierError`，且**每次重试重新计算签名**（时间戳刷新）⇒ 去抖/历史语义不变。
5. **空 body 必失败**：常量时间比较仍执行、不短路（避免以「提前返回」泄露信息）。
6. **密钥只来自配置，不入审计日志**（凭据纪律）。

## 已知局限（不要当缺陷重复提）
`subtle.ConstantTimeCompare` 换成 `==` 时**单测仍然绿**（无外部可观测差异，造竞态/时序测试既不稳也不值）
⇒ 该点只能靠代码审查守住，已显式记为已知局限。

## 依赖与外部契约
- 仅用标准库 `crypto/hmac` + `crypto/subtle`，无第三方依赖。
- 出站头（`X-Sproxy-Signature` / `X-Sproxy-Timestamp`，头名可配）与签名体格式 = 对外契约；
  接收侧验签示例属部署方实现（Go/Python 示例曾随文档交付）。
- 字段与默认值（`secret` / `sign_header` / `timestamp_header` / `clock_skew`）见 `docs/config.md` 通知渠道表。

## 落地指针
`pkg/server/notify/center.go`（`WebhookNotifier.Send` / `VerifyWebhookSignature`）、
`pkg/server/config/notify.go`（签名相关配置字段）。
