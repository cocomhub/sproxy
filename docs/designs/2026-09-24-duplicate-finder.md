# 重复文件发现（11.5-⑦）设计

## 背景 / 目标
- 现状（dedup.go 实证）：DedupStore 台账（checksum → refs 列表，per-tenant `dedup.json`）已维护内容引用，但无「全仓重复报告」入口；`dedup.enabled=false` 时台账为空，无法发现历史重复。
- 目标：两种模式的重复文件扫描报告：① 台账快照（instant，仅去重开启时）② 全量扫描（walk + SHA-256，权威，任意配置可用）。输出重复组 + 可回收空间（DuplicateBytes）。

## 组件与接口
- 新包 `pkg/files/dup_report.go`：
  - `type DupGroup struct { Checksum string; Size int64; Refs []DupRef }`；`DupRef{Volume, Rel, ModTime}`。
  - `type Report struct { Groups []DupGroup; ScannedFiles int; TotalBytes, DuplicateBytes int64; Truncated bool; Errors []ScanError }`。
  - `ReportFromLedger(ds *DedupStore) (*Report, error)`：台账快照模式（refs≥2 才输出）。
  - `ScanVolume(ctx, walkFn, opts ScanOptions) (*Report, error)`：遍历卷（跳过 symlink 与 meta/ 桶）→ 分块 `io.Copy(sha256)` → 分组。
- 服务端：`POST /api/dedup/report`（body: `{mode: ledger|scan, subdir?}`）→ 同步返回 Report JSON；扫描默认限量防拖垮。
- sclient：`dedup-report [--mode scan] [--subdir]`。
- 指标：`sproxy_dedup_scan_total{result}`（counter）+ `sproxy_dedup_duplicate_bytes`（gauge，报告后设置）。

## 数据流
scan：walk → 逐文件哈希 → 按 checksum 分组 → 过滤 refs<2 → `DuplicateBytes = Σ(size × (refs-1))` → Report。
ledger：DedupStore.entries 快照深拷贝 → 同上过滤/计算（秒级）。

## 错误处理
- 单文件读失败：记 `ScanError{Rel, Reason}` 继续（报告不因单文件失败中断）。
- 卷根不存在/权限不足：fail-fast 明确错误。
- ctx 取消/超时：返回已扫部分 + `Truncated=true`。
- 台账损坏：DedupStore 已降级空台账 → Report 空 + Warn（沿用现有语义）。

## 测试 + 变异点
1. 已知 3 组重复（2/3/5 份）→ 组数与 DuplicateBytes 精确断言（变异：savings 公式 refs-1 写成 refs → 红）。
2. scan 与 ledger 两模式同输入同输出（变异：漏过滤 refs<2 → 红）。
3. 含不可读文件 → Errors 记录且其余组完整（变异：panic/中断 → 红）。
4. symlink 不跟随（变异：跟随 → 红）。
5. ctx 取消 → Truncated=true（变异：忽略 ctx → 红）。
6. E2E：起真实服务，上传 2 个相同文件 → report 命中 1 组。

## 片划分
- P1：dup_report 纯逻辑（ledger + scan）+ 单测。
- P2：路由 + sclient 命令 + metrics + E2E。

## 风险与零回归保证
- 大仓扫描耗时/IO：subdir 可限 + 单请求限流，文档给出 cron 建议。
- 只读操作：不改文件不动台账；新路由/新命令，既有上传/删除路径零改动。
- 硬链接语义：scan 按内容哈希判定，硬链文件内容相同 → 视为重复，与去重语义一致（文档说明）。
