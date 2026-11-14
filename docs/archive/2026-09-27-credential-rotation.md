<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 运行中凭据自动轮换（Credential Auto-Rotation）— 归档

> **归档说明**：原文 `docs/designs/2026-09-27-credential-rotation.md` 已于 2026-11-14 归档。
> 配置字段/默认值与用法见 `docs/config.md` §`credentials.rotation.*`、`docs/cli.md` §trust、
> `docs/api.md` §`POST /api/credentials/{ak}/renew`（现行事实源）；本文件只保留**设计意图 /
> 取舍 / 仍有效的判断**。

## 背景与目标
SproxySig 凭据（AK + SK + skey-id）有有效期（服务端 `credential_ttl`，默认 30d）。常驻 sclient 进程
（http-proxy / socks / mesh connect / mesh up / mesh node / relay start / p2p listen）在 SK 到期前
需**手动 `trust renew` + 重启进程**——断连秒级且依赖人工。目标：常驻进程运行中自动轮换 SK，
**新 SK 热替换到签名器**（同进程立即生效，无需重启）。

## 服务端前提（已支持）
- **多 SK 共存**：一个 AK 可有多条 SK 条目（新 SK 生效，旧 SK 在宽限期内仍可用 ⇒ 不踢在途连接）。
- **renew API**：`POST /api/credentials/<ak>/renew`——用当前 SK 签名调用，服务端生成新 SK（信封加密
  wrap 返回），客户端用旧 SK 解信封得新 SK 明文 + 新 skey-id。
- **wrap 语义（安全关键）**：信封密钥 = `HKDF(旧SK, AK, mesh)` ⇒ **持旧 SK 才能解新 SK**，防未授权轮换。
- 服务端可选调度（`credentials.rotation.interval > 0`）：服务端到期前自动 renew 并存入 Ring，但
  客户端不知新 SK ⇒ **客户端仍需主动 renew 才拿到新 SK**（服务端调度不能替代客户端轮换）。

## 客户端设计（实现指针）
- **凭据热替换**（`pkg/client`）：`FileClient` 以 `credentialsMu`（RWMutex）保护 AK/SK；`RenewAccessKey`
  调 renew API 后**走锁回填**字段；`credentialsSnapshot()` 是签名点读一致快照 ⇒ **renew 后无需重启，
  后续请求即用新 SK**。
- **统一轮换工具**（`cmd/sclient/internal/credrotate`）：`Start(ctx, svc, Options{Interval, Logger, OnRotate})`
  ——`Interval <= 0` 或 `svc nil` 不启动；每 `Interval` 调 `svc.RenewAccessKey` 并 `OnRotate(newSK, newID)`
  回调热替换；`Options.OnRotate` 把新 SK 热替换到调用方的动态凭据容器。
- **各常驻场景接入**：动态凭据 provider（`NodeConfig.Credentials func() (ak,sk,id)` 每重连取最新 /
  `f.creds` 容器 / `creds.Get()`）——`nil` = 回落静态字段，未接入调用方零改动。

## 关键语义（仍有效）
1. **幂等安全**：renew 后旧 SK 宽限期仍可用（不中断在途）；轮换失败记 Warn 下次重试（`continue`，
   不中断常驻）；`credentialsMu` 保证 renew 与签名并发安全。
2. **向后兼容**：动态 provider 为 nil 时回落静态字段；`--renew-interval` 默认 24h，`0` = 关闭。
3. **动态 provider（mesh node）**：AutoRegister 每次重连（断线退避循环）取最新 SK ⇒ 轮换后下次重连
   即用新 SK，无需重启。

## 深化项（roadmap 12.2-2 指明，**刻意未实施**）
| 项 | 内容 | 取舍 / 理由 |
|----|------|-------------|
| **在途请求 drain** | 轮换时用旧 SK 签名中的在途请求是否需等存量完成再切 | **不做**：旧 SK 宽限期已保证不失败，drain 语义无实测收益 |
| **跨实例协调** | 多副本同一 AK 同时轮换 → 各产生多条 SK（服务端 `keep_old` 裁剪） | **不做**：服务端 `keep_old` 已部分缓解条目膨胀；真需协调时应走 leader 选主（避免各自 renew） |

## 验证指针
- `TestFileClient_RenewAccessKey`（黑盒：renew 后请求用新 SK 验签、旧 SK 验不过）+ credrotate 各场景测试
  （flag 断言 + 编译级）；生产验证：常驻代理带 `--renew-interval=24h` 观察到轮换日志
  `msg=凭据已自动轮换（新 SK 热替换生效）`。
