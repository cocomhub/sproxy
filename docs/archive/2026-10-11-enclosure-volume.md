<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 封装卷（Enclosure Volume：目录封装 + 封印 + 配额继承）— 归档

> **归档说明**：原文 `docs/designs/2026-10-11-enclosure-volume.md` 已于 2026-11-14 归档。
> 能力与用法见 `docs/config.md`（`volumes[].type` wrapper + `extra.source`/`extra.target`/`extra.wrapper`）、
> `docs/secret-volume.md` §3.8（嵌套封装）、`docs/architecture.md`（封印 / R26 / R27）。本文件只保留
> **命名定案 / 装配契约 / 真值修正机制 / 踩坑 / 仍有效边界 / 落地指针**。

## 1. 定位（正交边界）
封装卷 = 把已装配卷的**根或子目录**占为已用，经**封装处理器（wrapper）**提供变换后的访问
（加解密/镜像/压缩/脱敏…），并在**源卷侧封印**该区间（直连写恒拒、读可配 404/只读透视）。
与 secretdata/trusted/配额层**职责不重叠**：wrapper 只改字节，封印由源卷注入、配额由装配层包装。

## 2. 命名定案（写入代码注释与文档）
- **封装卷（enclosure volume）**：卷类型，承担声明 / 源视图 / 封印 / 配额继承。
- **封装处理器（wrapper）**：可注册的处理层，**只改字节**（不承担封印与配额）。
- **封印（seal）**：源卷侧对封装区间的直连拒绝。
- 曾考虑 **抽象卷（abstract volume）**（❌ 语义过载、不表达关键约束、Go 语境反面模式）与
  **托管卷（custodian）**（△ 弱化围栏语义）；定案 **enclosure**（同时覆盖「包装」与「围栏」两职责）。

## 3. 分层与装配契约
- 声明与解析：`pkg/volume/enclosure` 的 `Decl`（`Wrapper/SourceVol/SourcePath/ReadMode/AllowDirectURL/MaxBytes`）
  + `Parse`（环检测/深度/唯一性校验 fail-closed）+ `SealRules()`/`SourceKey(owner)`。
- 视图薄层：`enclosure.FS`（组合 `PrefixFS`，**不复制视图逻辑**）——默认**抑制 DirectURL**、
  透明转发 `MetaExtraUpdater`、携带身份（错误归因）、R21 登记。
- 处理层注册表：`Wrapper` 接口 + `RegisterWrapper`（处理层可插拔，外部/未来卷类型接入只需实现并注册）。
- 封印内核：`pkg/storage.SealRule` + `Root.SetSeals`（写恒拒；读 hidden/readable）。
- **装配链**：`wrapper(EnclosureFS(PrefixFS(源)))`——`Decl.Build` 须把 `NewFS(src,d)`（薄层）交给 wrapper，
  而非裸 `PrefixFS`（否则 `allow_direct_url` 策略形同虚设、裸 PrefixFS 无条件转发直链 ⇒ 客户端可绕过封印）。
- **声明形态唯一**：wrapper 类型 + `extra.source`；用户面推荐嵌套语法（wrapper 类型 + `extra.target: "<卷>/<子目录>"`，
  装配层翻译）；`volumes[].type: enclosure` **不是可写类型**（fail-closed 拒绝）。

## 4. 真值修正机制（让「知道真实占用的卷层」修正账本）
逻辑记账对会改变物理落盘量的卷层必然失真（实证：at-rest 加密卷 `1GiB` 限额实际可落 ~1.5TiB——
meta pad 不入账 + 外部/封装卷 `RootDir` 为空被对账扫描跳过 ⇒ 无自愈）。机制 = 「报告 + 对账 + 交叉校验」：
- `TrueUsageReporter{TrueUsage, TrueSize}` 由**掌握物理布局的那一层**（后端/封装处理器）实现，
  **禁止装饰器估算**；`ok=false` ⇒ 回落既有记账，**绝不静默降级为 0**。
- **`TrueUsageOf` 自外向内返回第一个实现者**（掌握物理的是**处理器层**，链中间/外层；最内层只是被包装的底层）。
- 修正只动 `committed`（沿父链聚合、防下溢），**不触碰在途 `reserved`**；真值优先于上限（可暴露超限但**不截断**）。
- 双源交叉校验：磁盘扫描 / backend `VolumeStatsProvider` 与报告者并存时差异超阈值告警，以真值为准；
  修正后下一次预留即按真值判定 ⇒ 账本失真不再能兑换成超额写入。
- 封装卷配额经 `quota.NewPoolWithParent` **继承源卷**（不双计）。

## 5. 踩坑（最可复用）
- **`RegisterWrapper` 在生产侧零调用**（仅测试注册替身）⇒ `enclosure.Parse` 对**任何** `type: enclosure`
  配置恒 `ErrUnknownWrapper`，`AttachAll` 失败被当 boot fail ⇒ **文档承诺的 `wrapper: secretdata` 根本起不来**
  （封装卷只能在测试里用替身跑）。修：`cmd/sproxy` 新增 `registerEnclosureWrappers()`（在 `enclosure.Parse`
  之前调用），并让 `Decl.Build` 走 `w.Build(ctx, v, NewFS(src,d), extra)`。
  ⇒ 教训：**「文档/示例承诺的配置形态」必须有生产注册闭环保住**，否则功能只在测试里存在。
- **封印此前只有文字承诺、无门禁** ⇒ DAV 面/备份源/同步本地侧三处 P0 都靠逐次评审才发现。
  现由 **R26**（`volume_root_bypass_gate_test.go`：非测试源码 `sync.NewLocalFS(...)` 每个调用点须登记理由）
  + **R27**（`external_fs_gate_test.go` + `external_fs_calls.tsv`：登记全部 `be.FS()` 直连点，只降不升）机械保证。
- **封印比较的 Unicode/8.3 归一**（Windows 实测）：`secret.txt.`（尾点）/ 尾空格 / 中间段尾点 / 全大写
  均可达同一文件 ⇒ 归一键逐段去尾点/空格 + 大小写不敏感；补 **Unicode NFC 归一**（macOS 缺口，ASCII 恒等无回归）；
  8.3 短名由装配期别名展开（`GetShortPathNameW`）处置。**残差**：macOS NFC 与启用 8.3 的卷上的别名展开待该环境实测。

## 6. 仍有效边界（记档）
- **外部卷源的 per-tenant 视图**构造在 S4 之后；在此之前外部源装配期**显式拒绝**（禁静默降级）。
- 封印只覆盖经 `storage.Root` 与注入检查的视图；任何**新增直连 os I/O 的面**都必须同时接入封印检查。
- 迁移场景的「只读透视」会暴露原始存储形态（密文）——显式取舍（便于卷迁移/带外校验）。
- `Decl.SourceKey(owner)` 未以该名字落地（语义已由「owner 限定声明的两个视图 + `Decl.Footprint()`」实现）；
  `type: secretdata` 独立卷与 `wrapper: secretdata` 并存（共用实现，未做旧写法下线）。

## 7. 落地指针
- `pkg/volume/enclosure/`（`enclosure.go`/`attach.go`/`sealfs.go`）、`pkg/server/enclosure`（翻译与装配编排）、
  `pkg/storage/seal.go`、`cmd/sproxy`（`registerEnclosureWrappers`）、`pkg/volume/capacity/phys.go`（真值）。
