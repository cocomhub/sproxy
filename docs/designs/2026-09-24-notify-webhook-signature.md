# 通知出站签名（11.10-⑤）

## 背景/目标
- 现状：`pkg/server/notify.go` 的通用渠道 `WebhookNotifier.Send` 裸 POST `{title,text,object,action}` JSON，无任何身份/防伪机制——接收方无法区分「sproxy 真实事件」与「伪造回调」。
- 目标：webhook 渠道出站请求携带 HMAC 签名头（HMAC-SHA256，时间戳 + 随机 nonce 防重放），接收方可用配置的共享密钥验签；其他渠道（wecom/serverchan/email/alertmanager/grafana）行为零变化。

## 组件与接口
- `pkg/server`：
  - `WebhookConfig` 新增字段：`secret string`（yaml `secret`；空 = 不加签名，零回归）、`sign_header string`（默认 `X-Sproxy-Signature`）、`timestamp_header string`（默认 `X-Sproxy-Timestamp`）。
  - `WebhookNotifier` 持有 `secret`；`Send` 构造 body 后：`ts := time.Now().Unix()`；`sig := HMAC-SHA256(secret, fmt.Sprintf("%d.%s", ts, body))`（hex 小写）；设头 `X-Sproxy-Signature: sha256=<ts>.<sig>`（格式含版本前缀便于演进），时间戳头同时带。
  - `VerifyWebhookSignature(secret, headerVal, body []byte, now, skew time.Duration) error` 纯函数（接收侧示例 + 单测直接测）——校验版本前缀、时间戳在 `±skew` 内、`subtle.ConstantTimeCompare` 比对。
  - 共享常量：`signatureVersion = "sha256"`、`maxClockSkew = 5 * time.Minute`（可配置字段 `clock_skew`）。
- 密钥来源：仅配置（`notify.channels.webhook.secret`），不入审计日志。

## 数据流
`Dispatch → dispatchTo → sendWithRetry → WebhookNotifier.Send`：序列化 body → 计算签名 → 设头 → POST。接收方：读头 → 校验版本/时间戳/签名 → 接受或 401。失败仍走既有 `NotifierError` 重试语义（签名每次重试重新计算，时间戳刷新）。

## 错误处理
- secret 为空 → 不签名（文档标注「仅内网/受信网络可用」）；发送 4xx/5xx 照旧 `NotifierError`（触发指数退避重试）；验签失败不吞——重试的是「网络失败」，签名错误（时钟漂移）通过 skew 容忍，配置错误记 WARN 日志（含渠道名，不含 secret）。
- 空 body 校验：必失败（常量时间比较仍执行，不短路）。

## 测试+变异点
- `VerifyWebhookSignature` table-driven：正确签名/篡改 body/过期时间戳/未来时间戳/坏版本/hex 畸形/常量时间路径；`WebhookNotifier.Send` 用 httptest mock 断言头存在且验签通过、secret 空不加头。
- 变异验证：① 签名算错（改 key/数据序）→ 校验红；② 时间戳检查缺失 → 过期用例红；③ `ConstantTimeCompare` 换成 `==` → 单测仍绿（可观测性上补注释，不强行造竞态测试——标注为已知局限）。

## 片划分
- 片1：`WebhookConfig` 扩展 + `Send` 签名头 + `VerifyWebhookSignature` 纯函数与单测（纯 pkg/server）。
- 片2：docs/notify.md（含 Go/Python 接收侧验签示例）+ config.example.yaml 注释 + 若 `notify.go` 内已有渠道测试则补 mock 断言。

## 风险与零回归
- secret 缺省 = 逐字旧行为（golden 头断言测试锁定）；签名格式带版本前缀（`sha256=...`）可平滑演进算法；不引入任何第三方依赖（crypto/hmac + crypto/subtle 标准库）；重试路径签名刷新不改变去抖/历史语义。
