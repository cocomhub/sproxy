<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 剩余装配根的优化设计（P2）— 归档

> **归档说明**：原文 `docs/designs/2026-11-01-residual-assembly-optimization.md` 已于 2026-11-14
> 归档。**T1/T2/T3 全部落地**（#994–#997）。现行事实源见 `docs/architecture.md` 的「T1 跨节点读写面」
> 「T3-a 凭据存储与主密钥解析」「T3-b 封装卷翻译与运行期装配」「横切能力：`internal/httpjson`」
> 「P2 总体度量」与 `AGENTS.md` 能力矩阵（`pkg/server/remotefs` / `internal/httpjson` /
> `pkg/server/credentials` / `pkg/server/enclosure`）。本文件只保留**切片判据 / 明确不做 /
> 实施中与原设计的偏差 / 仍有效的取舍**。

## 切片判据（可执行，取代「凭感觉切」）
**切片门槛**（三者全满足）：① 去重 `h.<字段>` ≤ 8（`Deps` 能压到 ≤8）；② `h.` 密度 ≤ 80 次/千行；
③ 有单一领域名（能一句话说清职责）。
**不切门槛**（任一满足即留装配）：① 是路由注册/能力面转发（`routes*`/`handlers.go`/
`files_service.go`）——切了只会把 `Handlers` 能力面复制一份，耦合上升；② 依赖 `ActorFrom`/
`sendJSONResponse` 等装配层语义超过 5 处（P1-c 的 cloudtasks 教训：HTTP 语义留在装配，领域核心进包）。

## 三条轨道（设计意图，已落地）
- **T1 `pkg/server/remotefs`（跨节点读写面）**：`remote_read/write + 两个 listener` 是一个完整
  协议面。核心不变量：**读写两面刻意反向同构**（同一套授权三重约束、同一套错误映射 401/403/404/
  409/500、同一套审计落点），放在一起才能守住；只读面物理上只注册 GET/HEAD，写面只注册 POST
  （非黑名单法）；**写面只做授权 + 调 `pkg/files` 域方法**，不复制 checksum 门禁/原子改名/版本/
  配额/文件锁/卷路由；远程写与本地写**共享同一锁池**；未装配 ⇒ 全部 no-op（单节点零回归）。
  `Deps` 恒 8 字段（`NumField()==8` 断言）。
- **T2 `internal/httpjson`（统一 JSON/错误响应）**：`sendJSONResponse` 全仓 337 处调用，且
  `ratelimit` 已因无法反向 import 复制了一份 429 回包。一期做 L0 包 + `ratelimit` 复用 + 门禁基线；
  二期按文件批量迁移（**棘轮只减**）。**字节契约必须逐字保持**（成功路径无尾随换行；序列化失败
  → 500 + 固定错误体，**带**尾随换行）。
- **T3 `credentials` / `enclosure`**：启动路径与 schema 耦合，放最后且各自独立 PR；
  `TranslateNestedVolume` 是**用户面语法 → 装配词汇**的翻译点，切片**不得改变翻译语义**。

## 明确不做
| 项 | 理由 |
|---|---|
| 继续切 `routes*`/`handlers.go`/`files_service.go` | 装配本体；切了要复制 `Handlers` 能力面（30+ 依赖），耦合上升 |
| 把 `usage_store`/`user_volume_store`/`cluster_index_sync` 合成一个包 | 三者无共同领域语义，合成会造出垃圾桶包 |
| 拆 repo / 拆进程 / build tags | 已在前序设计否决（23 个 go.mod 已提供隔离；CI 组合爆炸） |

## 实施中与原设计的偏差（保留，供后人理解现状）
1. T2 一期发现第 **4** 份 JSON 响应实现（`cloudtasks.writeJSON`），设计时只识别出 3 份；
2. T1 的 `federation_services.go` 是 `h.` 绑定的 HTTP handler，留装配层（设计已预判）；
3. T3-b 的 `volumeFromUserVolume` 移到 `pkg/server/user_volume_store.go` 而非新包——它转换 store
   模型（装配类型）→ `volume.Volume`，进包即反向依赖；
4. T3-a 按风险收窄为「只切无 `h.` 依赖的纯子集」，启动编排与轮换驱动留装配层。

## T2 二期（已完成）行为微调（错误路径）
为「无功能丢失」补了三项能力（`SendOpt`/`SendErr`/`WriteBody`）；两处行为微调均在**错误路径**：
① 裸写用 `json.Encoder`（带尾随换行），`Send` 不带；② 序列化失败从「200 + 空体」变为
「500 + 固定错误体」。

## 结果
`pkg/server` 非测试 **42,568 → 28,125（−33.9%）**；服务端子包 **1 → 19**；R54 由棘轮升级为**硬禁止**
（基线表删除）；剩余行数按切片判据已到自然边界。
