# 统一存储卷模型：按用途封装的本地卷 + 唯一卷访问入口（归档）

> 精华归档（docs/archive 按功能维度）：2026-10-28 落地（docs/designs/2026-10-28-unified-volume-model.md
> 的 U1-U5 + docs/architecture.md/api.md/cli.md 收敛）。本归档保留**跨时间有效**的判断，过程细节
> 以 git 历史为准。

## 背景

本地卷长期是**特例**：`*storage.Root` 双轨（254 处）、`assembleLocalVolume` 裸 OpenRoot、
转存目标只认外部卷（本地卷 400）、`volume://` 与转存 URL 语义分叉。M0/M1 已定义
`StorageVolume` 契约与 `RegisterLocalBackend` 但**装配未接线**——「注册了却没接上」的
半成品根治形态。

## 做了什么（统一卷模型的六层收敛）

1. **唯一身份**：`registry.Set.storageVolumes map` + `RegisterStorageVolume`/`StorageVolume(name)`
   ——本地 `localBackend` / 外部 `storageVolumeOf`（提升补 Kind）同表；AddExternal/Attach/Remove 同步维护。
2. **唯一取卷入口**：`Set.VolumeView(name, use)`——同一卷按用途返回不同装饰链视图
   （`user` = Guard→Wrap(trusted)→capacity→staging；`meta` = Guard）。
3. **装配接线**：`assembleLocalVolume` 改走 `NewLocalBackend`（复用 OpenLocalVolumeRoot，零重复）；
   `roots` 保留仅服务默认卷 tenant/globalRoot 存量语义。
4. **转存支持本地卷**：preflight 只要求 FS 可解析；scheme 空按 `local` 归一；
   产物落 `<owner>/user/<rel>`（ResolveUserLocation 统一键空间，/download 命中）。
5. **local:// 协议**：`RegisterLocalBackend(..., "local")`；`ResolveURL` 经 VolumeView 读本地卷，
   路径安全由 ResolveUserLocation 唯一入口承担。与 `volume://`（卷内定位序列化）并存分工。
6. **门禁收缩**：R27 直连点随 ExternalVolume/ExternalDeleter 迁 VolumeView 减少；R28（Set.Root）
   预算**维持不缩**（逐点核实 12 处全需 Root 方法——见下）。

## 踩过的坑 / 关键判断

1. **decorator 必须内置默认（trusted 装饰链），不能只靠装配层注入**：
   测试 fake 卷集不注入 decorator ⇒ VolumeView 返回裸 FS ⇒ 丢 meta sidecar 生成/校验、
   Delete 联动 sidecar+记账（6 测试当场红）。registry 可 import trusted（无环），
   内置默认后生产/测试行为一致。**「装配期注入」的隐式契约 = 漏注入即行为漂移**。
2. **R28 预算「能迁的迁完」与实际不符**：12 处 `Set.Root` 全部真需 Root 方法
   （Abs/SealAt/Stat/ListOwners/OpenFileEncrypted）——写内核原子写/加密语义只在 Root 完备，
   不把 32 方法塞进 StorageVolume 接口。统一的是**身份与访问入口**，不是数据面重写。
3. **AddExternalVolume/AttachExternal 也要同步 storageVolumes 表**（不只 NewSet）：
   fake set 测试路径用 AddExternalVolume，漏登记 ⇒ VolumeView 对已装配外部卷返回 !ok。
4. **staging 是 per-owner 瞬态**：不进装配期装饰链，由消费方 `ApplyStagingQuota` 取值时套
   （VolumeView 无 owner 维度）。
5. **secrets 视图不入通用表**：`VolumeView` 拒绝 `secrets` use（R26 白名单维持）——
   密钥视图只在装配期按需构造。

## 收益

- 本地/外部卷**同形**：新后端只需实现 5 方法（Name/Kind/FS/Close + 能力）。
- 转存/读/删路径**单一 recipe**（VolumeView）——不再「一处可信卷、一处裸卷」。
- R27 直连点随迁移收缩；未来新调用点经 VolumeView 拿装饰后视图，不可能裸 FS。
- 容量记账对本地卷同样在 FS 层（装配期套 capacity），不漏接。
