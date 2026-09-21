<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Benchmark 与 CI 稳定性经验（2026-09）

> 来源：`docs/archive/benchmark-ci-timeout-disk-io.md`（被 Makefile / 门禁硬引用，2026-09-20 自 learnings 移入），
> 保留原路径）与 `docs/audit/2026-09-18-benchmark-io-collapse.md`（已归档合并）。
> 本文是**浓缩版速查**；完整取证过程与修复细节见上述保留文档。

## 1. Benchmark 夹具不得把被测 payload 写进 runner 磁盘

- 症状：`pkg/client` benchmark 夹具把每个 payload `os.Create` 到 `b.TempDir()`（runner 系统盘），
  每趟 CI 写 ~2.5 GiB；runner 脏页回写带宽被挤到 ~0.15 MB/s 时，单次 1 MiB 上传 5ms → 6.5s
- 判据：**耗时与写入字节数成正比**（1 MiB→6.49s、4 MiB→29.38s，速率 140–180 KB/s），只读 benchmark 同窗口正常
- 修复：mock 只做「流式哈希 + 丢弃」+ `TestMockBenchUploadHandler_DoesNotPersistPayload` 钉住（夹具写盘即红）
- 机制：page cache → dirty 页累积 → 触 vm.dirty_ratio 后被限速到设备真实回写带宽

## 2. 三种塌陷形态与守卫

| 形态 | 特征 | 守卫 |
|------|------|------|
| ① 写盘塌陷 | 与字节数成正比、方差极小、只影响搬数据方向 | 夹具不落盘 + 测试钉住 |
| ② 慢但会返回 | 环境 I/O 塌陷，单次 op 恒定 >2s | `benchStallErr` 停滞守卫（单次 op 超 2s → b.Fatal + 重跑提示） |
| ③ 卡死不返回 | 无进展、无 panic 栈、job 级取消静默 | **进程外看门狗**（见下） |

## 3. `go test -timeout` 对 benchmark 无效（关键取证）

- 实验：`BenchmarkHang` 60s 睡眠 + `-timeout 5s` → **跑满 60s 后 PASS**；同样睡眠放测试里 → panic + FAIL
- 原因：alarm 在 `M.Run` 起止，benchmark 循环内不复位
- 结论：拦形态③必须**进程外看门狗**——`tools/benchwatch` 监视 stdout 静默窗口
  （startup 180s / limit 120s），越界判定卡死、终止 go test 及其后代、退出码 66、
  日志证据保全（CI artifact 改 `if: always()`）

## 4. 其它 CI 教训

- **`go test | tee` 管道退出码取自 tee**（POSIX sh 无 pipefail）→ go test 失败不使 job 变红；
  修复：把退出码写文件再读完 exit
- **benchmark 夹具与生产装配脱节**：手搓 Handlers 连 globalRoot/tenants 都没装 → 401/400 全被 job 绿遮住
  → 夹具改走 `RegisterRoutes` 生产装配 + 状态码断言
- **slogTracer.depth 只 ++ 不 --** → 每请求缩进 +2 空格，26MB job 日志 94% 是空白 → 按父 span 递推
- 后续形态④：2026-09-18 又现「单 op >2s 守卫已挡住的第三种」——I/O 塌陷多形态持续演进，
  处置原则仍是「取证的判据（耗时∝字节数 + 只读对照）可复用 + 守卫给可操作信息 + rerun」

## 5. 处置流程（I/O 塌陷时）

1. `gh run rerun <run-id> --failed`（单 op >2s 守卫已知会误报环境塌陷）
2. 判据复用：先看是否与字节数成正比、是否只影响搬数据方向、只读 benchmark 是否正常
3. 是环境塌陷 → rerun；连续复现 → 查夹具是否有磁盘副作用（`TestMockBenchUploadHandler_DoesNotPersistPayload`）
4. 卡死无输出 → 看 build/bench/output.txt 尾部（看门狗已保证证据保全）

## 6. 基准回归门禁（2026-09-21 新增）

- **工具**：`tools/benchgate`（进程内调用 `golang.org/x/perf/benchstat` 库，逐行判定
  Δ% 与显著性）。为什么不用 `benchstat` CLI：需要「噪声行 ~ 视为通过」与「退化超阈值
  即 exit 1」的可编程语义，shell 解析文本表不可靠。
- **基线**：`benchmarks/baseline/<GOOS>.txt`（git 跟踪；`make bench-baseline` 生成）。
  与 `build/benchmark/data/`（本地历史记录，git 忽略）区分：基线是门禁对比基准，
  仅当代表性环境（CI runner/CPU）变化时才重新生成并提交。
- **接线**：`make bench` 产出 `build/bench/output.txt` → CI Benchmark job 追加
  `make bench-gate`（默认阈值 `BENCH_GATE_THRESHOLD=0.15`）→ 退化超阈值 exit 1。
- **判据**：只统计「显著」（p<0.05）且 Δ 超阈值的退化；时间/内存类（ns/op、B/op、
  allocs/op）更大=差，速度类（MB/s）更小=差；改进不判失败；噪声行（~）不判。
- **验证**：`TestGate_*` 单测 + 人为把 BenchmarkUpload ns/op ×1.3 → benchgate exit 1；
  同数据改阈值 1% 亦红（双重佐证判定链路）。
