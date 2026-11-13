<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 联邦卷写面与强一致性（11.3-③）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-fed-consistency.md` 已于 2026-11-13 归档。
> 配置键、取值与错误语义以 `docs/config.md`（§Federated 后端配置）为准（现行事实源），
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图

联邦卷适配层（`pkg/volume/federated`）此前只有只读读面；若注入写面则写操作**直接转发**，
冲突语义是最朴素的 LWW（最后写入者胜）。目标：**可选**强一致性模式——`version`（CAS）
与 `conflict`（保留双方），且默认 `lww` 零回归。

## 定案与取舍（仍有效）

1. **默认 LWW = 零回归**：模式未配置即直转写面，语义与「无冲突管理」一致（远端 LWW）。
2. **能力经可选接口探测，不做静默降级**：`version`/`conflict` 要求写面实现 `VersionedWriter`
   （`CheckVersion` + `WriteFileVersioned`）。写面缺失该能力 → **fail-closed 报错**，绝不悄悄
   退回 lww（安全开关必须可观测）。
3. **冲突消解只在写面做一次**：CAS 比较在**远端**原子完成（本层只编排「先查后写」）——
   TOCTOU 由远端原子比较消除，本层不做锁。
4. **`conflict` 模式宁可失败也不丢旧内容**：先原子改名旧文件保留双方，再写新内容；改名失败 ⇒
   整次写入失败（不写新内容）。冲突文件命名沿用 sync 既有惯例（`.conflict-<ts>` 后缀），
   因此可被既有冲突索引/API 消费。
5. **配置形态**：`extra.writable`（是否注入写面）+ `extra.conflict_mode`（`lww`/`version`/`conflict`，
   大小写不敏感，非法值启动报错）。
6. **权限边界**：`writable=false` 时写方法恒 `ErrReadOnly`（只读约束真正生效，而非「不拦但也不写」）。

## 外部契约

- `VersionedWriter`（写面可选能力）：`CheckVersion(ctx, path) (int64, error)`（`-1` = 不存在；
  错误 = 无法判定 → fail-closed）、`WriteFileVersioned(ctx, path, r, size, mtime, expected)`。
- 哨兵错误：`ErrReadOnly`、`ErrVersionedUnsupported`；版本不匹配以远端 CAS 错误表达（对应 409）。

## 落地指针

`pkg/volume/federated/federated.go`（`ConflictMode`/`ParseConflictMode`/`VersionedWriter`/
`WithConflictMode`/`WriteFile`）、`pkg/volume/federated/backend.go`（`extra.writable` 与
`extra.conflict_mode` 装配与 fail-closed 校验）。

## 实测差异（以代码为准）

- 冲突文件名实际是 `<path>.conflict-<unixNano>`（设计写的是 `.conflict.<unix-ts>`）——分段符与
  精度都变了；做法仍以 `federated.go` 为准。
- 设计「片2/片3」（远端侧 CAS 实现落地、sclient/CLI 的 409/conflict 提示、跨节点 e2e）**未在本片
  落地**：当前只保证卷层分派与装配期 fail-closed，远端 `WriteFileVersioned` 语义由远端写面提供。
- 设计称「version/conflict 需显式配置才改变语义」——成立；但注意 `writable=true` 本身就会把写面
  接管（不再有「只读卷写出 ErrReadOnly」的兜底），这是本片引入的行为变化。
- **关联权威文档不一致（未在本任务修正）**：`docs/federation.md` §3/§5 仍写「写方法恒 `ErrReadOnly`/
  写面是后续片」——该表述已被 `extra.writable` 写面取代，以 `docs/config.md` 为准。
