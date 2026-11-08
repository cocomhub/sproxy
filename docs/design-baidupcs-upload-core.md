# 设计：BaiduPCS 自研分片上传内核（复用 audit WAL + recovery）

> 状态：**待评审**（2026-11-08）。目标文件：`pkg/volume/ext/baidupcs/uploadcore/`
> 关联：`docs/external-dependencies.md` §7.8.1（90s 实测）、PR #1056（库上传 fail-closed）

## 1. 问题与目标

### 问题（行级证据）
嵌入式调用 fork 库的 `uploader.MultiUploader` 时，**上传不可取消、耗时可到 ~90s**：
- `requester/uploader/multiworker.go`：失败分片**零退避、无次数上限**重入队（我们只能在 `TmpFile`
  里用 `*uploader.MultiError{Terminated:true}` 兜底终止，`multiupload.go:maxPartAttempts=6`）；
- 库自建 client 超时不可注入：`pcsupload/upload.go:37 uploadClient.SetTimeout(200s)`、
  `uploader/uploader.go:63 SetResponseHeaderTimeout(10s)`；
- 库的退避是 `time.Sleep`（`taskframework/executor.go:135` + `pcsfunctions/common.go:8-13` **不听 ctx**）；
- 结果：`ctx` 取消**只能**通过我们自己的 channel 绕行，且库内部任何等待都无法打断。

### 目标
1. **可取消**：`ctx` 取消 ⇒ 在途分片请求立即终止，最坏在 `请求超时` 内退出；
2. **可观测**：每个阶段/分片有打点与审计事件（能回答"卡在哪、重试了几次"）；
3. **可恢复**：进程崩溃/重启后**续传**（不重传已完成分片），最终**幂等**；
4. **可验证**：协议层可被 `httptest` 锁定（CI 可跑），不依赖真实网盘；
5. **复用已有能力**：断点用 `pkg/audit` 的 WAL，启动恢复用 `pkg/recovery` 的 Coordinator——
   **不新造**持久化/恢复框架。

### 非目标
- 不改动 PCS 协议本身；不新增第三方依赖（除已有 fork 库的**单请求原语**）；
- 不替代 `pkg/client/chunked.go`（那是 client↔sproxy 的分块上传，与 PCS 无关）。

## 2. 现状（本仓库已有资产，全部复用）

| 资产 | 位置 | 复用方式 |
|---|---|---|
| 三个 HTTP 动作（**已由我们自己发**） | `multiupload.go`：`Precreate` / `TmpFile`（`pcs.UploadTmpFile`）/ `CreateSuperFile`（`pcs.UploadCreateSuperFile`） | 保留调用，**替换其外的编排** |
| 分片失败终止判据 | `shouldTerminatePart`（400/401/403/413 + 次数上限） | 保留语义，移入内核重试策略 |
| 断点文件 | `Layout.Resume` + `saveUploadResume/loadUploadResume` | **升级为 audit WAL**（见 §4） |
| WAL 原语 | `pkg/audit`：`Service.Resume(ctx, ScopeID, ReplayApplier)`、`MarkDone`、`Entry`(state record) | 每上传会话一个 `ScopeID` |
| 启动恢复 | `pkg/recovery`：`Coordinator.Register(Registration)` + `Run(ctx, Options)` | 注册 `upload` 类型的 applier |
| 秒传刷新 | `pkg/sync` `RapidRefresher` / `BatchVerifier` | 上传后校验走同一套 |
| GCID/分片校验 | `pkg/integrity`（`GCIDOrderFor`/`MatchAnyGCID`） | 上传后数据校验（不写死块大小） |

**关键事实（决定流程）**：上传会话 `uploadid` **由 rapidupload 响应给出**
（`pcsfunctions/pcsupload/upload_task_unit.go:281-283`：`state.Uploadid == "" ⇒ = jsonData.UploadID`），
即 **必须"先秒传（拿到 uploadid 或直接完成）→ 再并发上传分片 → 再 createsuperfile"**。

## 3. 架构

```
uploadcore.Uploader （新内核，唯一编排者，全程 ctx 可控）
   ├─ Stage 1  probe     : 列举父目录（精确列举，0.33–0.38s）→ 目标已存在且同 md5 ⇒ 幂等完成
   ├─ Stage 2  rapid     : pcs.RapidUpload(...) ⇒ {uploadid | 已秒传成功}
   ├─ Stage 3  slices    : 并发 worker 池，逐分片 pcs.UploadTmpFile(...)，每片 md5 记录
   ├─ Stage 4  merge     : pcs.UploadCreateSuperFile(uploadid, policy, size, checksumMap)
   └─ Stage 5  verify    : 列举/Stat 取 md5 校验（数据操作必须 md5 确认）+ 秒传刷新自愈
```

