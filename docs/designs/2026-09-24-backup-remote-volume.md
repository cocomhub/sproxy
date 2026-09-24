# 备份到远端卷（11.5-⑧）设计

## 背景 / 目标
- 现状（federated.go 实证）：联邦卷 FS 已具写面（`WithWriter(sync.FS)` 注入，装配层经 `remote.Client.FS(ref)` 可达远端 hub 卷），但无备份编排。
- 目标：通用备份引擎（源任意 sync.FS → 目标任意 sync.FS），以 federated FS 为远端目标；全量/增量（manifest 比较）+ 每文件校验 + 报告。

## 组件与接口
- 新包 `pkg/backup/`（纯标准库）：
  - `type Options struct { Concurrent int; Verify bool; MaxRetries int; Exclude []string }`（默认 Concurrent=4）。
  - `type Report struct { Files, Bytes, Skipped, Failed int; Errors []FileError; Truncated bool }`。
  - `Run(ctx, src sync.FS, dst sync.FS, opts Options) (*Report, error)`：泛化于 sync.FS（本地→本地可测；远端 = `federated.New(r).WithWriter(remote.Client.FS(ref))`）。
- 装配层（cmd/sproxy/backup.go）：源 = 本地卷 sync.FS；目标 = federated 写面。
- 入口：`POST /api/backup`（body: `{target: hub 节点/ref, subdir?, incremental: true}`）+ sclient `backup <target> [--incremental]`。
- Manifest：目标卷 `<backup>/manifest.json`（rel → {size, mtime, checksum}），原子写（tmp+Rename，对齐 DedupStore.save 模式）。

## 数据流
walk 源（跳过规则同 dup 扫描）→ stat → manifest 比对（size+mtime 相同 → skip）→ `OpenRead` → `dst.WriteFile(path, r, size, mtime)` → Verify 时对端 Stat 校验 size → 更新 manifest → 报告。worker 池并发，每文件错误隔离。

## 错误处理
- 目标未注入写面（ErrReadOnly）→ fail-fast「备份目标只读，需装配远端写面」。
- 单文件失败：重试 MaxRetries（指数退避 100ms×2ⁿ）→ 记 Failed + FileError 继续。
- 部分失败：CLI 退出码非 0；服务端 200 + report（业务完成、部分失败语义显式化）。
- manifest 损坏：Warn + 全量重扫（不中断备份）。

## 测试 + 变异点
1. 全量：源 5 文件 → 目标 5 文件 + manifest 落盘（变异：不写 manifest → 红）。
2. 增量：再跑 → Skipped=5（变异：mtime 比较漏 → 红）。
3. 单文件写失败 → Failed=1 + 其余成功 + 重试次数断言（变异：不重试 → 红）。
4. Verify 校验 size 不符 → Failed（变异：不校验 → 红）。
5. ctx 取消 → 返回已拷贝部分 + Truncated（变异：忽略 ctx → 红）。
6. 并发峰值 == Concurrent（atomic counter；变异：去信号量 → 红）。
7. E2E：mock 远端（httptest hub 模拟）→ backup 成功 + 对端文件可读。

## 片划分
- P1：pkg/backup 引擎 + manifest + 单测。
- P2：路由/CLI 接线 + federated 写面装配 + E2E。

## 风险与零回归保证
- federated.go 不改（只复用 WithWriter 契约）；新增独立包，既有卷代码零改动。
- 大目录内存：walk 惰性流式；manifest 单文件原子写。
- 硬链/去重：按逻辑文件备份（非 inode），与去重物理层无关（文档说明备份为内容级）。
