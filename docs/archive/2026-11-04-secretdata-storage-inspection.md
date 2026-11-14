<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# secretdata 真实存储审查能力（`volume trace` / `volume explain`）— 归档

> **归档说明**：原文 `docs/designs/2026-11-04-secretdata-storage-inspection.md`（416 行）已于
> 2026-11-14 归档。**P1 已全部落地**。现行事实源见 `docs/architecture.md`「真实存储审查（逻辑 ↔
> 物理落点，2026-11-04）」、`docs/api.md`（`GET /api/volumes/{name}/trace|explain` + 差异码表）、
> `docs/cli.md`（`sclient volume trace|explain`）与 `AGENTS.md` 能力矩阵（`StorageInspector` 行）。
> 本文件只保留**目标/非目标 / 分层落位 / 安全取舍 / 实测踩坑 / 未决取舍**。

## 目标 / 非目标（非目标是硬边界，仍有效）
- 目标：① 正向 `Trace(rel)`（逻辑 → 容器链/meta/分块/parity/去重池）；② 反向
  `Explain(realRel)`（真实路径 → 用途与逻辑归属、是否孤儿）；③ `--disk` 差异审计（结构化差异码）；
  ④ 能力由**能力包**提供、HTTP/CLI 只做薄封装（远端卷天然支持，客户端不重实现布局解析）；
  ⑤ 人类可读树 + 稳定 JSON（`schema_version`）+ 退出码可做脚本门禁。
- **非目标**：**不解密内容**（只解 meta）；**不做修复动作**（删除孤儿/重建 meta/搬迁容器，修复走
  既有 `volume refresh`/GC）；P1 只覆盖 secretdata（其它卷型「未实现 = 不支持」）；**不承诺索引与磁盘
  强一致**（本能力正是用来**暴露**不一致）；不做 Web UI（下一批）。

## 分层落位（布局知识不得在装配层/CLI 复制）
`pkg/volume` 定义契约（`StorageInspector` = `Trace`/`Explain`；后续追加可选 `TreeInspector`），
`pkg/volume/secretdata` 是**唯一布局实现**；`pkg/server`（两条只读路由）+ `pkg/client` +
`cmd/sclient volume trace|explain` 只做薄编排。

## 安全取舍
- **路径穿越**：反解为卷根相对 + 显式拒绝越界（`ErrOutsideVolume` → 403）；**禁止**静默
  `filepath.Clean` 后继续。
- **越权读布局**：路由 `ClassReader`（与 `/api/volumes/export` 同级）；不新增公开面。
- **明文泄漏**：只解 meta、不读内容；日志只记 rel/blob 名；不写临时解密文件；走卷装配期已注入的
  secret。
- **输出不得被当作删除依据**：文案与 JSON 均标注「只读审计，不构成删除建议」；退出码 3 仅表示差异
  存在。**`blob_orphan` 不等于可安全删除**（GC 有 reconfirm 流程与 crash 窗口）——历史 P0 教训：
  绕过 Root/卷语义直删。

## 实测踩坑（仍有效，避免重走）
1. **容器内的目录条目 = 子容器**：根容器里除 dir-meta/分块外还有子容器目录；把目录当 blob 判定
   ⇒ 每个子目录都被报 `blob_orphan`（0 B），结论严重失真。⇒ `RealEntry.Kind` 增 `container` 并带
   `Container` 归属。
2. **索引键空间是 `<owner>/<bucket>/<rel>`**，而调用方只见 `rel`：直接把用户路径当键查 ⇒ **全部
   404**。⇒ HTTP 层用 `Volume.ResolveUserLocation(owner, path).FSPath()` 映射（空 path 卷根不映射）；
   `explain` 的 `real` 仍按**区域相对真实路径**解释（两种键空间不可混用）。
3. **上传窗口内的可见性**：原实现只有「索引有/没有」两态 ⇒ trace 404、explain 报孤儿。补**进程内
   在途上传注册表**：`trace` 给 `mode=uploading` + 进度（退 0）；`explain` 给
   `kind=upload_in_progress`（**不是** `blob_orphan`）。边界：注册表是**本进程**内存态；跨实例上传
   仍表现为未引用 blob（blob mtime 刻意打散，**不做时间猜测**）。
4. **对抗性审查 6 处修复**：① 分块判型**不得**先 `readBlob`（单块可达 ~118 MiB）；② 不得用
   `context.Background()`（须透传请求 ctx）；③ 在途 **file-meta**（已上传、索引未提交）须判
   `upload_in_progress`；④ 退出码**只对真实差异码**退 3（两种输出模式共用判定）；⑤ 容器解释须**一次
   遍历**构建 `containerIndex`（否则 O(容器条目 × 全卷条目)，大卷分钟级 + 锁抖动，R40 登记为
   「单请求派生视图」）；⑥ 上传注册表须**按令牌**（同 rel 并发上传时先结束的一路会删掉另一路记录）。
5. **递归树性能（重要）**：`trace --depth 2` 若子项走完整 `Trace`，每个子目录都为 dir-meta 做一次
   ListDir ⇒ 远端卷数百子目录实测 **3m36s**；改为 `treeChildren` 单次 `ListDir` + 逐项
   `traceChildIndexOnly`（**纯索引、零后端调用**）后只保留根节点 1 次 ListDir（上限
   `MaxTreeDepth=3`/`MaxTreeNodes=200`；`disk` 只核验根节点）。

## 未决取舍（原文 §11，实施已按此默认执行）
① 路由用 `ClassReader`（非更严的 `ClassAuth`）；② `explain` 接受卷根相对路径（绝对路径由 CLI 剥前缀）；
③ 不实现删除/修复子命令；④ `--disk` 有差异退 3；⑤ P1 仅 secretdata，其它卷 501。
风险：索引未加载/陈旧时输出误导 ⇒ 始终带 `mode` + `Evidence`，`blob_orphan` 文案标「疑似」并指向
GC；巨型容器截断 + `truncated`；`dir_meta` 解密失败 ⇒ `meta_unreadable`（不中断整次 explain）。

## 落地指针
`pkg/volume/inspect.go`（契约）、`pkg/volume/secretdata/inspect.go`、`pkg/server/volume_inspect.go`、
`pkg/client`、`cmd/sclient/volume_inspect.go`。
