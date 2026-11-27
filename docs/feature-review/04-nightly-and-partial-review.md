# 04 · 近期合并代码对抗审查（第二批次，2026-11-27）

**范围**：`#1337`/`#1338`/`#1339`（nightly 基建）、`#1334`（块写 mtime）、`#1335`（cloud 日志/清理）、
`#1318`（失败断点处置与账本归还）。

**结论**：**2 项 P2**（均在 nightly 基建，已修）+ 3 项通过 + 2 条 P3 观察（不改，附理由）。

## 发现与处置

### F1（P2，已修）nightly MinIO 镜像跟随 `latest`

`#1338`（quay → elestio）与 `#1339`（再换镜像）连改两轮，根因都是**镜像引用不稳定**：

```yaml
elestio/minio:latest   # ← 上游推新 digest 就可能让 nightly 神秘失效
```

后果不是「跑挂了看得见」，而是**无法复现当时通过的组合**——nightly 的价值就是「同一组合
昨天绿今天红 ⇒ 一定是我们改坏了」。跟随 `latest` 把这条因果链直接破坏。

修：按 **digest** 固定（`elestio/minio@sha256:25348a25...`），并写清升级方式
（`docker buildx imagetools inspect <image>` 取新 digest）。同一 digest 也用于建桶的
`mc` 步骤（原来是同一个 image 被拉两次）。

### F2（P2，已修）nightly 失败无人可见

扫描式流程失败时没人盯 Actions 页面。GitHub 对 scheduled workflow 失败只给
「最后修改 cron 的人」发邮件 ⇒ 换人维护后 nightly 就成了**每天红但没人管**的背景噪声。

修：`if: failure()` 步骤经 `actions/github-script` 开/复用一个 `nightly` label 的 issue
（已有 OPEN 则追加评论），带运行链接与日志 artifact 指引；`permissions` 加 `issues: write`。

> 同类推广：任何「不做 gate 的周期性流程」都应自带失败出口，否则等于没有。

### P3 观察（不改）

1. **`#1318` `discardPartialOnFail` 的 `after = before` 分支**：`diskUsageOfTask <= ReservedSize`
   时把 `after` 回写成 `before`（差额 0）—— 语义正确但读起来像"覆盖测量值"。真正的护栏是
   `ReservedSize` 在锁内被写成 `after`（=0）⇒ 重复调用不会二次释放。可读性债，非缺陷。
2. **`#1334` 块写路径的 `applyUploadMTime`**：`remotePath` 与 `rel` 都传 `sess.dstRel`
   （前者只用于日志）⇒ 日志里看不到用户可见路径。低价值，未改（改动会让日志与 write_ops 分叉）。

## 通过项

| PR | 结论 |
|---|---|
| `#1318` | 判据从 `Downloaded` 计数改为**磁盘真实占用**（崩溃/续传时计数为 0 会误删可续断点）；`discardPartialOnFail` 先删后按实际收敛账本，`ReservedSize` 锁内清零 ⇒ 重复调用不二次释放；`clearPartialDiagnostics` 与「提示必须诚实」同源 ✅ |
| `#1334` | 块写关闭先 `applyUploadMTime` 再生成 sidecar（sidecar 的 MTime 取自文件当前时间），并把 mtime 收敛为 `applyUploadMTime` 单一实现（R8 门禁守护）✅ |
| `#1335` | `shortChecksumForLog` 修掉 `Checksum[:16]` 越界 panic；`cleanupTaskCloud` 两分支统一 `markDirty` 且不再传调用方旧指针 ✅ |
| `#1337` | 跨存储 meta nightly 用真实 local+S3（非 mock），断言两侧 meta 等价；新增 Bearer 支持覆盖 `api_keys` 部署形态 ✅ |