### 状态机（每个上传会话）
```
pending ──probe──▶ (exists&same) ──▶ done
   │
   ├──rapid──▶ (秒传命中) ──▶ done
   │        └─▶ uploading(uploadid, partDone set)
   │                 ├─ slice ok ──▶ partDone[seq]=true   (WAL 记录)
   │                 ├─ slice fail ─▶ retry(≤N, 退避) ──▶ 超限 ⇒ failed(可重试续传)
   │                 └─ ctx done ───▶ canceled(保留 uploadid + partDone)
   └──merge──▶ done ──verify──▶ verified | damaged(⇒ 重传/秒传刷新)
```

## 4. 复用 audit WAL + recovery（**能力复用，不新造**）

### 4.1 WAL 记录（每上传会话）
- `ScopeID`：`upload:<volume>:<sha256(目标路径)>`（确定性 ⇒ 崩溃后能定位同一会话）；
- `Entry`（state record）写入时机：
  - `stage=rapid` → `{uploadid, file_md5, file_size, block_size}`；
  - 每分片成功 → `{part_seq, part_md5}`（**逐片持久化** ⇒ 崩溃最多丢在途片）；
  - `stage=merge` 成功 → `MarkDone(ctx, scope)`（完成后 scope 收口，不再参与恢复）。
- 复用 `audit.Service.Resume` 回放得到 `ScopeState`（`uploadid` + 已完成分片集合）。

### 4.2 启动恢复
- 在装配层注册：`recovery.Coordinator.Register(recovery.Registration{Kind: "upload", Scope: ..., Factory: applierFactory})`；
- `ApplierFactory` 返回的 `ReplayApplier` 只做**状态重建**（把 `partDone` 集合与 `uploadid` 装回
  内核），**不在恢复期发请求**（避免启动风暴）；
- 由后续业务写入路径（或显式 `Run`）决定是否续传；`recovery.Run` 的 `Report` 用于启动日志与指标。

### 4.3 幂等
- 会话恢复后：`uploadid` 有效则**跳过 probe/rapid**，直接补传缺失分片；
- `precreate/upload` 的语义允许同片重传（同 `partseq` 覆盖）⇒ 补传天然幂等；
- 合并前校验 `checksumMap` 完整；合并后 `verify` 阶段用 md5 对比，不一致 ⇒ 秒传刷新自愈
  （`pkg/sync.BatchVerifier` 既有契约：发现摘要不一致 ⇒ 后端自己先秒传刷新并复验）。

## 5. 高可用与稳定性设计

| 维度 | 设计 |
|---|---|
| **取消** | 每分片请求带 `ctx`；`req.WithContext` + 连接层关闭；内核 select 监听 `ctx.Done()`；**最坏退出时间 = 单请求超时**（内核自持，不由库决定） |
| **超时** | 内核自建 `http.Client`：`ResponseHeaderTimeout`、`Timeout`、`TLSHandshakeTimeout` 全部显式配置；**分片级**超时（默认 60s）与**整体**预算（默认 30m） |
| **重试** | 每片 ≤N 次（默认 4），退避 `1s/2s/4s/8s` + 抖动，**ctx 可中断**；4xx（400/401/403/413）立即终止；`31066`（会话失效）⇒ 重走 rapid 拿新 uploadid |
| **并发** | 默认 4（可配 1–16），分片 4MiB（PCS `superfile2` 约束）；限流器（令牌桶）防 31034 风控 |
| **背压** | 通过 `pkg/waitq`/`quota` 已有排队能力约束同卷并发上传（沿用既有 WAL 恢复路径） |
| **大小文件分流** | `< 分片大小` 走单请求上传（`superfile2/upload`），跳过 rapid/merge 组合开销 |
| **多实例** | 同一目标路径的上传经 `FileLocks`（既有）串行；会话 `ScopeID` 确定性 ⇒ 两实例恢复同一会话时由锁裁决 |
| **可观测** | 阶段打点（复用 `logPhase` 风格）+ 指标：`upload_stage_seconds{stage}`、`upload_slices_total/retried`、`upload_cancel_total`、`upload_verify_mismatch_total` |
| **回退** | 配置 `baidupcs.upload_core = auto\|nucleus\|legacy`（默认 `auto`：内核优先，失败一次后本进程降级 legacy 并告警；`legacy` 保留现有库编排路径） |

## 6. 测试策略（三层，全部可 CI 或按需）

| 层 | 内容 | 能否 CI |
|---|---|---|
| **L1 单测** | 状态机（幂等/续传/取消/重试上限/退避可注入）、WAL 记录与回放、`checksumMap` 组装 | ✅ |
| **L2 契约** | `httptest` 假百度：rapidupload/upload/createsuperfile 的**请求形状与响应处理**（含 `31066`、`31061`、`31079`、限流 429/31034）、**取消时立即中断**、**重试确实发生** | ✅（本设计的**验收关键**） |
| **L3 e2e** | 真实网盘：`TestE2E_SecretdataOverBaidupcs`（数据路径 md5） + 探针 P9（内核阶段耗时/取消时延） | ⚠️ 手动/定时 |

