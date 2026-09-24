# sclient 并发批量 + 进度条（11.5-⑨）设计

## 背景 / 目标
- 现状（batch.go 实证）：仅含结果类型与打印（batchOperationResult / printBatchResults / countBatchSuccess）；批量命令逐行串行执行，大清单耗时线性。
- 目标：`--workers N` 并发执行（默认 4）+ `--progress` TTY 实时进度条（非 TTY 自动关闭）；结果输出保持输入顺序（与串行输出逐字节一致 = 脚本兼容零回归）；SIGINT 优雅收尾。

## 组件与接口
- 新文件 `cmd/sclient/batch_run.go`：
  - `runBatchConcurrent(ops []string, workers int, exec func(raw string) batchOperationResult) ([]batchOperationResult, error)`：exec 注入便于单测（不依赖真实上传/网络）。
  - 内部按 `Index` 槽位聚合保序；信号量（buffered chan）限并发。
  - `type ProgressSink interface { Report(Progress) }`；`Progress{Total, Done, Failed}`（stderr 进度条 / no-op 两种实现）。
- 现有接口不动：printBatchResults / countBatchSuccess 原样复用（终态打印，按 Index 升序）。
- flag：`--workers`（int，默认 4；0/1 = 串行退化）、`--progress`（bool，默认 false，显式开启才画条）。

## 数据流
读 batch 文件 → 解析 ops（现有解析/引号语义不动）→ worker 池 → 每 op 调 exec → 结果入 ordered slot → 聚合器每完成一个调 ProgressSink.Report → 全部完成后按 Index 排序 printBatchResults → 退出码（含任一 FAIL → 非 0，沿用现状）。

## 错误处理
- 单 op 失败：捕获进 batchOperationResult（现有语义），不中断其它 op。
- SIGINT：ctx cancel → 在飞 op 完成后退出，未开始 op 标记 Skipped（Message 说明），部分结果照常打印。
- workers > len(ops)：钳位。
- exec panic：recover → 该 op FAIL（防一个 op 拖垮整批）。

## 测试 + 变异点
1. 30 ops 全执行、结果与输入同序（变异：无序输出 → 红）。
2. 并发峰值 ≤ workers（atomic counter；变异：删信号量 → 红）。
3. 失败 op 不阻塞其余（变异：失败即 abort → 红）。
4. Progress 回调 Done/Failed 与终态一致（变异：Done 少计 → 红）。
5. workers=1 输出与串行一致（零回归基线）。
6. SIGINT 后未执行 op 标记 Skipped（变异：继续执行 → 红）。
7. E2E：mock 服务器 + 20 行 batch 文件 → 全 OK；`--workers 1` 输出逐字节等于串行。

## 片划分
- P1：runBatchConcurrent + 保序 + 信号量 + 单测。
- P2：--workers/--progress 接线 + 进度条渲染（TTY 检测）+ E2E。

## 风险与零回归保证
- 默认并发改变执行时序 → 结果输出保序兜底；严格串行可 `--workers 1`。
- 进度条只写 stderr，stdout 终态输出格式不变（脚本兼容）。
- batch 解析（引号/注释）完全不动。