> L2 的注入通道：`NewPCSWithClient` + 自定义 `RoundTripper` 重写 host 到 `httptest.Server`。
> **已知障碍**：库在发出 HTTP 前有会话/uk 前置检查（实测本地直接报 31045）⇒
> **内核自持 HTTP 客户端**（不经库的 client）可绕开该障碍——这也是采用内核的额外收益。

## 7. 分阶段实施计划（每阶段 TDD + 变异命中）

| 阶段 | 内容 | 产出/验收 |
|---|---|---|
| **S1** | 内核骨架 + 状态机 + 注入点（HTTP/时钟/退避/waitq） | L1 全绿；`httptest` 能驱动完整流程（L2 起步） |
| **S2** | WAL 集成（`audit.Service` scope per upload + `Resume` 回放） | 崩溃恢复用例（kill 后重建 → 只补缺失片） |
| **S3** | `recovery.Coordinator` 注册 + 装配层接线 | 启动恢复 Report 可见；不追加启动网络风暴 |
| **S4** | 秒传优先 + verify + 秒传刷新自愈（复用 `BatchVerifier`） | 摘要不一致 ⇒ 自动修复用例 |
| **S5** | 灰度开关 + 指标 + 文档（收敛到 `docs/architecture.md` / `external-dependencies.md`） | `upload_core=legacy` 回退演练 |

## 8. 风险与对策

| 风险 | 对策 |
|---|---|
| PCS 协议细节（`uploadid` 复用/`partseq` 语义）判断错 | L2 httptest 按**真实抓包形状**锁定；L3 探针定期校准；错误码 `31066` 触发重建会话 |
| 分片 md5 在 4MiB 约束下与 PCS 校验不符 | 与 `pkg/integrity` 同源（不写死块大小）；L3 用真实网盘校验 |
| WAL 写放大（大文件分片多） | 逐片一条记录 + 段合并（复用 `pkg/audit` 已有归档）；分片数上限（>2 万片拒绝并提示） |
| 双路径并存导致行为漂移 | 开关 `upload_core` 单一判定点；`legacy` 路径仅在显式配置时启用，且带告警 |
| 风控/限流 | 令牌桶 + 4xx 立即终止 + 指数退避；命中风控即降级并记录 |

## 9. 已确认决策（2026-11-08，人工确认）

| # | 决策 | 结论 |
|---|---|---|
| 1 | **`uploadid` 获取** | **沿用库 `pcs.RapidUpload`**：协议实现不重造（我们在 `TmpFile`/`CreateSuperFile` 上已同策略复用）；秒传命中即直接完成 |
| 2 | **WAL 粒度** | **每文件一个 `audit` scope + 逐片 state record**：真正复用 `audit.Service.Resume` + `recovery.Coordinator`；恢复期只重建状态、不发请求 |
| 3 | **默认策略** | **`auto`**：内核优先；失败一次 ⇒ 本进程降级 `legacy`（现有库编排）+ **WARN + 指标**（不静默；避免新代码阻塞生产上传） |
| 4 | **大小文件分流** | **≥4MiB 走分片（rapid + 分片 + merge）；<4MiB 走 `superfile2/upload` 单请求**（省掉 rapid/merge 与逐片 WAL 开销） |

### 由此确定的实现约束
- 内核必须**自持 HTTP 客户端**（不经库的 client）⇒ 顺带绕开「库在发 HTTP 前做会话/uk 前置检查」这一 L2 障碍（见 §6）；
- `uploadid` / `RapidUpload` 仍经库调用 ⇒ **L2 里这一段的锁定通过「内核接口注入」完成**（内核不认识库，注入 fake `rapidProvider`）；
- 降级路径必须可观测：`upload_core_fallback_total{reason}` + WARN 日志，且 `legacy` 分支仅由开关进入。

## 附：多分片护栏与 ETag 语义（2026-11-08 追加）

真实验收发现：内核在**多分片**（≥2 块，需 merge）上**内容正确**（回读逐字节一致），但百度给的
**ETag 既非整文件 md5 也非块列表 md5** ⇒ 内核 `Verify` 的「ETag == 整文件 md5」判据对多块文件误报，
秒传自愈也改不了 ETag 语义。

因此保留一道尺寸护栏 `coreMaxVerifiedSize = uploadBlockSize (4MiB)`：超限时 `auto` 走既有路径、
`nucleus` 显式报错。**护栏原因是校验语义，不是内容缺陷**；要让内核覆盖多分片，需先把校验改为
回读内容比对（或对齐 ETag 语义）——详见 `docs/external-dependencies.md` §7.8.3（含实测三值）。
